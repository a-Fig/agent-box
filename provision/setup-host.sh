#!/usr/bin/env bash
# setup-host.sh - run as root on the machine that hosts the boxes: an Ubuntu or Debian server, or
# the `agents` WSL distro on a Windows PC (install-pc.ps1 runs it there with --tailscale).
# Installs Incus (Zabbly stable) + web UI, applies the preseed, creates the restricted `agents`
# project with its own default profile, that profile and the `cache` volume, and the reaper;
# optionally Tailscale, boxd and `agent-base`. Ends by printing the line a client runs to connect.
# Needs Linux 6.12+ (Incus 7.x's minimum). Safe to re-run: each step checks before it changes anything.
#
# Environment (each overrides a default; on WSL the defaults are the PC's, elsewhere they are
# sized from nproc, RAM and free disk):
#   TS_AUTHKEY        Tailscale auth key, with --tailscale. Read from the environment only; never
#                     written to disk or logs. Without it Tailscale is installed but not joined.
#   AGENTS_CPU_CAP    project cap on the sum of limits.cpu       (WSL 24; else 2 x nproc)
#   AGENTS_MEM_CAP    limits.memory, set once on a new project    (WSL 12GiB, then the tray's; else 3/4 of RAM)
#   AGENTS_MAX_BOXES  project limits.instances                   (WSL 10; else RAM cap / 2GiB, at most 10)
#   AGENTS_DISK_CAP   project limits.disk, all boxes + volumes   (WSL 210GiB; else 18GiB a box + 30GiB)
#   AGENTS_POOL_SIZE  btrfs pool size, on a fresh Incus only     (WSL 200GiB; else 80% of free disk, at most 200GiB)
# Flags:
#   --tailscale       install Tailscale and join it with $TS_AUTHKEY, then the tailnet certificate
#                     and boxd. A host already on a tailnet gets these steps without the flag.
#   --remote NAME     the client's name for this host (default pc, which box uses without BOX_REMOTE)
#   --host ADDR       the address clients reach this host at (default its tailnet name, else its main IP)
#   --new-client      make a client token even though a restricted client is already trusted
#   --build-base      build the `agent-base` image if it does not exist
#   --rebuild-base    build it again even if it exists
set -Eeuo pipefail
# incus create/edit commands read YAML from stdin when it isn't a terminal; over SSH or a pipe
# they would hang waiting for it. Nothing here needs stdin.
exec </dev/null

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ZABBLY_FPR="4EFC590696CB15B87C73A3AD82CC8797C838DCFD"
PROJECT=agents
CACHE_SIZE=30GiB
BUILD_BASE=0
REBUILD=0
TAILSCALE=0
REMOTE=pc
HOST_ADDR=
NEW_CLIENT=0

while [ $# -gt 0 ]; do
  case "$1" in
    --new-client) NEW_CLIENT=1 ;;
    --build-base) BUILD_BASE=1 ;;
    --rebuild-base) BUILD_BASE=1; REBUILD=1 ;;
    --tailscale) TAILSCALE=1 ;;
    --remote) [ $# -ge 2 ] || { echo "--remote needs a name" >&2; exit 2; }; REMOTE=$2; shift ;;
    --host) [ $# -ge 2 ] || { echo "--host needs an address" >&2; exit 2; }; HOST_ADDR=$2; shift ;;
    -h|--help) sed -n '2,25p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done
case "$REMOTE" in ''|*[!A-Za-z0-9_.-]*) echo "--remote: use letters, digits, '.', '_' and '-' only" >&2; exit 2 ;; esac

step() { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
# ERR, not EXIT: only an ERR trap sees the failing line in $LINENO.
trap 'printf "ERROR: setup-host.sh failed (exit %s) at line %s\n" "$?" "$LINENO" >&2' ERR

[ "$(id -u)" -eq 0 ] || die "run as root (sudo, or on WSL: wsl -d agents -u root ...)"
[ -r /etc/os-release ] || die "no /etc/os-release"
# shellcheck disable=SC1091
. /etc/os-release
case "${ID:-}" in
  ubuntu|debian) ;;
  *) die "expected Ubuntu or Debian, found ${ID:-unknown}" ;;
esac
# WSL's kernel names itself (6.x-microsoft-standard-WSL2). Only there are the GPU folders, the
# lxcfs workaround, the PC's fixed caps and the tray.
WSL=0
if grep -qi microsoft /proc/sys/kernel/osrelease 2>/dev/null; then WSL=1; fi

# Incus 7.x (Zabbly stable) documents Linux 6.12 as its minimum: 7.0 raised it from 5.15 and
# dropped cgroup v1, and containers fail to start on older kernels (no core scheduling). So the
# check holds on every host; only the way to a newer kernel differs.
kernel=$(uname -r)
kmajor=${kernel%%.*}; krest=${kernel#*.}; kminor=${krest%%[^0-9]*}
if [ "$kmajor" -lt 6 ] || { [ "$kmajor" -eq 6 ] && [ "${kminor:-0}" -lt 12 ]; }; then
  if [ "$WSL" -eq 1 ]; then
    die "WSL kernel is $kernel; Incus 7.x needs Linux 6.12 or newer. On Windows run 'wsl --update' (install-pc.ps1 does), then 'wsl --shutdown', then run this again."
  fi
  die "kernel is $kernel; Incus 7.x needs Linux 6.12 or newer. Ubuntu 24.04: apt install linux-generic-hwe-24.04, then reboot. Debian 12: a kernel from bookworm-backports. Debian 13 ships 6.12."
fi
for f in incus-preseed.yaml default-profile.yaml agent-base.yaml reaper.sh https-cert.sh update-boxd.sh systemd; do
  [ -r "$HERE/$f" ] || die "missing $HERE/$f"
done

# The profile (default-profile.yaml) puts boxes on pool `default` and network `incusbr0`. An Incus
# that has them in another shape is refused, not configured around: only btrfs enforces the box
# and cache sizes, and restricted.devices.nic=managed needs a managed network.
check_pool_and_bridge() {
  local drv net
  drv=$(incus storage show default | sed -n 's/^driver: *//p')
  [ "$drv" = btrfs ] || die "storage pool 'default' is ${drv:-of an unknown driver}, not btrfs, which boxes need (only it enforces disk sizes). Use a host without a 'default' pool, or empty and delete that pool."
  net=$(incus network show incusbr0 2>/dev/null) \
    || die "storage pool 'default' exists but network 'incusbr0' doesn't. Create it (incus network create incusbr0) or use a fresh host."
  { printf '%s\n' "$net" | grep -qx 'managed: true' && printf '%s\n' "$net" | grep -qx 'type: bridge'; } \
    || die "network 'incusbr0' isn't a managed bridge, which boxes need."
}
# Before anything changes: an Incus that is already here is checked first. One with pools but
# none named `default` is refused too: the preseed would rewrite its default profile and
# core.https_address under whatever already runs there.
if command -v incus >/dev/null 2>&1 && incus storage list -f csv >/dev/null 2>&1; then
  if incus storage show default >/dev/null 2>&1; then
    check_pool_and_bridge
  elif [ -n "$(incus storage list -f csv)" ]; then
    die "Incus here already has storage pools, none named 'default'. Setup would change its default profile and HTTPS address, so it stops. Use a host without Incus, or one whose 'default' pool is btrfs."
  fi
fi

# Overrides are checked before they are used: whole numbers, and sizes like 12GiB.
for v in AGENTS_CPU_CAP AGENTS_MAX_BOXES; do
  case "${!v:-0}" in *[!0-9]*) die "$v must be a whole number, not '${!v}'" ;; esac
done
for v in AGENTS_MEM_CAP AGENTS_DISK_CAP AGENTS_POOL_SIZE; do
  [ -z "${!v:-}" ] || [[ ${!v} =~ ^[0-9]+GiB$ ]] || die "$v must be a size in GiB, like 12GiB, not '${!v}'"
done

# Caps. On WSL they stay the PC's (docs/DESIGN.md, Box sizes): the tray's memory budgets
# (internal/mode/size.go) are fixed to them. Elsewhere there is no tray, so the mode stays Shared
# and the caps follow the machine. A box counts against limits.cpu at its Full CPUs (medium: 4)
# but runs on half that in Shared, so 2 x nproc lets boxes fill the CPUs. 3/4 of RAM leaves the
# rest to the host. A small box is 2GiB and 18GiB of disk, and the cache volume 30GiB.
if [ "$WSL" -eq 1 ]; then
  d_cpu=24; d_mem=12GiB; d_boxes=10; d_disk=210GiB; d_pool=200GiB
else
  mem_gib=$(awk '/^MemTotal:/ { printf "%d", $2 * 3 / 4 / 1048576 }' /proc/meminfo)
  [ "$mem_gib" -ge 2 ] || mem_gib=2
  d_boxes=$(( mem_gib / 2 )); [ "$d_boxes" -le 10 ] || d_boxes=10
  d_cpu=$(( $(nproc) * 2 )); [ "$d_cpu" -ge 4 ] || d_cpu=4
  d_mem=${mem_gib}GiB
  free_gib=$(df -BG --output=avail /var/lib | awk 'NR==2 { sub("G", ""); print int($1) }')
  pool_gib=$(( free_gib * 8 / 10 )); [ "$pool_gib" -le 200 ] || pool_gib=200
  d_pool=${pool_gib}GiB
  # The disk cap never exceeds the pool, so a full pool is a clear quota refusal, not ENOSPC.
  pool_cap=${AGENTS_POOL_SIZE:-$d_pool}; pool_cap=${pool_cap%GiB}
  # At least 60, the cache and one medium box (see the pool floor below), even with one box.
  disk_gib=$(( ${AGENTS_MAX_BOXES:-$d_boxes} * 18 + 30 )); [ "$disk_gib" -ge 60 ] || disk_gib=60
  [ "$disk_gib" -le "$pool_cap" ] || disk_gib=$pool_cap
  d_disk=${disk_gib}GiB
fi
CPU_CAP=${AGENTS_CPU_CAP:-$d_cpu}
MEM_CAP=${AGENTS_MEM_CAP:-$d_mem}
MAX_BOXES=${AGENTS_MAX_BOXES:-$d_boxes}
DISK_CAP=${AGENTS_DISK_CAP:-$d_disk}
POOL_SIZE=${AGENTS_POOL_SIZE:-$d_pool}

# Tailscale is opt-in, but a host already on a tailnet keeps its steps. On WSL (the PC, rerun by
# hand) an installed Tailscale is enough: just after a WSL boot tailscaled may not say Running yet.
ts_state() { tailscale status --json 2>/dev/null | sed -n 's/.*"BackendState": *"\([^"]*\)".*/\1/p' | head -n1 || true; }
if [ "$TAILSCALE" -eq 0 ] && command -v tailscale >/dev/null 2>&1; then
  if [ "$WSL" -eq 1 ] || [ "$(ts_state)" = Running ]; then TAILSCALE=1; fi
fi

export DEBIAN_FRONTEND=noninteractive

step "Checking systemd"
state=$(systemctl is-system-running 2>/dev/null || true)
case "$state" in
  running|degraded) note "systemd is $state" ;;
  *)
    [ "$WSL" -eq 1 ] && die "systemd is '$state'. Is systemd=true in /etc/wsl.conf and has the distro been restarted (wsl --terminate agents)?"
    die "systemd is '$state'; Incus and the timers here need systemd running."
    ;;
esac

step "Installing prerequisites (curl, gnupg, ca-certificates, btrfs-progs, python3)"
apt-get update -qq
# python3 reads Tailscale's and Incus's JSON (here and in https-cert.sh); minimal Debian lacks it.
apt-get install -y -qq curl gnupg ca-certificates btrfs-progs python3

# ---------------------------------------------------------------- Incus
step "Adding the Zabbly Incus stable repository"
install -d -m 0755 /etc/apt/keyrings
tmpkey=$(mktemp)
curl -fsSL https://pkgs.zabbly.com/key.asc -o "$tmpkey"
got=$(gpg --show-keys --with-colons "$tmpkey" | awk -F: '$1=="fpr"{print $10; exit}')
[ "$got" = "$ZABBLY_FPR" ] || die "Zabbly key fingerprint mismatch: got '$got', expected '$ZABBLY_FPR'. Not trusting it."
install -m 0644 "$tmpkey" /etc/apt/keyrings/zabbly.asc
rm -f "$tmpkey"
cat > /etc/apt/sources.list.d/zabbly-incus-stable.sources <<EOF
Enabled: yes
Types: deb
URIs: https://pkgs.zabbly.com/incus/stable
Suites: ${VERSION_CODENAME}
Components: main
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/zabbly.asc
EOF
note "repo written, key fingerprint verified"

step "Installing incus and incus-ui-canonical"
apt-get update -qq
apt-get install -y -qq incus incus-ui-canonical
systemctl enable --now incus.service incus.socket >/dev/null 2>&1 || systemctl enable --now incus
incus version

step "Loading btrfs (the storage pool needs it)"
modprobe btrfs
echo btrfs > /etc/modules-load.d/agent-box-btrfs.conf
note "btrfs loaded, and loaded at every boot"

step "lxcfs (boxes see their own memory and CPUs in /proc)"
if [ "$WSL" -eq 1 ]; then
  # systemd in WSL counts as a container, and incus-lxcfs.service skips containers
  # (ConditionVirtualization=!container). Without lxcfs, a box's /proc/meminfo shows the whole VM,
  # so tools that size themselves from it (Gradle, JVMs, test runners) overshoot their cgroup limit.
  mkdir -p /etc/systemd/system/incus-lxcfs.service.d
  printf '[Unit]\nConditionVirtualization=\n' > /etc/systemd/system/incus-lxcfs.service.d/wsl.conf
  systemctl daemon-reload
fi
systemctl enable --now incus-lxcfs >/dev/null 2>&1 || true
note "incus-lxcfs: $(systemctl is-active incus-lxcfs || true)"

step "Initialising Incus (preseed)"
if incus storage show default >/dev/null 2>&1; then
  note "storage pool 'default' exists: already initialised, skipping preseed"
else
  # Incus may have been stopped at the first check; now that it runs, refuse other pools here too.
  [ -z "$(incus storage list -f csv)" ] || die "Incus here already has storage pools, none named 'default'. Setup would change its default profile and HTTPS address, so it stops. Use a host without Incus, or one whose 'default' pool is btrfs."
  pool_n=${POOL_SIZE%GiB}
  case "$pool_n" in ''|*[!0-9]*) pool_n=0 ;; esac
  # 60: the 30GiB cache and one medium box (30GiB root), the default size and the size
  # --build-base's builder launches at. With 50 (2026-11-25, a 60GB VM) both hit limits.disk.
  [ "$pool_n" -ge 60 ] || die "pool size '$POOL_SIZE' isn't 60GiB or more, in GiB (the 30GiB cache volume and one medium box, 30GiB). It is 80% of the free disk under /var/lib: free about 75GiB there, or set AGENTS_POOL_SIZE=<n>GiB (no more than is free)."
  sed "s/^\( *size:\) 200GiB$/\1 $POOL_SIZE/" "$HERE/incus-preseed.yaml" | incus admin init --preseed
  note "applied incus-preseed.yaml (btrfs pool of $POOL_SIZE, incusbr0 NAT bridge, HTTPS on :8443)"
fi
check_pool_and_bridge
addr=$(incus config get core.https_address)
[ -n "$addr" ] || { incus config set core.https_address "[::]:8443"; addr="[::]:8443"; }
note "HTTPS API address: $addr"

# ---------------------------------------------------------------- project
step "Project '$PROJECT'"
# features.profiles=true: the project has its own profiles. Incus refuses restricted=true on a
# project without them ("Projects without their own profiles cannot be restricted"). Agents' certs
# can edit these profiles, but restricted=true still validates what goes in them, the project caps
# bound the totals, and the tray resets each box's own CPU limits every tick.
# features.images=false: images are shared with the default project.
# Neither can change after creation, so an existing project is only checked.
if incus project show "$PROJECT" >/dev/null 2>&1; then
  note "project exists; features.* cannot change after creation, only checking"
  bad=0
  for kv in features.images=false features.profiles=true; do
    k=${kv%%=*}; v=${kv#*=}
    cur=$(incus project get "$PROJECT" "$k")
    if [ "$cur" != "$v" ]; then
      printf 'ERROR: project %s has %s=%s, expected %s.\n' "$PROJECT" "$k" "${cur:-<unset>}" "$v" >&2
      bad=1
    fi
  done
  if [ "$bad" -eq 1 ]; then
    cat >&2 <<EOF

Incus cannot change features.* on an existing project, so it has to be recreated. This deletes
every box in it and the shared cache volume (push anything you need from /work first):
  incus list --project $PROJECT -c n --format csv | xargs -r -n1 incus delete --force --project $PROJECT
  incus storage volume delete default cache --project $PROJECT
  incus project delete $PROJECT
Then run setup-host.sh again.
EOF
    exit 1
  fi
else
  incus project create "$PROJECT" \
    -c features.images=false -c features.profiles=true \
    -c user.agent-box.mode=shared
  note "created (own profiles; images shared with the default project)"
fi
# The mode: the tray writes it on the PC; with no tray it is Shared for good, and box launches
# each box with Shared's CPUs (internal/jobrun/launch.go). Written only when missing, so the
# tray's value stands.
if [ -z "$(incus project get "$PROJECT" user.agent-box.mode)" ]; then
  incus project set "$PROJECT" user.agent-box.mode=shared
fi
note "mode: $(incus project get "$PROJECT" user.agent-box.mode) (with no tray it stays Shared)"
# Allocation caps (docs/DESIGN.md, Box sizes). Box sizes (internal/mode/size.go) set each box's
# memory and root disk at launch and its limits.cpu per mode. limits.memory is set only on a new
# project: on the PC the tray owns it from then on (24GiB in Full, 12GiB otherwise, never below
# what boxes already hold), and setting it again could fail (Incus won't lower it below what is
# in use) or fight the tray. On the PC, 24GiB fits 12 small boxes, so 10 instances is the binding
# cap; 10 small root disks and the 30GiB cache allocate 210GiB, the disk cap.
incus project set "$PROJECT" limits.instances="$MAX_BOXES"
incus project set "$PROJECT" limits.cpu="$CPU_CAP"
# A failed read prints nothing too; only a read that worked and came back empty means unset.
MEM_NOW=$(incus project get "$PROJECT" limits.memory) || die "could not read limits.memory of project $PROJECT"
if [ -z "$MEM_NOW" ]; then
  incus project set "$PROJECT" limits.memory="$MEM_CAP"
  MEM_NOW=$MEM_CAP
fi
# limits.disk: "maximum value of the aggregate disk space used by all instance volumes, custom
# volumes, and images of the project" (project config reference, key limits.disk).
incus project set "$PROJECT" limits.disk="$DISK_CAP"
note "caps: instances=$MAX_BOXES cpu=$CPU_CAP memory=$MEM_NOW (set once) disk=$DISK_CAP (allocation caps, not usage caps)"
# Restricted project. Keys per https://linuxcontainers.org/incus/docs/main/reference/projects/
#   restricted=true                                turns the restricted.* keys on
#   restricted.containers.privilege=unprivileged   blocks security.privileged=true (the default)
#   restricted.containers.nesting=allow            boxes need security.nesting=true (default: block)
#   restricted.devices.nic=managed                 only NICs on a managed network: incusbr0 (default)
# On WSL only, for the GPU box (GPUDevices in internal/mode/size.go):
#   restricted.devices.disk=allow                  pool-backed disks (root, /cache) as under `managed`,
#   restricted.devices.disk.paths=...              plus host paths under these prefixes only: the
#                                                  WSL GPU folders
#   restricted.devices.unix-char=allow             /dev/dxg (default: block)
# Elsewhere those two are `managed` and `block`, their defaults: there is no /dev/dxg, and a box
# gets no host paths or character devices.
# The GPU keys, checked against the reference and Incus's source on 2026-11-24
# (internal/server/project/permissions.go, internal/server/device/disk.go): disk.paths applies
# only with disk=allow and only to disks without pool=, so pool volumes pass as under `managed`.
# It matches whole path components of `source`, opens the path with openat2 so a symlink can't
# lead outside it, and refuses shift=true. No key enforces `readonly`, but boxes are unprivileged
# and the files belong to host root, so a box can't write them. unix-char is allow or block for
# every character device; there is no per-path key, so this lets an agent's cert attach any host
# character device to a box, not only /dev/dxg (docs/DESIGN.md, Isolation).
# No restricted.* key stops a restricted client cert from creating a custom storage volume: the
# reference has none for volumes (restricted.devices.disk only limits which disk DEVICES an
# instance may have; it still lets it attach any pool volume). So the real limit is
# limits.disk above, which counts custom volumes too. Agents should not store data in a volume,
# and the reaper only deletes instances, so volumes agents create are bounded, not removed.
incus project set "$PROJECT" restricted=true \
  restricted.containers.privilege=unprivileged \
  restricted.containers.nesting=allow \
  restricted.devices.nic=managed
if [ "$WSL" -eq 1 ]; then
  incus project set "$PROJECT" \
    restricted.devices.disk=allow \
    restricted.devices.disk.paths=/usr/lib/wsl/lib,/usr/lib/wsl/drivers \
    restricted.devices.unix-char=allow
  note "restricted: unprivileged containers, nesting allowed, managed NICs, pool disks plus the two WSL GPU folders, unix-char devices (for /dev/dxg)"
  if [ -e /dev/dxg ] && [ -d /usr/lib/wsl/lib ] && [ -d /usr/lib/wsl/drivers ]; then
    note "GPU: /dev/dxg and the WSL GPU folders are present, so gpu boxes can start"
  else
    note "GPU: /dev/dxg or /usr/lib/wsl/{lib,drivers} is missing, so gpu boxes won't start (needs the NVIDIA driver on Windows)"
  fi
else
  incus project set "$PROJECT" restricted.devices.disk=managed restricted.devices.unix-char=block
  note "restricted: unprivileged containers, nesting allowed, managed NICs, pool disks only"
  note "GPU: gpu boxes need WSL's /dev/dxg, so they won't start on this host"
fi

step "Volume 'cache' (shared /cache for Go, npm, Gradle, Android SDK, Robolectric, uv)"
if incus storage volume show default cache --project "$PROJECT" >/dev/null 2>&1; then
  note "exists"
else
  incus storage volume create default cache --project "$PROJECT" size="$CACHE_SIZE"
  note "created, $CACHE_SIZE"
fi
# Idempotent: an existing volume gets the quota too.
incus storage volume set default cache size="$CACHE_SIZE" --project "$PROJECT"
note "cache volume size=$CACHE_SIZE (a cache: it may be wiped at any time)"

step "Default profile of the '$PROJECT' project"
incus profile edit default --project "$PROJECT" < "$HERE/default-profile.yaml"
note "applied default-profile.yaml (medium-size defaults: limits.cpu=2, limits.memory=4GiB, 30GiB root; nesting, /cache)"

# ---------------------------------------------------------------- Tailscale
step "Tailscale"
if [ "$TAILSCALE" -eq 0 ]; then
  note "skipped (opt-in: --tailscale). box works without it; the dashboard and the queue need it."
else
  if ! command -v tailscale >/dev/null 2>&1; then
    install -d -m 0755 /usr/share/keyrings
    curl -fsSL "https://pkgs.tailscale.com/stable/${ID}/${VERSION_CODENAME}.noarmor.gpg" \
      -o /usr/share/keyrings/tailscale-archive-keyring.gpg
    curl -fsSL "https://pkgs.tailscale.com/stable/${ID}/${VERSION_CODENAME}.tailscale-keyring.list" \
      -o /etc/apt/sources.list.d/tailscale.list
    apt-get update -qq
    apt-get install -y -qq tailscale
    note "installed"
  else
    note "already installed"
  fi
  systemctl enable --now tailscaled >/dev/null 2>&1

  if [ "$(ts_state)" = Running ]; then
    note "already joined the tailnet; leaving it alone"
  elif [ -n "${TS_AUTHKEY:-}" ]; then
    note "joining the tailnet as 'agent-box' (auth key read from \$TS_AUTHKEY, not stored)"
    # --accept-dns=false: keep the host's resolv.conf; boxes use the incusbr0 resolver.
    tailscale up --hostname agent-box --accept-dns=false --authkey "$TS_AUTHKEY"
    unset TS_AUTHKEY
  else
    note "TS_AUTHKEY not set: NOT joined. Later: sudo tailscale up --hostname agent-box --accept-dns=false"
    note "(or re-run this script with TS_AUTHKEY set)"
  fi
fi

# Warm an image in the btrfs pool's images/<fp> cache so later launches copy from it quickly.
# On Incus 7.6 (2026-11-24, btrfs + GHSA-5pgx-q9xx-j3qv), a concurrent `incus config set`
# (e.g. from boxtray's 5s tick in Full, or in Shared for instances launched without Shared's
# limits.cpu/limits.cpu.priority keys) while EnsureImage is still unpacking the tarball runs
# UpdateInstanceBackupFile -> btrfs MountVolume -> EnsureMountPath, creating
# containers/<project>_<name> as a plain directory before `btrfs subvolume snapshot` runs.
# btrfs then nests the snapshot inside that directory (<name>/<fp>/rootfs), which
# stripInstanceMetadata rejects with "Instance metadata contained disallowed entries" and leaves
# the empty plain directory behind.
# Warm in the default project: `agents` has features.images=false so both share storage pool
# `default` and its images/<fp> cache, while avoiding `agents`'s caps and boxtray's ticks.
# Still absorb that exact error once and rmdir any leftover plain dir in case of a race.
warm_image() {
  local img=$1 name=$2 dir="/var/lib/incus/storage-pools/default/containers/$2" out
  incus delete --force "$name" --project default >/dev/null 2>&1 || true
  rmdir "$dir" 2>/dev/null || true
  if ! out=$(incus init "$img" "$name" --project default 2>&1); then
    if [[ $out == *"Instance metadata contained disallowed entries"* ]]; then
      note "absorbed Incus 7.6 btrfs first-launch metadata race while warming $img; retrying once"
      incus delete --force "$name" --project default >/dev/null 2>&1 || true
      rmdir "$dir" 2>/dev/null || true
      if ! out=$(incus init "$img" "$name" --project default 2>&1); then
        out=${out##*$'\n'}
        if (( ${#out} > 400 )); then out="${out:0:400}..."; fi
        printf '%s\n' "$out" >&2
        return 1
      fi
    else
      printf '%s\n' "$out" >&2
      return 1
    fi
  fi
  incus delete --force "$name" --project default >/dev/null || return 1
  rmdir "$dir" 2>/dev/null || true
}

# ---------------------------------------------------------------- agent-base
if [ "$BUILD_BASE" -eq 1 ]; then
  step "Building image 'agent-base'"
  if incus image alias list --format csv | cut -d, -f1 | grep -qx agent-base && [ "$REBUILD" -eq 0 ]; then
    note "alias exists; pass --rebuild-base to build again"
    warm_image agent-base agent-base-warm \
      || note "warning: couldn't warm agent-base; the first box launch will unpack it"
  else
    builder=agent-base-build
    incus delete --force "$builder" --project "$PROJECT" >/dev/null 2>&1 || true
    rmdir "/var/lib/incus/storage-pools/default/containers/${PROJECT}_${builder}" 2>/dev/null || true
    warm_image images:ubuntu/24.04/cloud agent-base-build-warm \
      || note "warning: couldn't warm base Ubuntu image; continuing"
    incus launch images:ubuntu/24.04/cloud "$builder" --project "$PROJECT" < "$HERE/agent-base.yaml"
    note "waiting for cloud-init (installs Go, Node, JDK, ...); this takes a few minutes"
    until incus exec "$builder" --project "$PROJECT" -- true >/dev/null 2>&1; do sleep 2; done
    incus exec "$builder" --project "$PROJECT" -- cloud-init status --wait --long \
      || die "cloud-init failed in $builder; see: incus exec $builder --project $PROJECT -- cat /var/log/agent-box-provision.log"
    # A box is thrown away, so nothing in it updates itself or runs idle housekeeping: apt's
    # timers and unattended-upgrades hold the dpkg lock and change packages mid-job. Masked, not
    # just disabled, so no package postinst turns them back on. Kept: networkd, resolved, journald,
    # logind, udevd, dbus, cron. The image is updated by rebuilding it.
    incus exec "$builder" --project "$PROJECT" -- systemctl mask --now \
      unattended-upgrades.service apt-daily.timer apt-daily.service \
      apt-daily-upgrade.timer apt-daily-upgrade.service motd-news.timer ua-timer.timer \
      dpkg-db-backup.timer e2scrub_all.timer fstrim.timer \
      systemd-sysupdate.timer systemd-sysupdate-reboot.timer rsyslog.service polkit.service </dev/null
    # Toolchains, profile.d and netplan (/etc/netplan/50-cloud-init.yaml) are baked into the image
    # now. Scrub the builder's per-instance identity (truncate /etc/machine-id so systemd generates
    # a fresh one per box and DHCPv4 DUIDs don't collide; remove any SSH host keys) while preserving
    # netplan, then disable cloud-init so launches skip ssh-keygen, apt_configure and runcmd and
    # don't delay systemd-networkd (#194).
    incus exec "$builder" --project "$PROJECT" -- bash -c '
      rm -f /etc/machine-id /etc/ssh/ssh_host_*
      : > /etc/machine-id
      [ -L /var/lib/dbus/machine-id ] || rm -f /var/lib/dbus/machine-id
      touch /etc/cloud/cloud-init.disabled
    ' </dev/null
    incus config unset "$builder" cloud-init.user-data --project "$PROJECT"
    incus stop "$builder" --project "$PROJECT"
    incus publish "$builder" --project "$PROJECT" --alias agent-base --reuse \
      description="agent-box base: git, build-essential, Go, Node LTS, uv, JDK 17, Android SDK (lazy), gh, Chromium libraries"
    incus delete "$builder" --project "$PROJECT"
    if warm_image agent-base agent-base-warm; then
      note "published and warmed 'agent-base' (images are shared via the default project)"
    else
      note "published 'agent-base'; warning: couldn't warm it, so the first box launch will unpack it"
    fi
  fi
else
  step "Image 'agent-base'"
  if incus image alias list --format csv | cut -d, -f1 | grep -qx agent-base; then
    note "exists (--rebuild-base builds it again)"
  else
    note "not built. box launches it, so runs fail with 'Image \"agent-base\" not found' until it is. Build with: setup-host.sh --build-base"
  fi
fi

# ---------------------------------------------------------------- VM capability
step "Reaper (deletes expired boxes every minute, with or without the tray)"
install -m 0755 "$HERE/reaper.sh" /usr/local/sbin/agent-box-reaper
install -m 0644 "$HERE/systemd/agent-box-reaper.service" "$HERE/systemd/agent-box-reaper.timer" /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now agent-box-reaper.timer >/dev/null 2>&1
note "agent-box-reaper.timer enabled"

step "HTTPS certificate (Let's Encrypt through Tailscale, renewed weekly)"
if [ "$TAILSCALE" -eq 0 ]; then
  note "skipped (needs --tailscale): Incus keeps its self-signed certificate, which the client's token pins"
else
  install -m 0755 "$HERE/https-cert.sh" /usr/local/sbin/agent-box-https-cert
  install -m 0644 "$HERE/systemd/agent-box-https-cert.service" "$HERE/systemd/agent-box-https-cert.timer" /etc/systemd/system/
  systemctl daemon-reload
  systemctl enable --now agent-box-https-cert.timer >/dev/null 2>&1
  if /usr/local/sbin/agent-box-https-cert; then
    note "Incus serves a trusted certificate; renewed weekly"
  else
    note "no certificate yet (Tailscale not joined, or HTTPS certificates off in the tailnet's DNS settings); the timer retries weekly"
  fi
fi

step "Dashboard server (boxd, docs/ui.md)"
# boxd trusts only Tailscale's login header (internal/boxd/auth.go), so it comes with --tailscale.
# boxd is copied in by install-pc.ps1 -BoxdPath, which empties this folder first, so a boxd here
# is one this run staged. It is used once and deleted: on 2026-11-25 a rerun of this script by
# hand found the copy staged at the first install still here and put it over the newer boxd that
# deploy-boxd.sh had installed. It never downgrades either: a staged boxd older (by mtime) than
# the installed one is skipped. deploy-boxd.sh is how boxd is updated after setup.
# The mtime check gates only the binary: the unit (when new or missing), boxd.json and tailscale
# serve are applied whenever a boxd is installed, so a rerun repairs a setup that stopped
# halfway. To put an older build in on purpose, use deploy-boxd.sh, or delete the installed one.
if [ "$TAILSCALE" -eq 0 ]; then
  note "skipped (needs --tailscale and a boxd staged beside this script)"
else
  boxd_new=no
  if [ -f "$HERE/boxd" ]; then
    # The same file counts as new, so a rerun after a failure below installs and restarts it.
    if [ -f /opt/agent-box/bin/boxd ] && ! cmp -s "$HERE/boxd" /opt/agent-box/bin/boxd && [ "$HERE/boxd" -ot /opt/agent-box/bin/boxd ]; then
      note "the staged boxd is older than the installed one: kept the installed one (deploy-boxd.sh updates boxd)"
    else
      install -D -m 0755 "$HERE/boxd" /opt/agent-box/bin/boxd
      boxd_new=yes
    fi
  fi
  if [ -f /opt/agent-box/bin/boxd ]; then
    # A unit deploy-boxd.sh installed is kept unless this run brings a new boxd with it.
    if [ "$boxd_new" = yes ] || [ ! -f /etc/systemd/system/agent-box-boxd.service ]; then
      install -m 0644 "$HERE/systemd/agent-box-boxd.service" /etc/systemd/system/
    fi
    install -d -m 0755 /etc/agent-box
    # The owner is the tailnet login this node belongs to. It is read here, never written into
    # the repo, which is public. A tagged node has no login: the list stays empty and boxd
    # refuses everyone until it is filled in by hand.
    if [ ! -s /etc/agent-box/boxd.json ]; then
      owner=$(tailscale status --json 2>/dev/null | python3 -c 'import json, sys
d = json.load(sys.stdin); u = d.get("User", {}).get(str(d["Self"].get("UserID", "")), {})
print(u.get("LoginName", "") if not d["Self"].get("Tags") else "")' 2>/dev/null || true)
      if [ -n "$owner" ]; then owners="[\"$owner\"]"; else owners="[]"; fi
      printf '{"owners": %s, "listen": "127.0.0.1:7766"}\n' "$owners" > /etc/agent-box/boxd.json
      chmod 0644 /etc/agent-box/boxd.json
      [ -n "$owner" ] || note "WARNING: no tailnet login found; add owners to /etc/agent-box/boxd.json by hand"
    fi
    systemctl daemon-reload
    systemctl enable agent-box-boxd.service >/dev/null 2>&1
    if [ "$boxd_new" = yes ]; then
      systemctl restart agent-box-boxd.service
    else
      systemctl start agent-box-boxd.service # a no-op when it runs
    fi
    note "boxd: $(systemctl is-active agent-box-boxd.service || true), owners in /etc/agent-box/boxd.json"
    if tailscale status >/dev/null 2>&1; then
      # It runs on every rerun now, so a failure (HTTPS certificates off in the tailnet's DNS
      # settings) is a note rather than the end of setup; timeout in case it waits for a consent.
      if timeout 30 tailscale serve --bg --https=443 http://127.0.0.1:7766 >/dev/null </dev/null; then
        note "tailscale serve: https://<this node>.<tailnet>.ts.net/ -> 127.0.0.1:7766 (tailnet only)"
      else
        note "tailscale serve failed (HTTPS certificates off in the tailnet's DNS settings?): run 'tailscale serve --bg --https=443 http://127.0.0.1:7766' once they are on"
      fi
    else
      note "Tailscale is not up: run 'tailscale serve --bg --https=443 http://127.0.0.1:7766' once it is"
    fi
  else
    note "not installed (stage a Linux boxd beside this script; install-pc.ps1 -BoxdPath does)"
  fi

  step "boxd updates (the latest stable release, checked hourly; docs/releasing.md)"
  # After the boxd step: the timer runs at once when started, and must see the boxd this run installed.
  install -m 0755 "$HERE/update-boxd.sh" /usr/local/sbin/agent-box-update-boxd
  install -m 0644 "$HERE/systemd/agent-box-update.service" "$HERE/systemd/agent-box-update.timer" /etc/systemd/system/
  systemctl daemon-reload
  systemctl enable --now agent-box-update.timer >/dev/null 2>&1
  note "agent-box-update.timer enabled; a boxd that isn't a release is left alone (AGENT_BOX_NO_UPDATE=1 in /etc/agent-box/update.env turns it off)"
fi
# Only now, with the step done (set -e stops the script on a failure above, keeping it for a
# rerun), is the staged copy deleted.
rm -f "$HERE/boxd"

step "Virtual machines (/dev/kvm)"
if [ -e /dev/kvm ]; then
  note "/dev/kvm present: Incus VMs (incus launch --vm) are possible."
elif [ "$WSL" -eq 1 ]; then
  note "/dev/kvm missing: only containers. VMs would need nested virtualization"
  note "(.wslconfig [wsl2] nestedVirtualization=true on Windows 11, then wsl --shutdown)."
else
  note "/dev/kvm missing: only containers (boxes are containers, so that is enough)."
fi

# ---------------------------------------------------------------- client
step "Done. Connect a client"
# A restricted client cert, limited to the agents project, as box expects. A token still waiting
# for its client is shown again rather than a new one made, so a rerun leaves one pending. Once a
# restricted client is trusted (on the PC, the laptop's), none is made without --new-client:
# tokens don't expire unless core.remote_token_expiry is set, and an unused one is a way in.
trust_name="box-$REMOTE"
token=$(incus config trust list-tokens --format csv | awk -F, -v n="$trust_name" '$1 == n { print $2; exit }')
clients=$(incus query '/1.0/certificates?recursion=1' | python3 -c 'import json, sys
print(sum(1 for c in json.load(sys.stdin) if c.get("type") == "client" and c.get("restricted") and sys.argv[1] in (c.get("projects") or [])))' "$PROJECT")
if [ -z "$token" ] && { [ "$clients" -eq 0 ] || [ "$NEW_CLIENT" -eq 1 ]; }; then
  token=$(incus config trust add "$trust_name" --restricted --projects "$PROJECT" | tail -n1)
fi
if [ -z "$HOST_ADDR" ]; then
  if [ "$TAILSCALE" -eq 1 ]; then
    # The tailnet name: the certificate https-cert.sh installs is for it.
    HOST_ADDR=$(tailscale status --json 2>/dev/null | python3 -c 'import json, sys; print(json.load(sys.stdin)["Self"]["DNSName"].rstrip("."))' 2>/dev/null || true)
  fi
  # The source address of the default route: the one other machines on the network reach.
  [ -n "$HOST_ADDR" ] || HOST_ADDR=$(ip -o route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([^ ]*\).*/\1/p')
  [ -n "$HOST_ADDR" ] || HOST_ADDR='<this host>'
fi
port=${addr##*:}
# `incus remote add <url> --token` asks to confirm a certificate the client doesn't trust (a
# self-signed one; not the tailnet's Let's Encrypt one) and takes the fingerprint itself as the
# answer, refusing any other (cmd/incus/remote.go, read 2026-11-25). So the fingerprint goes in on
# stdin: one line, no prompt, and still checked. The bare-token form checks it too, but uses the
# addresses in the token and gives up when the first one fails.
fpr=$(incus info | sed -n 's/^ *certificate_fingerprint: *//p' | head -n1)
[ -n "$fpr" ] || die "could not read this host's certificate fingerprint (incus info)"
if [ -z "$token" ]; then
  note "$clients restricted client(s) already trusted for '$PROJECT'; rerun with --new-client for another."
else
  cat <<EOF

  On the client, with the Incus client installed, run once:

    echo $fpr | incus remote add $REMOTE https://$HOST_ADDR:$port --token $token --project $PROJECT

  The echo answers the client's certificate check with this host's fingerprint. The token is
  single-use and doesn't expire; revoke an unused one with: incus config trust revoke-token $trust_name
  For another client, rerun with --new-client (--host if the address is wrong).
EOF
fi
[ "$REMOTE" = pc ] || printf '  box uses the remote named pc unless BOX_REMOTE=%s is set for it.\n' "$REMOTE"
if [ "$WSL" -eq 1 ]; then
  cat <<'EOF'

  Tray on the PC (admin). It must use a DIFFERENT client cert than agents:
    incus config trust add boxtray
    #   on the PC, with its own config dir (e.g. INCUS_CONF=%LOCALAPPDATA%\agent-box\incus):
    #   incus remote add pc https://localhost:8443 --token <token>
EOF
else
  echo
  note "No tray here, so the mode stays Shared: boxes always get Shared's CPUs."
  note "Incus listens on $addr, every interface; only trusted client certificates get in."
fi
cat <<'EOF'

  Web UI: https://<host>:8443 (import a client cert into the browser; see the Incus UI docs)
  List / revoke:  incus config trust list ;  incus config trust remove <fingerprint>

EOF
trap - EXIT
