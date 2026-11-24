#!/bin/sh
# Deletes boxes in the agents project whose user.expires (RFC3339 UTC, set by `box`) has passed.
# A systemd timer runs it every minute inside the distro, so expired boxes go even when nobody is
# logged in to Windows to run the tray (the tray reaps too; deleting twice is harmless).
set -eu
PROJECT=${AGENTS_PROJECT:-agents}
now=$(date -u +%s)
incus list --project "$PROJECT" -f csv -c n,user.expires </dev/null |
while IFS=, read -r name expires; do
  [ -n "$expires" ] || continue                        # no expiry: leave it
  t=$(date -u -d "$expires" +%s 2>/dev/null) || { echo "skip $name: bad user.expires '$expires'"; continue; }
  if [ "$t" -lt "$now" ]; then
    echo "deleting $name (expired $expires)"
    incus delete --force --project "$PROJECT" "$name" </dev/null || echo "delete $name failed; next run retries"
  fi
done
