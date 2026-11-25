#!/usr/bin/env bash
# install-boxd.sh: install a boxd build and its unit in the agents distro, as
# root, from the folder this script is in. deploy-boxd.sh sends that folder
# and runs this; it isn't meant to be run by hand, but it can be.
#
# The folder holds boxd, agent-box-boxd.service and SHA256SUMS. Nothing is
# changed unless every checksum matches. The running boxd and unit are kept
# as boxd.old and agent-box-boxd.service.old. If the restarted service isn't
# up and answering after SETTLE seconds, both are put back and the old boxd is
# restarted, and this exits non-zero.
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
[ -f boxd ] && [ -f agent-box-boxd.service ] && [ -f SHA256SUMS ] || fail "boxd, agent-box-boxd.service or SHA256SUMS missing in $HERE"
sha256sum --quiet -c SHA256SUMS || fail "checksum mismatch: nothing was changed"
say "checksums OK"

# The service is healthy when systemd says active, it hasn't restarted itself
# while we waited, and something answers HTTP on boxd's address (any status:
# the owner check refuses a request with no tailnet login, which still shows
# boxd is serving). NRestarts is compared, not checked for 0: systemctl
# restart doesn't reset it (seen 2026-11-25, after a rollback).
listen=$(sed -n 's/.*"listen"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' /etc/agent-box/boxd.json 2>/dev/null | head -n1)
listen=${listen:-127.0.0.1:7766}
restarts() { systemctl show -p NRestarts --value "$SVC"; }
healthy() {
  local n0
  n0=$(restarts)
  sleep "$SETTLE"
  [ "$(systemctl is-active "$SVC" || true)" = active ] || return 1
  [ "$(restarts)" = "$n0" ] || return 1
  code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "http://$listen/api/state" || true)
  [ -n "$code" ] && [ "$code" != 000 ]
}

had_old=no
if [ -f "$BIN/boxd" ]; then
  cp -p "$BIN/boxd" "$BIN/boxd.old"
  had_old=yes
fi
[ -f "$UNIT" ] && cp -p "$UNIT" "$UNIT.old"

install -d -m 0755 "$BIN"
install -m 0755 boxd "$BIN/boxd.new"
mv -f "$BIN/boxd.new" "$BIN/boxd" # a rename: the running boxd keeps its file until it stops
install -m 0644 agent-box-boxd.service "$UNIT"
systemctl daemon-reload
systemctl enable "$SVC" >/dev/null 2>&1
systemctl restart "$SVC"

if healthy; then
  say "boxd $(sha256sum "$BIN/boxd" | cut -c1-12) is active and answering on $listen"
  journalctl -u "$SVC" -n 20 --no-pager -o cat --since "-$((SETTLE + 5))s" | grep -i 'warning' || true
  exit 0
fi

printf 'install-boxd: the new boxd is not healthy; its log:\n' >&2
journalctl -u "$SVC" -n 20 --no-pager -o cat >&2 || true
if [ "$had_old" = yes ]; then
  mv -f "$BIN/boxd.old" "$BIN/boxd"
  [ -f "$UNIT.old" ] && mv -f "$UNIT.old" "$UNIT"
  systemctl daemon-reload
  systemctl restart "$SVC"
  if healthy; then
    fail "rolled back to the previous boxd, which is active again"
  fi
  fail "rolled back, but the previous boxd isn't healthy either: look at journalctl -u $SVC"
fi
fail "no previous boxd to roll back to"
