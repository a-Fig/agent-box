#!/bin/sh
# Use the host policy when the installed boxd supports it. The timestamp
# policy keeps ordinary idle boxes expiring during installation and rollback.
set -eu
boxd=${AGENT_BOX_BOXD:-/opt/agent-box/bin/boxd}
if [ -x "$boxd" ] && "$boxd" -help 2>&1 | grep -q '^  -reap$'; then
    exec "$boxd" -reap -project "${AGENTS_PROJECT:-agents}" "$@"
fi

exec python3 - "${AGENTS_PROJECT:-agents}" "$@" <<'PY'
import argparse
from datetime import datetime, timezone
import json
import re
import subprocess
import sys
from urllib.parse import quote, urlencode

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


def read_json(*command):
    return json.loads(subprocess.check_output(["incus", *command], stdin=subprocess.DEVNULL))


try:
    boxes = read_json("list", "--project", args.project, "-f", "json")
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


def expired(name, config):
    expires = config.get("user.expires", "")
    if expires == "":
        return False
    expiry = deadline(expires)
    if expiry is None:
        print(f"skip {name}: bad user.expires {expires!r}")
        return False
    hold = config.get("user.box.hold-until", "")
    held_until = deadline(hold)
    if hold != "" and held_until is None:
        print(f"ignoring {name}: bad user.box.hold-until {hold!r}")
    return max(expiry, held_until or expiry) < now


project = urlencode({"project": args.project})
for box in boxes:
    name = box["name"]
    if not expired(name, box["config"]):
        continue
    # A setup or hold can land after the list. Fetch both fields together
    # before the name-bound delete.
    try:
        config = read_json("query", f"/1.0/instances/{quote(name, safe='')}?{project}")["config"]
        if not isinstance(config, dict):
            raise ValueError("expected instance config")
    except (OSError, subprocess.CalledProcessError, ValueError, TypeError, KeyError) as err:
        print(f"skip {name}: could not recheck expiry: {err}")
        continue
    if not expired(name, config):
        continue
    action = "would delete" if args.dry_run else "deleting"
    print(f"{action} {name} (expired {config['user.expires']})", flush=True)
    if not args.dry_run and subprocess.call(
        ["incus", "delete", "--force", "--project", args.project, "--", name],
        stdin=subprocess.DEVNULL,
    ):
        print(f"delete {name} failed; next run retries")
PY
