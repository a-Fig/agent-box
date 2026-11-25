#!/usr/bin/env bash
# deploy-boxd.sh: build boxd from this checkout and deploy it, with its
# systemd unit, to the PC's agents distro in one go. Run it from the laptop
# (Git Bash), at the repo root or anywhere in it:
#
#   provision/deploy-boxd.sh user@pc-host
#
# The target can also come from AGENT_BOX_PC. It needs box (BOX names another
# box.exe), ssh and scp, and key-based SSH to the PC (bootstrap-ssh.ps1),
# whose shell is PowerShell.
#
# Why a script: boxd and its unit were copied by hand, and on 2026-11-25 the
# PC's unit was found older than provision/systemd's (no INCUS_CONF), so every
# job scan had been failing. This sends both together from one commit.
#
# How: the build, the unit, install-boxd.sh and their SHA256SUMS go to the PC
# as one tar, with a .cmd that pipes it into the distro. The distro has no
# /mnt/c automount (wsl.conf), and cmd keeps a binary stdin intact where
# PowerShell doesn't. install-boxd.sh, in the distro, checks the sums, keeps
# the running boxd as boxd.old, restarts the service and rolls back if the
# new one isn't healthy. This script exits non-zero if anything fails.
set -euo pipefail

PC=${1:-${AGENT_BOX_PC:-}}
DISTRO=${AGENT_BOX_DISTRO:-agents}
[ -n "$PC" ] || { echo "usage: $0 user@pc-host   (or set AGENT_BOX_PC)" >&2; exit 2; }
[[ "$DISTRO" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "bad distro name $DISTRO" >&2; exit 2; }

ROOT=$(git -C "$(dirname "$0")" rev-parse --show-toplevel)
cd "$ROOT"
commit=$(git rev-parse --short HEAD)
dirty="" modified=false
if ! git diff --quiet HEAD -- cmd internal provision go.mod go.sum ||
  [ -n "$(git ls-files --others --exclude-standard -- cmd internal provision go.mod go.sum)" ]; then
  dirty=" (with uncommitted changes)" modified=true
fi
# The page shows boxd's commit (internal/version). Go stamps it from git too,
# but counts any untracked file, anywhere, as a change; this looks only at
# what goes into boxd.
v=github.com/a-Fig/agent-box/internal/version
ldflags="-X $v.Commit=$(git rev-parse HEAD) -X $v.Time=$(git log -1 --format=%cI HEAD) -X $v.Modified=$modified"

STAGE=$(mktemp -d "${TMPDIR:-/tmp}/boxd-deploy.XXXXXX")
trap 'rm -rf "$STAGE"' EXIT
echo "== building boxd at $commit$dirty, on a box"
# On a box, not the laptop (#80): box run sends this checkout, uncommitted
# changes included, and box pull fetches the build. The repo's own box stays
# after the run, so the pull finds it whichever box.exe runs this.
BOX=${BOX:-box}
job=$("$BOX" run --reason "deploy-boxd.sh: build boxd at $commit to deploy it to the PC" -- \
  "mkdir -p .deploy && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags '$ldflags' -o .deploy/boxd ./cmd/boxd" 2>&1 |
  tee /dev/stderr | sed -n 's/^box: job \([^ ]*\) on .*/\1/p')
[ -n "$job" ] || { echo "== the build on a box didn't start" >&2; exit 1; }
"$BOX" pull "$job" .deploy/boxd "$STAGE"
[ -s "$STAGE/boxd" ] || { echo "== box pull brought back no boxd from $job" >&2; exit 1; }
# The scripts and unit go as LF, whatever the checkout did to them.
tr -d '\r' < provision/systemd/agent-box-boxd.service > "$STAGE/agent-box-boxd.service"
tr -d '\r' < provision/install-boxd.sh > "$STAGE/install-boxd.sh"
(cd "$STAGE" && sha256sum boxd agent-box-boxd.service install-boxd.sh > SHA256SUMS)
run=boxd-deploy-$(date +%Y%m%d-%H%M%S)-$$
tar -cf "$STAGE/$run.tar" -C "$STAGE" boxd agent-box-boxd.service install-boxd.sh SHA256SUMS
echo "   boxd $(cut -c1-12 "$STAGE/SHA256SUMS" | head -n1)"

# The .cmd runs in the SSH session's home folder on the PC, where scp put the
# tar. Each run has its own names there and in the distro, so two deploys at
# once don't delete each other's files; install-boxd.sh lets one install run.
dest=/opt/agent-box-provision/$run
printf '%s\r\n' \
  '@echo off' \
  "wsl.exe -d $DISTRO -u root --exec sh -c \"rm -rf $dest && mkdir -p $dest && tar -xf - -C $dest\" < $run.tar || exit /b 1" \
  "wsl.exe -d $DISTRO -u root --exec bash $dest/install-boxd.sh" \
  'set rc=%ERRORLEVEL%' \
  "wsl.exe -d $DISTRO -u root --exec rm -rf $dest" \
  'exit /b %rc%' > "$STAGE/$run.cmd"

ssh_opts=(-o BatchMode=yes -o ConnectTimeout=15)
echo "== copying to $PC"
scp -q "${ssh_opts[@]}" "$STAGE/$run.tar" "$STAGE/$run.cmd" "$PC:"
echo "== installing in the $DISTRO distro"
rc=0
ssh "${ssh_opts[@]}" "$PC" "cmd.exe /c $run.cmd" </dev/null || rc=$?
# A batch file can't delete itself while it runs, so both go afterwards.
ssh "${ssh_opts[@]}" "$PC" "Remove-Item -ErrorAction SilentlyContinue $run.cmd, $run.tar" </dev/null || true
if [ "$rc" != 0 ]; then
  echo "== deploy failed (exit $rc); see above" >&2
  exit "$rc"
fi
echo "== deployed boxd $commit$dirty"
