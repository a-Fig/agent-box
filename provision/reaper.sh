#!/bin/sh
# The expiry pass runs independently of the tray and the boxd HTTP service.
set -eu
exec /opt/agent-box/bin/boxd -reap -project "${AGENTS_PROJECT:-agents}" "$@"
