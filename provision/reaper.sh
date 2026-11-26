#!/bin/sh
# The timer reaps expired boxes even when the tray is offline. A valid hold
# can keep a box past its job's expiry.
set -eu
exec python3 - "${AGENTS_PROJECT:-agents}" "$@" <<'PY'
import argparse
from datetime import datetime, timezone
import json
import re
import subprocess
import sys

parser = argparse.ArgumentParser()
parser.add_argument("project")
parser.add_argument("--dry-run", action="store_true")
args = parser.parse_args()

utc_format = re.compile(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?Z")


def deadline(value):
    if isinstance(value, str) and utc_format.fullmatch(value):
        try:
            return datetime.fromisoformat(value[:-1] + "+00:00")
        except ValueError:
            pass
    return None


try:
    boxes = json.loads(subprocess.check_output(
        ["incus", "list", "--project", args.project, "-f", "json"],
        stdin=subprocess.DEVNULL,
    ))
    if not isinstance(boxes, list) or any(
        not isinstance(box, dict)
        or not isinstance(box.get("name"), str)
        or not box["name"]
        or not isinstance(box.get("config"), dict)
        for box in boxes
    ):
        raise ValueError("expected instance records with name and config")
except (OSError, subprocess.CalledProcessError, ValueError) as err:
    sys.exit(f"reaper: could not read Incus instances: {err}")

now = datetime.now(timezone.utc).replace(microsecond=0)
for box in boxes:
    name, config = box["name"], box["config"]
    expires = config.get("user.expires", "")
    if expires == "":
        continue
    expiry = deadline(expires)
    if expiry is None:
        print(f"skip {name}: bad user.expires {expires!r}")
        continue
    hold = config.get("user.box.hold-until", "")
    held_until = deadline(hold)
    if hold != "" and held_until is None:
        print(f"ignoring {name}: bad user.box.hold-until {hold!r}")
    if max(expiry, held_until or expiry) >= now:
        continue
    action = "would delete" if args.dry_run else "deleting"
    print(f"{action} {name} (expired {expires})", flush=True)
    if not args.dry_run and subprocess.call(
        ["incus", "delete", "--force", "--project", args.project, "--", name],
        stdin=subprocess.DEVNULL,
    ):
        print(f"delete {name} failed; next run retries")
PY
