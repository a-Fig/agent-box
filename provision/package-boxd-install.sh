#!/bin/sh
# Bundle the reaper wrapper in the installer delivered by deploy and releases.
set -eu
here=$(dirname "$0")
head -n 1 "$here/install-boxd.sh"
printf "%s\n" 'write_packaged_reaper() {' "cat <<'AGENT_BOX_REAPER'"
cat "$here/reaper.sh"
printf "%s\n" 'AGENT_BOX_REAPER' '}'
tail -n +2 "$here/install-boxd.sh"
