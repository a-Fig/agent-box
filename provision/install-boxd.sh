#!/usr/bin/env bash
# install-boxd.sh: install a boxd build and its unit in the agents distro, as
# root, from the folder this script is in. deploy-boxd.sh sends that folder
# and runs this; it isn't meant to be run by hand, but it can be.
#
# The folder holds boxd, agent-box-boxd.service and SHA256SUMS. Nothing is
# changed unless every checksum matches. A running boxd that is healthy is kept
# as boxd.old, with its unit as agent-box-boxd.service.old; an unhealthy one is
# not, so a retry never replaces the last good build with a bad one. If the
# restarted service isn't healthy after SETTLE seconds, or this script is cut
# off once it has swapped files, the old ones are put back and restarted, and
# it exits non-zero. One install runs at a time.
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=/opt/agent-box/bin
UNIT=/etc/systemd/system/agent-box-boxd.service
SETTLE=${SETTLE:-8}
SVC=agent-box-boxd.service

say() { printf '  %s\n' "$*"; }
fail() { printf 'install-boxd: %s\n' "$*" >&2; exit 1; }

cd "$HERE"
[ "$(id -u)" = 0 ] || fail "run as root"
exec 9>/run/agent-box-deploy.lock
flock -n 9 || fail "another boxd install is running"
[ -f boxd ] && [ -f agent-box-boxd.service ] && [ -f SHA256SUMS ] || fail "boxd, agent-box-boxd.service or SHA256SUMS missing in $HERE"
sha256sum --quiet -c SHA256SUMS || fail "checksum mismatch: nothing was changed"
say "checksums OK"

# Healthy: systemd says active, the service hasn't restarted itself while we
# waited, something answers HTTP on boxd's address (any status: the owner
# check refuses a request with no tailnet login, which still shows boxd is
# serving), and boxd logged no WARNING since it started (such as a missing
# INCUS_CONF, which leaves it up but unable to scan). NRestarts is compared,
# not checked for 0: systemctl restart doesn't reset it (seen 2026-11-25 UTC,
# after a rollback).
listen=$(sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /etc/agent-box/boxd.json 2>/dev/null | head -n1)
listen=${listen:-127.0.0.1:7766}
restarts() { systemctl show -p NRestarts --value "$SVC"; }
answering() {
  local code
  code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://$listen/api/state" || true)
  [ -n "$code" ] && [ "$code" != 000 ]
}
healthy() { # $1: when the service was (re)started, in epoch seconds
  local n0
  n0=$(restarts)
  sleep "$SETTLE"
  [ "$(systemctl is-active "$SVC" || true)" = active ] || return 1
  [ "$(restarts)" = "$n0" ] || return 1
  answering || return 1
  if journalctl -u "$SVC" --since "@$1" --no-pager -o cat 2>/dev/null | grep 'boxd: WARNING'; then
    return 1
  fi
}
restart() { # restarts the service and prints when
  local t
  t=$(date +%s)
  systemctl daemon-reload >&2 || true
  systemctl enable "$SVC" >/dev/null 2>&1 || true
  systemctl restart "$SVC" >&2 || true
  echo "$t"
}

# Keep the running build only if it is good; otherwise the .old already there
# is the last good one.
had_old=no had_old_unit=no
if [ -f "$BIN/boxd" ] && [ "$(systemctl is-active "$SVC" || true)" = active ] && answering; then
  cp -p "$BIN/boxd" "$BIN/boxd.old"
  if [ -f "$UNIT" ]; then cp -p "$UNIT" "$UNIT.old"; else rm -f "$UNIT.old"; fi
  say "kept the running boxd as boxd.old"
elif [ -f "$BIN/boxd.old" ]; then
  say "the running boxd isn't healthy: keeping the boxd.old already there"
fi
[ -f "$BIN/boxd.old" ] && had_old=yes
[ -f "$UNIT.old" ] && had_old_unit=yes

rollback() {
  trap - ERR INT TERM HUP
  if [ "$had_old" = yes ]; then
    cp -p "$BIN/boxd.old" "$BIN/boxd.new" && mv -f "$BIN/boxd.new" "$BIN/boxd"
    if [ "$had_old_unit" = yes ]; then cp -p "$UNIT.old" "$UNIT"; fi
    t=$(restart)
    if healthy "$t"; then fail "rolled back to the previous boxd, which is healthy again"; fi
    fail "rolled back, but the previous boxd isn't healthy either: look at journalctl -u $SVC"
  fi
  systemctl disable --now "$SVC" >/dev/null 2>&1 || true
  fail "no previous boxd to roll back to: stopped and disabled $SVC"
}
trap 'printf "install-boxd: cut off; rolling back\n" >&2; rollback' ERR INT TERM HUP

install -d -m 0755 "$BIN"
install -m 0755 boxd "$BIN/boxd.new"
mv -f "$BIN/boxd.new" "$BIN/boxd" # a rename: the running boxd keeps its file until it stops
install -m 0644 agent-box-boxd.service "$UNIT"
t=$(restart)

if healthy "$t"; then
  trap - ERR INT TERM HUP
  say "boxd $(sha256sum "$BIN/boxd" | cut -c1-12) is active and answering on $listen"
  exit 0
fi
trap - ERR
printf 'install-boxd: the new boxd is not healthy; its log:\n' >&2
journalctl -u "$SVC" -n 20 --no-pager -o cat >&2 || true
rollback
