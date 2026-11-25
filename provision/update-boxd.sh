#!/usr/bin/env bash
# update-boxd.sh [TAG]: install a released boxd in the agents distro, as root.
# agent-box-update.timer runs it hourly with no TAG: it takes GitHub's latest
# release, which is never a prerelease, and only when that is newer than the
# installed boxd. With a TAG it installs that release, a prerelease or an
# older one included, as a move made by hand (docs/releasing.md).
#
# It downloads the release's boxd, unit and install-boxd.sh, checks them
# against the release's SHA256SUMS, runs the new boxd with -version and
# refuses it unless it says the tag, then hands the three files to that
# install-boxd.sh, which keeps the running boxd as boxd.old, health-checks
# the new one and rolls back.
#
# A boxd that isn't a release (deploy-boxd.sh's builds from a checkout) is
# left alone by the timer, as a dev build never updates itself in Murmur.
# AGENT_BOX_NO_UPDATE=1, in the environment or in /etc/agent-box/update.env,
# turns the timer off; a TAG given by hand still installs.
set -euo pipefail

REPO=${AGENT_BOX_REPO:-a-Fig/agent-box}
BOXD=/opt/agent-box/bin/boxd
TAG=${1:-}

say() { printf 'update-boxd: %s\n' "$*"; }
fail() { printf 'update-boxd: %s\n' "$*" >&2; exit 1; }
is_tag() { printf '%s' "$1" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; }

# newer A B: whether release A is newer than release B. X.Y.Z compares as
# numbers; with the same X.Y.Z, a release is newer than any prerelease of it,
# and prereleases compare as sort -V does (nightly.YYYYMMDD.N).
newer() {
  local a=${1#v} b=${2#v} ca cb
  ca=${a%%-*} cb=${b%%-*}
  if [ "$ca" != "$cb" ]; then
    [ "$(printf '%s\n%s\n' "$ca" "$cb" | sort -V | tail -n1)" = "$ca" ]
    return
  fi
  case "$a,$b" in
    *-*,*-*) [ "$a" != "$b" ] && [ "$(printf '%s\n%s\n' "$a" "$b" | sort -V | tail -n1)" = "$a" ] ;;
    *-*,*) return 1 ;;
    *,*-*) return 0 ;;
    *) return 1 ;;
  esac
}

[ "$(id -u)" = 0 ] || fail "run as root"
if [ -z "$TAG" ] && [ "${AGENT_BOX_NO_UPDATE:-0}" != 0 ]; then
  say "AGENT_BOX_NO_UPDATE is set: not updating"
  exit 0
fi

cur=
if [ -x "$BOXD" ]; then
  cur=$("$BOXD" -version 2>/dev/null | awk '{ print $2 }') || cur=
  cur=${cur:-unknown} # a boxd from before -version: a dev build
fi

if [ -z "$TAG" ]; then
  if [ -n "$cur" ] && ! is_tag "$cur"; then
    say "boxd is a dev build ($cur), not a release: not updating (update-boxd.sh TAG installs one)"
    exit 0
  fi
  code=0
  json=$(curl -fsSL -m 30 -H 'Accept: application/vnd.github+json' "https://api.github.com/repos/$REPO/releases/latest") || code=$?
  if [ "$code" = 22 ]; then
    say "no stable release yet: nothing to do"
    exit 0
  fi
  [ "$code" = 0 ] || fail "reading the latest release failed (curl exit $code)"
  TAG=$(printf '%s' "$json" | sed -n 's/^[[:space:]]*"tag_name":[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1)
  [ -n "$TAG" ] || fail "the latest release has no tag_name"
  if [ "$cur" = "$TAG" ]; then
    exit 0 # the timer's usual hour: quiet
  fi
  if [ -n "$cur" ] && newer "$cur" "$TAG"; then
    say "boxd $cur is newer than the latest release $TAG: keeping it"
    exit 0
  fi
fi
is_tag "$TAG" || fail "$TAG is not a release tag like v1.2.3"
if [ "$cur" = "$TAG" ]; then
  say "boxd is already $TAG"
  exit 0
fi

exec 9>/run/agent-box-update.lock
flock -n 9 || fail "another update is running"
DIR=$(mktemp -d /opt/agent-box-update.XXXXXX)
trap 'rm -rf "$DIR"' EXIT
cd "$DIR"
say "downloading $TAG (boxd now: ${cur:-none})"
base="https://github.com/$REPO/releases/download/$TAG"
for f in SHA256SUMS boxd-linux-amd64 agent-box-boxd.service install-boxd.sh; do
  curl -fsSL -m 300 -o "$f" "$base/$f" || fail "downloading $f from $TAG failed"
done
sums=$(grep -E '  (boxd-linux-amd64|agent-box-boxd\.service|install-boxd\.sh)$' SHA256SUMS || true)
[ "$(printf '%s\n' "$sums" | grep -c .)" = 3 ] || fail "$TAG's SHA256SUMS doesn't list boxd-linux-amd64, agent-box-boxd.service and install-boxd.sh"
printf '%s\n' "$sums" | sha256sum --quiet -c - || fail "checksum mismatch in $TAG: nothing was changed"
chmod 0755 boxd-linux-amd64
says=$(./boxd-linux-amd64 -version 2>/dev/null | awk '{ print $2 }') || says=
[ "$says" = "$TAG" ] || fail "$TAG's boxd says it is \"$says\", not $TAG: nothing was changed"

# install-boxd.sh's own folder layout: boxd, the unit and their sums.
mkdir install
mv boxd-linux-amd64 install/boxd
mv agent-box-boxd.service install-boxd.sh install/
(cd install && sha256sum boxd agent-box-boxd.service install-boxd.sh > SHA256SUMS)
say "installing boxd $TAG"
bash install/install-boxd.sh
say "boxd is $TAG"
