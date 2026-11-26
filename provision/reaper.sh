#!/bin/sh
# Deletes boxes in the agents project whose user.expires (RFC3339 UTC, set by `box`) has passed,
# and whose user.box.hold-until (a person's extend, if any) has passed too.
# A systemd timer runs it every minute inside the distro, so expired boxes go even when nobody is
# logged in to Windows to run the tray (the tray reaps too; deleting twice is harmless).
set -eu
PROJECT=${AGENTS_PROJECT:-agents}
now=$(date -u +%s)
# Incus's csv wraps a field holding a comma or quote in double quotes; the values here are plain
# RFC3339 UTC, so strip them. Anything that isn't exactly that form is refused, as the Go side
# does (mode.Held), so a malformed hold never counts as a future time.
unq() { printf '%s' "$1" | sed 's/^"//; s/"$//'; }
PAT='^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z$'
epoch() { printf '%s' "$1" | grep -Eq "$PAT" && date -u -d "$1" +%s 2>/dev/null; }
incus list --project "$PROJECT" -f csv -c n,user.expires,user.box.hold-until </dev/null |
while IFS=, read -r name expires hold; do
  expires=$(unq "$expires"); hold=$(unq "$hold")
  [ -n "$expires" ] || continue                        # no expiry: leave it
  t=$(epoch "$expires") || { echo "skip $name: bad user.expires '$expires'"; continue; }
  if [ -n "$hold" ]; then
    if h=$(epoch "$hold"); then
      [ "$h" -gt "$t" ] && t=$h                        # held: the later of the two
    else
      echo "ignoring $name: bad user.box.hold-until '$hold'"
    fi
  fi
  if [ "$t" -lt "$now" ]; then
    echo "deleting $name (expired $expires${hold:+, hold $hold})"
    incus delete --force --project "$PROJECT" "$name" </dev/null || echo "delete $name failed; next run retries"
  fi
done
