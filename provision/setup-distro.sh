#!/usr/bin/env bash
# setup-distro.sh - run as root INSIDE the `agents` WSL distro (install-pc.ps1 does this).
# Installs Incus (Zabbly stable) + web UI, applies the preseed, creates the `agents` project,
# its default profile and `cache` volume, installs Tailscale, optionally builds `agent-base`.
# Safe to re-run: each step checks before it changes anything.
#
# Environment:
#   TS_AUTHKEY        Tailscale auth key. Read from the environment only; never written to disk or logs.
#                     Without it Tailscale is installed but not joined.
#   AGENTS_CPU_CAP    project cap on the sum of limits.cpu     (default 12)
#   AGENTS_MEM_CAP    project cap on the sum of limits.memory  (default 12GiB)
#   AGENTS_MAX_BOXES  project limits.instances                 (default 2)
# Flags:
#   --build-base      build the `agent-base` image if it does not exist
#   --rebuild-base    build it again even if it exists
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ZABBLY_FPR="4EFC590696CB15B87C73A3AD82CC8797C838DCFD"
PROJECT=agents
CPU_CAP=${AGENTS_CPU_CAP:-12}
MEM_CAP=${AGENTS_MEM_CAP:-12GiB}
MAX_BOXES=${AGENTS_MAX_BOXES:-2}
BUILD_BASE=0
REBUILD=0

for a in "$@"; do
  case "$a" in
    --build-base) BUILD_BASE=1 ;;
    --rebuild-base) BUILD_BASE=1; REBUILD=1 ;;
    -h|--help) sed -n '2,20p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $a" >&2; exit 2 ;;
  esac
done

step() { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
trap 'rc=$?; [ $rc -ne 0 ] && printf "ERROR: setup-distro.sh failed (exit %s) near line %s\n" "$rc" "$LINENO" >&2' EXIT

[ "$(id -u)" -eq 0 ] || die "run as root (wsl -d agents -u root ...)"
[ -r /etc/os-release ] || die "no /etc/os-release"
# shellcheck disable=SC1091
. /etc/os-release
[ "${ID:-}" = ubuntu ] || die "expected Ubuntu, found ${ID:-unknown}"
for f in incus-preseed.yaml agents-profile.yaml; do
  [ -r "$HERE/$f" ] || die "missing $HERE/$f"
done

export DEBIAN_FRONTEND=noninteractive

step "Checking systemd"
state=$(systemctl is-system-running 2>/dev/null || true)
case "$state" in
  running|degraded) note "systemd is $state" ;;
  *) die "systemd is '$state'. Is systemd=true in /etc/wsl.conf and has the distro been restarted (wsl --terminate agents)?" ;;
esac

step "Installing prerequisites (curl, gnupg, ca-certificates)"
apt-get update -qq
apt-get install -y -qq curl gnupg ca-certificates

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

step "Initialising Incus (preseed)"
if incus storage show default >/dev/null 2>&1; then
  note "storage pool 'default' exists: already initialised, skipping preseed"
else
  incus admin init --preseed < "$HERE/incus-preseed.yaml"
  note "applied incus-preseed.yaml (dir pool, incusbr0 NAT bridge, HTTPS on :8443)"
fi
addr=$(incus config get core.https_address)
[ -n "$addr" ] || { incus config set core.https_address "[::]:8443"; addr="[::]:8443"; }
note "HTTPS API address: $addr"

# ---------------------------------------------------------------- project
step "Project '$PROJECT'"
if incus project show "$PROJECT" >/dev/null 2>&1; then
  note "project exists; features.* cannot change after creation, only checking"
  for kv in features.images=false features.profiles=true; do
    k=${kv%%=*}; v=${kv#*=}
    cur=$(incus project get "$PROJECT" "$k")
    [ "$cur" = "$v" ] || note "WARNING: $k is '$cur', expected '$v' (recreate the project if this matters)"
  done
else
  incus project create "$PROJECT" \
    -c features.images=false -c features.profiles=true \
    -c user.agent-box.mode=shared
  note "created (images shared with the default project, own profiles)"
fi
incus project set "$PROJECT" limits.instances "$MAX_BOXES"
incus project set "$PROJECT" limits.cpu "$CPU_CAP"
incus project set "$PROJECT" limits.memory "$MEM_CAP"
note "caps: instances=$MAX_BOXES cpu=$CPU_CAP memory=$MEM_CAP (allocation caps, not usage caps)"

step "Volume 'cache' (shared /cache for Go, npm, Gradle, uv)"
if incus storage volume show default cache --project "$PROJECT" >/dev/null 2>&1; then
  note "exists"
else
  incus storage volume create default cache --project "$PROJECT"
  note "created"
fi

step "Default profile of project '$PROJECT'"
incus profile edit default --project "$PROJECT" < "$HERE/agents-profile.yaml"
note "applied agents-profile.yaml (limits.cpu=2, limits.memory=4GiB, nesting, /cache, cloud-init)"

# ---------------------------------------------------------------- Tailscale
step "Tailscale"
if ! command -v tailscale >/dev/null 2>&1; then
  install -d -m 0755 /usr/share/keyrings
  curl -fsSL "https://pkgs.tailscale.com/stable/ubuntu/${VERSION_CODENAME}.noarmor.gpg" \
    -o /usr/share/keyrings/tailscale-archive-keyring.gpg
  curl -fsSL "https://pkgs.tailscale.com/stable/ubuntu/${VERSION_CODENAME}.tailscale-keyring.list" \
    -o /etc/apt/sources.list.d/tailscale.list
  apt-get update -qq
  apt-get install -y -qq tailscale
  note "installed"
else
  note "already installed"
fi
systemctl enable --now tailscaled >/dev/null 2>&1

ts_state=$(tailscale status --json 2>/dev/null | sed -n 's/.*"BackendState": *"\([^"]*\)".*/\1/p' | head -n1 || true)
if [ "$ts_state" = Running ]; then
  note "already joined the tailnet; leaving it alone"
elif [ -n "${TS_AUTHKEY:-}" ]; then
  note "joining the tailnet as 'agent-box' (auth key read from \$TS_AUTHKEY, not stored)"
  # --accept-dns=false: keep WSL's resolv.conf; boxes use the incusbr0 resolver.
  tailscale up --hostname agent-box --accept-dns=false --authkey "$TS_AUTHKEY"
  unset TS_AUTHKEY
else
  note "TS_AUTHKEY not set: NOT joined. Later: sudo tailscale up --hostname agent-box --accept-dns=false"
  note "(or re-run install-pc.ps1 / this script with TS_AUTHKEY set)"
fi

# ---------------------------------------------------------------- agent-base
if [ "$BUILD_BASE" -eq 1 ]; then
  step "Building image 'agent-base'"
  if incus image alias list --format csv | cut -d, -f1 | grep -qx agent-base && [ "$REBUILD" -eq 0 ]; then
    note "alias exists; pass --rebuild-base to build again"
  else
    builder=agent-base-build
    incus delete --force "$builder" --project "$PROJECT" >/dev/null 2>&1 || true
    incus launch images:ubuntu/24.04/cloud "$builder" --project "$PROJECT"
    note "waiting for cloud-init (installs Go, Node, JDK, ...); this takes a few minutes"
    until incus exec "$builder" --project "$PROJECT" -- true >/dev/null 2>&1; do sleep 2; done
    incus exec "$builder" --project "$PROJECT" -- cloud-init status --wait --long \
      || die "cloud-init failed in $builder; see: incus exec $builder --project $PROJECT -- cat /var/log/agent-box-provision.log"
    incus stop "$builder" --project "$PROJECT"
    incus publish "$builder" --project "$PROJECT" --alias agent-base --reuse \
      description="agent-box base: git, build-essential, Go, Node LTS, uv, JDK 17, gh"
    incus delete "$builder" --project "$PROJECT"
    note "published as 'agent-base' (images are shared via the default project)"
  fi
else
  step "Image 'agent-base'"
  note "not built (optional). Build with: setup-distro.sh --build-base"
fi

# ---------------------------------------------------------------- VM capability
step "Virtual machines (/dev/kvm)"
if [ -e /dev/kvm ]; then
  note "/dev/kvm present: Incus VMs (incus launch --vm) are possible."
else
  note "/dev/kvm missing: only containers. VMs would need nested virtualization"
  note "(.wslconfig [wsl2] nestedVirtualization=true on Windows 11, then wsl --shutdown)."
fi

# ---------------------------------------------------------------- trust
step "Done. Add client certificates (run these here, once per client)"
cat <<'EOF'

  # Laptop, for agents (restricted to the agents project):
  incus config trust add laptop-agents --restricted --projects agents
  #   prints a token; on the laptop:
  #   incus remote add pc https://agent-box:8443 --token <token>
  #   incus project switch agents   # or pass --project agents

  # Tray on the PC (admin). It must use a DIFFERENT client cert than agents:
  incus config trust add boxtray
  #   on the PC, with its own config dir (e.g. INCUS_CONF=%LOCALAPPDATA%\agent-box\incus):
  #   incus remote add pc https://localhost:8443 --token <token>

  # Web UI: https://agent-box:8443 (import a client cert into the browser; see the Incus UI docs)
  # List / revoke:  incus config trust list ;  incus config trust remove <fingerprint>

EOF
trap - EXIT
