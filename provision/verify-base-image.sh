#!/usr/bin/env bash
# verify-base-image.sh - non-disruptive live verification for a staged agent-base image (#194).
# Compares baseline vs candidate boot/network/DNS timing, launches two candidate clones together
# to verify distinct identities, DHCPv4 leases/DUIDs, resolver readiness, environment-owned DNS
# lookup, SSH, masked units, and tool versions, and cleans up only the temporary boxes it creates.
# Leaves `agent-base`, the `default` profile, and all other boxes untouched.
#
# Usage:
#   provision/verify-base-image.sh [--remote pc] [--baseline agent-base] --candidate agent-base-candidate [--dns-name NAME] [--box path/to/box]
#   provision/verify-base-image.sh [--remote pc] --baseline-only [--dns-name NAME] [--box path/to/box]
#
# Flags:
#   --baseline NAME    existing image alias to measure as baseline (default: agent-base)
#   --candidate NAME   staged candidate image alias to test and compare against baseline
#   --baseline-only    measure only the baseline image (no candidate required)
#   --remote NAME      Incus remote name (default: local host; pass `pc` from the laptop)
#   --project NAME     Incus project name (default: agents)
#   --dns-name NAME    host-approved test name to resolve (default: <box>.incus on incusbr0;
#                      custom names and their resolved IPs are never printed)
#   --box PATH         optional path to box / box.exe to also measure end-to-end `box run --new --fresh` timing
#   --expected-go VER  expected Go version in /usr/local/go (default: GO_VERSION from agent-base.yaml, else 1.27.3)
#   --keep             keep temporary verify-* boxes on exit (for debugging)
set -Eeuo pipefail
exec </dev/null
# Prevent Git Bash on Windows from rewriting POSIX paths passed to native incus.exe.
export MSYS_NO_PATHCONV=1

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BASELINE=agent-base
CANDIDATE=
BASELINE_ONLY=0
REMOTE=
PROJECT=agents
DNS_NAME=
BOX_BIN=
EXPECTED_GO=
KEEP=0

while [ $# -gt 0 ]; do
  case "$1" in
    --baseline) [ $# -ge 2 ] || { echo "--baseline needs an alias" >&2; exit 2; }; BASELINE=$2; shift ;;
    --candidate) [ $# -ge 2 ] || { echo "--candidate needs an alias" >&2; exit 2; }; CANDIDATE=$2; shift ;;
    --baseline-only) BASELINE_ONLY=1 ;;
    --remote) [ $# -ge 2 ] || { echo "--remote needs a name" >&2; exit 2; }; REMOTE=$2; shift ;;
    --project) [ $# -ge 2 ] || { echo "--project needs a name" >&2; exit 2; }; PROJECT=$2; shift ;;
    --dns-name) [ $# -ge 2 ] || { echo "--dns-name needs a name" >&2; exit 2; }; DNS_NAME=$2; shift ;;
    --box) [ $# -ge 2 ] || { echo "--box needs a path" >&2; exit 2; }; BOX_BIN=$2; shift ;;
    --expected-go) [ $# -ge 2 ] || { echo "--expected-go needs a version" >&2; exit 2; }; EXPECTED_GO=$2; shift ;;
    --keep) KEEP=1 ;;
    -h|--help) sed -n '2,22p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
  shift
done

if [ "$BASELINE_ONLY" -eq 0 ] && [ -z "$CANDIDATE" ]; then
  echo "pass --candidate <alias> to verify a staged image, or --baseline-only to measure baseline only" >&2
  exit 2
fi
if [ "$BASELINE_ONLY" -eq 1 ] && [ -n "$CANDIDATE" ]; then
  echo "--baseline-only cannot be combined with --candidate" >&2
  exit 2
fi
if [ -n "$DNS_NAME" ]; then
  [[ "$DNS_NAME" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "--dns-name: use letters, digits, '.', '_' and '-' only" >&2; exit 2; }
fi
if [ -n "$BOX_BIN" ] && [ ! -x "$BOX_BIN" ]; then
  echo "--box binary is not executable: $BOX_BIN" >&2
  exit 2
fi
if [ -z "$EXPECTED_GO" ] && [ -r "$HERE/agent-base.yaml" ]; then
  EXPECTED_GO=$(sed -n 's/^[[:space:]]*GO_VERSION=\([0-9][0-9.]*\)[[:space:]]*$/\1/p' "$HERE/agent-base.yaml" | head -n1 || true)
fi
EXPECTED_GO=${EXPECTED_GO:-1.27.3}

PREFIX=${REMOTE:+${REMOTE}:}
RUN_ID=$(printf '%04x%02x' "$(( $$ % 65536 ))" "$(( RANDOM % 256 ))")
CREATED_BOXES=()
FAILURES=0

step() { printf '\n==> %s\n' "$*"; }
note() { printf '    %s\n' "$*"; }
pass() { printf '    [PASS] %s\n' "$*"; }
fail() { printf '    [FAIL] %s\n' "$*" >&2; FAILURES=$((FAILURES + 1)); }
die()  { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

incus_p() {
  incus --project "$PROJECT" "$@" </dev/null | tr -d '\r'
}

now_ms() {
  date +%s%3N
}

kv_get() {
  local data=$1 key=$2
  printf '%s\n' "$data" | awk -F= -v k="$key" '$1 == k { sub(/^[^=]*=/, ""); print; exit }'
}

# Return the environment-owned DNS name to query for box $1.
# Default is <box>.incus, answered over the network by incusbr0's dnsmasq (not /etc/hosts).
dns_target_for() {
  local box=$1
  if [ -n "$DNS_NAME" ]; then
    printf '%s' "$DNS_NAME"
  else
    printf '%s.incus' "$box"
  fi
}

# Safe label for logs/GitHub output (never echoes a custom --dns-name).
dns_label() {
  if [ -n "$DNS_NAME" ]; then
    printf 'host-approved (--dns-name)'
  else
    printf '<box>.incus'
  fi
}

cleanup() {
  if [ "$KEEP" -eq 1 ]; then
    if [ "${#CREATED_BOXES[@]}" -gt 0 ]; then
      note "keeping temporary boxes (--keep): ${CREATED_BOXES[*]}"
    fi
    return
  fi
  local b
  for b in "${CREATED_BOXES[@]}"; do
    incus --project "$PROJECT" delete --force "${PREFIX}${b}" >/dev/null 2>&1 </dev/null || true
  done
}
trap cleanup EXIT

delete_box() {
  local b=$1 remaining=() x
  if [ "$KEEP" -eq 1 ]; then return; fi
  incus --project "$PROJECT" delete --force "${PREFIX}${b}" >/dev/null 2>&1 </dev/null || true
  for x in "${CREATED_BOXES[@]}"; do
    [ "$x" = "$b" ] || remaining+=("$x")
  done
  CREATED_BOXES=("${remaining[@]}")
}

image_fp() {
  local alias=$1
  incus_p image alias list "${PREFIX}" --format csv | awk -F, -v a="$alias" '$1 == a { print $2; exit }'
}

# Launch a small temporary box with a short TTL so the reaper removes it if this script is killed.
# Args: <alias> <box-name> <clear-cloud-init:0|1>
launch_temp() {
  local alias=$1 name=$2 clear_ci=$3 exp
  exp=$(date -u -d '+30 minutes' +%Y-%m-%dT%H:%M:%SZ)
  local args=(
    launch "${PREFIX}${alias}" "${PREFIX}${name}"
    -c "user.expires=${exp}"
    -c "user.box.size=small"
    -c "limits.cpu=1"
    -c "limits.cpu.priority=0"
    -c "limits.memory=2GiB"
    -d "root,size=18GiB"
  )
  if [ "$clear_ci" -eq 1 ]; then
    # Simulate stripped default-profile.yaml even while the live default profile still has cloud-init.
    args+=(-c "cloud-init.user-data=")
  fi
  CREATED_BOXES+=("$name")
  incus_p "${args[@]}" >/dev/null
}

# Launch a box and measure independent elapsed milestones from `incus launch` start:
#   TIMING_LAUNCH_MS    incus launch returned
#   TIMING_EXEC_MS      first incus exec started inside the container
#   TIMING_DHCP_MS      eth0 acquired a global IPv4 address via DHCPv4
#   TIMING_RESOLVER_MS  /run/systemd/resolve/resolv.conf received an uplink nameserver
#   TIMING_LOOKUP_MS    environment-owned test name resolved via getent hosts
#   TIMING_BOOT_MS      systemctl is-system-running reached running|degraded
# Also sets matching container-uptime timestamps (TIMING_*_UP_MS) so DHCP vs resolver vs lookup
# ordering is captured at 25ms resolution inside the container without exec RTT skew.
measure_boot() {
  local alias=$1 name=$2 clear_ci=$3 target probe_out t0 t_launch t_done deadline rc
  target=$(dns_target_for "$name")
  t0=$(now_ms)
  launch_temp "$alias" "$name" "$clear_ci"
  t_launch=$(( $(now_ms) - t0 ))

  deadline=$(( $(now_ms) + 30000 ))
  probe_out=
  while :; do
    rc=0
    probe_out=$(incus --project "$PROJECT" exec -T "${PREFIX}${name}" -- bash -s "$target" <<'EOF' 2>/dev/null | tr -d '\r'
set +e
target=$1
up_ms() { awk '{printf "%d", $1 * 1000}' /proc/uptime; }
s0=$(date +%s%3N)
exec_up=$(up_ms)
dhcp_d=-1; dhcp_up=-1
res_d=-1; res_up=-1
lkp_d=-1; lkp_up=-1
boot_d=-1; boot_up=-1
end_net=$((s0 + 30000))
end_boot=$((s0 + 45000))

while :; do
  now=$(date +%s%3N)
  if [ "$dhcp_d" -lt 0 ]; then
    if ip -4 -o addr show dev eth0 scope global 2>/dev/null | grep -q 'inet '; then
      dhcp_d=$((now - s0)); dhcp_up=$(up_ms)
    fi
  fi
  if [ "$res_d" -lt 0 ]; then
    if grep -qE '^nameserver[[:space:]]+' /run/systemd/resolve/resolv.conf 2>/dev/null; then
      res_d=$((now - s0)); res_up=$(up_ms)
    fi
  fi
  if [ "$lkp_d" -lt 0 ]; then
    if getent hosts "$target" >/dev/null 2>&1; then
      lkp_d=$((now - s0)); lkp_up=$(up_ms)
    fi
  fi
  if [ "$boot_d" -lt 0 ]; then
    st=$(systemctl is-system-running 2>/dev/null || true)
    case "$st" in
      running|degraded) boot_d=$((now - s0)); boot_up=$(up_ms) ;;
    esac
  fi
  if [ "$dhcp_d" -ge 0 ] && [ "$res_d" -ge 0 ] && [ "$lkp_d" -ge 0 ] && [ "$boot_d" -ge 0 ]; then
    break
  fi
  if [ "$now" -ge "$end_net" ] && { [ "$dhcp_d" -lt 0 ] || [ "$res_d" -lt 0 ] || [ "$lkp_d" -lt 0 ]; }; then
    break
  fi
  if [ "$now" -ge "$end_boot" ]; then
    break
  fi
  sleep 0.025
done
total_d=$(( $(date +%s%3N) - s0 ))
printf 'EXEC_UP_MS=%s\n' "$exec_up"
printf 'DHCP_DELTA_MS=%s\nDHCP_UP_MS=%s\n' "$dhcp_d" "$dhcp_up"
printf 'RESOLVER_DELTA_MS=%s\nRESOLVER_UP_MS=%s\n' "$res_d" "$res_up"
printf 'LOOKUP_DELTA_MS=%s\nLOOKUP_UP_MS=%s\n' "$lkp_d" "$lkp_up"
printf 'BOOT_DELTA_MS=%s\nBOOT_UP_MS=%s\n' "$boot_d" "$boot_up"
printf 'TOTAL_DELTA_MS=%s\n' "$total_d"
EOF
    ) || rc=$?
    if [ "$rc" -eq 0 ] && printf '%s\n' "$probe_out" | grep -q '^TOTAL_DELTA_MS='; then
      break
    fi
    [ "$(now_ms)" -lt "$deadline" ] || die "timed out waiting for first exec probe in $name"
    sleep 0.05
  done
  t_done=$(now_ms)

  local total_d exec_ms dhcp_d res_d lkp_d boot_d
  total_d=$(kv_get "$probe_out" TOTAL_DELTA_MS)
  dhcp_d=$(kv_get "$probe_out" DHCP_DELTA_MS)
  res_d=$(kv_get "$probe_out" RESOLVER_DELTA_MS)
  lkp_d=$(kv_get "$probe_out" LOOKUP_DELTA_MS)
  boot_d=$(kv_get "$probe_out" BOOT_DELTA_MS)

  exec_ms=$(( (t_done - t0) - total_d ))
  if [ "$exec_ms" -lt "$t_launch" ]; then exec_ms=$t_launch; fi

  TIMING_LAUNCH_MS=$t_launch
  TIMING_EXEC_MS=$exec_ms
  TIMING_EXEC_UP_MS=$(kv_get "$probe_out" EXEC_UP_MS)
  TIMING_DHCP_MS=$([ "$dhcp_d" -ge 0 ] && echo $((exec_ms + dhcp_d)) || echo -1)
  TIMING_DHCP_UP_MS=$(kv_get "$probe_out" DHCP_UP_MS)
  TIMING_RESOLVER_MS=$([ "$res_d" -ge 0 ] && echo $((exec_ms + res_d)) || echo -1)
  TIMING_RESOLVER_UP_MS=$(kv_get "$probe_out" RESOLVER_UP_MS)
  TIMING_LOOKUP_MS=$([ "$lkp_d" -ge 0 ] && echo $((exec_ms + lkp_d)) || echo -1)
  TIMING_LOOKUP_UP_MS=$(kv_get "$probe_out" LOOKUP_UP_MS)
  TIMING_BOOT_MS=$([ "$boot_d" -ge 0 ] && echo $((exec_ms + boot_d)) || echo -1)
  TIMING_BOOT_UP_MS=$(kv_get "$probe_out" BOOT_UP_MS)

  if [ "$TIMING_DHCP_MS" -lt 0 ] || [ "$TIMING_RESOLVER_MS" -lt 0 ] || [ "$TIMING_LOOKUP_MS" -lt 0 ]; then
    die "timed out waiting for network/DNS in $name (dhcp_ms=$TIMING_DHCP_MS resolver_ms=$TIMING_RESOLVER_MS lookup_ms=$TIMING_LOOKUP_MS)"
  fi
}

fmt_ms() {
  local v=$1
  if [ "$v" -lt 0 ]; then printf 'timeout'; else printf '%sms' "$v"; fi
}

# Collect key-value facts from inside a running box (never echoes private DNS names/IPs).
inspect_box() {
  local name=$1 target
  target=$(dns_target_for "$name")
  incus --project "$PROJECT" exec -T "${PREFIX}${name}" -- bash -s "$target" <<'EOF' | tr -d '\r'
set +e
target=$1
printf 'HOSTNAME=%s\n' "$(hostname 2>/dev/null || true)"
printf 'ETC_HOSTNAME=%s\n' "$(cat /etc/hostname 2>/dev/null || true)"
if grep -qwF "$(hostname 2>/dev/null)" /etc/hosts 2>/dev/null; then
  printf 'HOSTS_MATCH=yes\n'
else
  printf 'HOSTS_MATCH=no\n'
fi
printf 'MACHINE_ID=%s\n' "$(cat /etc/machine-id 2>/dev/null || true)"
printf 'DBUS_MACHINE_ID=%s\n' "$(cat /var/lib/dbus/machine-id 2>/dev/null || true)"
if [ -f /etc/cloud/cloud-init.disabled ]; then
  printf 'CLOUD_INIT_DISABLED_FILE=yes\n'
else
  printf 'CLOUD_INIT_DISABLED_FILE=no\n'
fi
ci_status=$(cloud-init status 2>/dev/null | sed -n 's/^status:[[:space:]]*//p' | head -n1 || true)
printf 'CLOUD_INIT_STATUS=%s\n' "${ci_status:-unknown}"
ci_local_start=$(systemctl show cloud-init-local.service -p ExecMainStartTimestampMonotonic --value 2>/dev/null || echo unknown)
printf 'CLOUD_INIT_LOCAL_START=%s\n' "$ci_local_start"
sa_userspace=$(systemd-analyze 2>/dev/null | head -n1 || true)
printf 'SYSTEMD_ANALYZE=%s\n' "$sa_userspace"
ci_blame=$(systemd-analyze blame 2>/dev/null | awk '/cloud-init-local\.service/{print $1; exit}' || true)
printf 'CLOUD_INIT_LOCAL_BLAME=%s\n' "${ci_blame:-0}"
ipv4=$(ip -4 -o addr show dev eth0 scope global 2>/dev/null | awk '{split($4,a,"/"); print a[1]; exit}')
printf 'IPV4=%s\n' "$ipv4"
clientid=$(awk -F= '/^CLIENTID=/{print $2; exit}' /run/systemd/netif/leases/* 2>/dev/null || true)
printf 'DHCP_CLIENTID=%s\n' "$clientid"
if grep -qE '^nameserver[[:space:]]+' /run/systemd/resolve/resolv.conf 2>/dev/null; then
  printf 'RESOLVER_READY=yes\n'
else
  printf 'RESOLVER_READY=no\n'
fi
getent hosts "$target" >/dev/null 2>&1 && printf 'DNS_LOOKUP=ok\n' || printf 'DNS_LOOKUP=fail\n'
ssh_units=$(systemctl list-unit-files 'ssh.service' 'ssh.socket' --no-legend 2>/dev/null | wc -l | tr -d ' ')
printf 'SSH_UNITS=%s\n' "$ssh_units"
shopt -s nullglob
ssh_keys=(/etc/ssh/ssh_host_*)
printf 'SSH_HOST_KEY_COUNT=%s\n' "${#ssh_keys[@]}"
if [ "${#ssh_keys[@]}" -gt 0 ]; then
  printf 'SSH_HOST_KEY_HASH=%s\n' "$(sha256sum "${ssh_keys[@]}" | sha256sum | awk '{print $1}')"
else
  printf 'SSH_HOST_KEY_HASH=none\n'
fi
masked_bad=()
for u in unattended-upgrades.service apt-daily.timer apt-daily-upgrade.timer rsyslog.service polkit.service; do
  st=$(systemctl is-enabled "$u" 2>/dev/null || true)
  [ "$st" = "masked" ] || masked_bad+=("${u}:${st:-unknown}")
done
printf 'UNMASKED_IDLE=%s\n' "${masked_bad[*]:-none}"
[ -f /etc/netplan/50-cloud-init.yaml ] && printf 'NETPLAN_PRESENT=yes\n' || printf 'NETPLAN_PRESENT=no\n'
EOF
}

check_tools() {
  local name=$1
  incus --project "$PROJECT" exec -T "${PREFIX}${name}" -- bash -lc '
set +e
cd /tmp || exit 1
printf "GO_BIN=%s\n" "$(/usr/local/go/bin/go version 2>&1 | head -n1)"
printf "NODE=%s\n" "$(node -v 2>&1 | head -n1)"
printf "NPM=%s\n" "$(npm -v 2>&1 | head -n1)"
printf "UV=%s\n" "$(uv --version 2>&1 | head -n1)"
printf "GH=%s\n" "$(gh --version 2>&1 | head -n1)"
printf "JAVA=%s\n" "$(java -version 2>&1 | head -n1)"
printf "JQ=%s\n" "$(jq --version 2>&1 | head -n1)"
printf "GIT=%s\n" "$(git --version 2>&1 | head -n1)"
printf "GCC=%s\n" "$(gcc --version 2>&1 | head -n1)"
printf "ENV_GOCACHE=%s\n" "${GOCACHE:-}"
printf "ENV_GOMODCACHE=%s\n" "${GOMODCACHE:-}"
printf "ENV_ANDROID_HOME=%s\n" "${ANDROID_HOME:-}"
printf "ENV_PLAYWRIGHT=%s\n" "${PLAYWRIGHT_BROWSERS_PATH:-}"
printf "ENV_PUPPETEER=%s\n" "${PUPPETEER_CACHE_DIR:-}"
printf "LINK_PUPPETEER=%s\n" "$(readlink /root/.cache/puppeteer 2>/dev/null || true)"
printf "LINK_PLAYWRIGHT=%s\n" "$(readlink /root/.cache/ms-playwright 2>/dev/null || true)"
[ -f /etc/profile.d/agent-box.sh ] && printf "FILE_PROFILE=yes\n" || printf "FILE_PROFILE=no\n"
[ -f /etc/agent-box/android-sdk.init.gradle ] && printf "FILE_GRADLE_INIT=yes\n" || printf "FILE_GRADLE_INIT=no\n"
[ -x /usr/local/bin/agent-box-ensure-android-sdk ] && printf "FILE_ANDROID_HELPER=yes\n" || printf "FILE_ANDROID_HELPER=no\n"
[ -x /usr/local/bin/sdkmanager ] && printf "FILE_SDKMANAGER=yes\n" || printf "FILE_SDKMANAGER=no\n"
[ -d /work ] && printf "DIR_WORK=yes\n" || printf "DIR_WORK=no\n"
' </dev/null | tr -d '\r'
}

# Measure optional end-to-end `box run --new --fresh` launch-to-job-start wall-clock ms
# using the environment-owned test name (never public DNS).
measure_box_run() {
  local alias=$1 t0 out box_name rc=0 cmd
  local env_args=("BOX_IMAGE=$alias" "BOX_PROJECT=$PROJECT")
  if [ -n "$REMOTE" ]; then
    env_args+=("BOX_REMOTE=$REMOTE")
  fi
  if [ -n "$DNS_NAME" ]; then
    cmd="getent hosts '$DNS_NAME' >/dev/null && echo BOX_DNS_OK || echo BOX_DNS_FAIL"
  else
    cmd='getent hosts "$(hostname).incus" >/dev/null && echo BOX_DNS_OK || echo BOX_DNS_FAIL'
  fi
  t0=$(now_ms)
  out=$(env "${env_args[@]}" \
    "$BOX_BIN" run --new --fresh --size small --reason "Staged base image launch-to-job-start verification (#194)." \
    -- "$cmd" 2>&1) || rc=$?
  BOX_RUN_MS=$(( $(now_ms) - t0 ))
  BOX_RUN_RC=$rc
  BOX_RUN_DNS=$(printf '%s\n' "$out" | grep -oE 'BOX_DNS_(OK|FAIL)' | head -n1 || true)
  box_name=$(printf '%s\n' "$out" | sed -n 's/^box: job [^ ]* on \([^ ]*\).*/\1/p' | head -n1 | tr -d '\r')
  if [ -n "$box_name" ] && [ "$KEEP" -eq 0 ]; then
    incus --project "$PROJECT" delete --force "${PREFIX}${box_name}" >/dev/null 2>&1 </dev/null || true
  fi
}

step "Preflight (remote=${REMOTE:-local}, project=$PROJECT, expected Go=$EXPECTED_GO, dns=$(dns_label))"
BASELINE_FP=$(image_fp "$BASELINE")
[ -n "$BASELINE_FP" ] || die "baseline image alias '$BASELINE' not found"
note "baseline:  $BASELINE ($BASELINE_FP)"

CANDIDATE_FP=
if [ "$BASELINE_ONLY" -eq 0 ]; then
  CANDIDATE_FP=$(image_fp "$CANDIDATE")
  [ -n "$CANDIDATE_FP" ] || die "candidate image alias '$CANDIDATE' not found"
  note "candidate: $CANDIDATE ($CANDIDATE_FP)"
  if [ "$CANDIDATE_FP" = "$BASELINE_FP" ]; then
    note "WARNING: candidate and baseline aliases point to the same fingerprint ($CANDIDATE_FP)"
  fi
fi

step "Measuring baseline '$BASELINE'"
BASE_BOX="verify-${RUN_ID}-base"
measure_boot "$BASELINE" "$BASE_BOX" 0
BASE_LAUNCH_MS=$TIMING_LAUNCH_MS
BASE_EXEC_MS=$TIMING_EXEC_MS
BASE_EXEC_UP_MS=$TIMING_EXEC_UP_MS
BASE_DHCP_MS=$TIMING_DHCP_MS
BASE_DHCP_UP_MS=$TIMING_DHCP_UP_MS
BASE_RESOLVER_MS=$TIMING_RESOLVER_MS
BASE_RESOLVER_UP_MS=$TIMING_RESOLVER_UP_MS
BASE_LOOKUP_MS=$TIMING_LOOKUP_MS
BASE_LOOKUP_UP_MS=$TIMING_LOOKUP_UP_MS
BASE_BOOT_MS=$TIMING_BOOT_MS
BASE_BOOT_UP_MS=$TIMING_BOOT_UP_MS
BASE_INFO=$(inspect_box "$BASE_BOX")
BASE_TOOLS=$(check_tools "$BASE_BOX")
note "elapsed: launch=$(fmt_ms "$BASE_LAUNCH_MS") first_exec=$(fmt_ms "$BASE_EXEC_MS") dhcp=$(fmt_ms "$BASE_DHCP_MS") resolver=$(fmt_ms "$BASE_RESOLVER_MS") lookup=$(fmt_ms "$BASE_LOOKUP_MS") boot=$(fmt_ms "$BASE_BOOT_MS")"
note "uptime:  first_exec=$(fmt_ms "$BASE_EXEC_UP_MS") dhcp=$(fmt_ms "$BASE_DHCP_UP_MS") resolver=$(fmt_ms "$BASE_RESOLVER_UP_MS") lookup=$(fmt_ms "$BASE_LOOKUP_UP_MS") boot=$(fmt_ms "$BASE_BOOT_UP_MS")"
note "inside:  cloud-init=$(kv_get "$BASE_INFO" CLOUD_INIT_STATUS) cloud-init-local=$(kv_get "$BASE_INFO" CLOUD_INIT_LOCAL_BLAME) ip=$(kv_get "$BASE_INFO" IPV4) machine-id=$(kv_get "$BASE_INFO" MACHINE_ID)"
note "systemd: $(kv_get "$BASE_INFO" SYSTEMD_ANALYZE)"
delete_box "$BASE_BOX"

BASE_BOXRUN_MS=n/a
BASE_BOXRUN_DNS=n/a
if [ -n "$BOX_BIN" ]; then
  measure_box_run "$BASELINE"
  BASE_BOXRUN_MS="${BOX_RUN_MS}ms (rc=$BOX_RUN_RC)"
  BASE_BOXRUN_DNS="${BOX_RUN_DNS:-unknown}"
  note "box run --new --fresh: $BASE_BOXRUN_MS dns=$BASE_BOXRUN_DNS"
fi

if [ "$BASELINE_ONLY" -eq 1 ]; then
  step "Baseline tool versions ($BASELINE)"
  printf '%s\n' "$BASE_TOOLS" | grep -E '^(GO_BIN|NODE|NPM|UV|GH|JAVA|JQ|GIT|GCC)=' | sed 's/^/    /'
  note "baseline-only run complete (no candidate assertions run)"
  exit 0
fi

step "Measuring candidate '$CANDIDATE' and launching two simultaneous clones"
CAND_A="verify-${RUN_ID}-cand-a"
CAND_B="verify-${RUN_ID}-cand-b"

measure_boot "$CANDIDATE" "$CAND_A" 1
CAND_A_LAUNCH_MS=$TIMING_LAUNCH_MS
CAND_A_EXEC_MS=$TIMING_EXEC_MS
CAND_A_EXEC_UP_MS=$TIMING_EXEC_UP_MS
CAND_A_DHCP_MS=$TIMING_DHCP_MS
CAND_A_DHCP_UP_MS=$TIMING_DHCP_UP_MS
CAND_A_RESOLVER_MS=$TIMING_RESOLVER_MS
CAND_A_RESOLVER_UP_MS=$TIMING_RESOLVER_UP_MS
CAND_A_LOOKUP_MS=$TIMING_LOOKUP_MS
CAND_A_LOOKUP_UP_MS=$TIMING_LOOKUP_UP_MS
CAND_A_BOOT_MS=$TIMING_BOOT_MS
CAND_A_BOOT_UP_MS=$TIMING_BOOT_UP_MS
INFO_A=$(inspect_box "$CAND_A")
TOOLS_A=$(check_tools "$CAND_A")
note "clone A elapsed: launch=$(fmt_ms "$CAND_A_LAUNCH_MS") first_exec=$(fmt_ms "$CAND_A_EXEC_MS") dhcp=$(fmt_ms "$CAND_A_DHCP_MS") resolver=$(fmt_ms "$CAND_A_RESOLVER_MS") lookup=$(fmt_ms "$CAND_A_LOOKUP_MS") boot=$(fmt_ms "$CAND_A_BOOT_MS")"
note "clone A uptime:  first_exec=$(fmt_ms "$CAND_A_EXEC_UP_MS") dhcp=$(fmt_ms "$CAND_A_DHCP_UP_MS") resolver=$(fmt_ms "$CAND_A_RESOLVER_UP_MS") lookup=$(fmt_ms "$CAND_A_LOOKUP_UP_MS") boot=$(fmt_ms "$CAND_A_BOOT_UP_MS")"

# Launch clone B while clone A is still running so DHCPv4 leases coexist on incusbr0.
measure_boot "$CANDIDATE" "$CAND_B" 1
CAND_B_LAUNCH_MS=$TIMING_LAUNCH_MS
CAND_B_EXEC_MS=$TIMING_EXEC_MS
CAND_B_EXEC_UP_MS=$TIMING_EXEC_UP_MS
CAND_B_DHCP_MS=$TIMING_DHCP_MS
CAND_B_DHCP_UP_MS=$TIMING_DHCP_UP_MS
CAND_B_RESOLVER_MS=$TIMING_RESOLVER_MS
CAND_B_RESOLVER_UP_MS=$TIMING_RESOLVER_UP_MS
CAND_B_LOOKUP_MS=$TIMING_LOOKUP_MS
CAND_B_LOOKUP_UP_MS=$TIMING_LOOKUP_UP_MS
CAND_B_BOOT_MS=$TIMING_BOOT_MS
CAND_B_BOOT_UP_MS=$TIMING_BOOT_UP_MS
INFO_B=$(inspect_box "$CAND_B")
note "clone B elapsed: launch=$(fmt_ms "$CAND_B_LAUNCH_MS") first_exec=$(fmt_ms "$CAND_B_EXEC_MS") dhcp=$(fmt_ms "$CAND_B_DHCP_MS") resolver=$(fmt_ms "$CAND_B_RESOLVER_MS") lookup=$(fmt_ms "$CAND_B_LOOKUP_MS") boot=$(fmt_ms "$CAND_B_BOOT_MS")"
note "clone B uptime:  first_exec=$(fmt_ms "$CAND_B_EXEC_UP_MS") dhcp=$(fmt_ms "$CAND_B_DHCP_UP_MS") resolver=$(fmt_ms "$CAND_B_RESOLVER_UP_MS") lookup=$(fmt_ms "$CAND_B_LOOKUP_UP_MS") boot=$(fmt_ms "$CAND_B_BOOT_UP_MS")"

step "Verifying two-clone identity, cloud-init, DHCP/DNS, SSH, and masked units"
for pair in "A:$CAND_A" "B:$CAND_B"; do
  label=${pair%%:*}
  bname=${pair#*:}
  if [ "$label" = A ]; then info=$INFO_A; else info=$INFO_B; fi

  ci_file=$(kv_get "$info" CLOUD_INIT_DISABLED_FILE)
  ci_st=$(kv_get "$info" CLOUD_INIT_STATUS)
  ci_loc=$(kv_get "$info" CLOUD_INIT_LOCAL_START)
  if [ "$ci_file" = yes ] && [ "$ci_st" = disabled ] && [ "$ci_loc" = 0 ]; then
    pass "clone $label: cloud-init disabled (status=$ci_st, cloud-init-local never ran)"
  else
    fail "clone $label: cloud-init expected disabled (file=$ci_file status=$ci_st local_start=$ci_loc)"
  fi

  hn=$(kv_get "$info" HOSTNAME)
  ehn=$(kv_get "$info" ETC_HOSTNAME)
  hm=$(kv_get "$info" HOSTS_MATCH)
  if [ "$hn" = "$bname" ] && [ "$ehn" = "$bname" ] && [ "$hm" = yes ]; then
    pass "clone $label: hostname & /etc/hosts match '$bname'"
  else
    fail "clone $label: hostname mismatch (hostname=$hn /etc/hostname=$ehn /etc/hosts=$hm want=$bname)"
  fi

  mid=$(kv_get "$info" MACHINE_ID)
  dmid=$(kv_get "$info" DBUS_MACHINE_ID)
  if [[ "$mid" =~ ^[0-9a-f]{32}$ ]] && [ "$dmid" = "$mid" ]; then
    pass "clone $label: valid /etc/machine-id ($mid) matching /var/lib/dbus/machine-id"
  else
    fail "clone $label: invalid or mismatched machine-id (/etc=$mid dbus=$dmid)"
  fi

  ip=$(kv_get "$info" IPV4)
  cid=$(kv_get "$info" DHCP_CLIENTID)
  res=$(kv_get "$info" RESOLVER_READY)
  lkp=$(kv_get "$info" DNS_LOOKUP)
  if [ -n "$ip" ] && [ -n "$cid" ] && [ "$res" = yes ] && [ "$lkp" = ok ]; then
    pass "clone $label: DHCPv4=$ip, resolver configured, $(dns_label) resolves"
  else
    fail "clone $label: network/DNS check failed (ip=$ip clientid=$cid resolver=$res lookup=$lkp)"
  fi

  ssh_u=$(kv_get "$info" SSH_UNITS)
  ssh_c=$(kv_get "$info" SSH_HOST_KEY_COUNT)
  if [ "$ssh_u" = 0 ] && [ "$ssh_c" = 0 ]; then
    pass "clone $label: SSH absent (0 units, 0 /etc/ssh/ssh_host_* files)"
  else
    fail "clone $label: unexpected SSH state (units=$ssh_u host_key_files=$ssh_c)"
  fi

  unmasked=$(kv_get "$info" UNMASKED_IDLE)
  if [ "$unmasked" = none ]; then
    pass "clone $label: idle apt/housekeeping units masked"
  else
    fail "clone $label: expected idle units masked, found: $unmasked"
  fi

  if [ "$(kv_get "$info" NETPLAN_PRESENT)" = yes ]; then
    pass "clone $label: /etc/netplan/50-cloud-init.yaml preserved"
  else
    fail "clone $label: /etc/netplan/50-cloud-init.yaml missing"
  fi
done

MID_A=$(kv_get "$INFO_A" MACHINE_ID)
MID_B=$(kv_get "$INFO_B" MACHINE_ID)
if [ -n "$MID_A" ] && [ "$MID_A" != "$MID_B" ]; then
  pass "distinct machine-ids across clones ($MID_A != $MID_B)"
else
  fail "clones share machine-id ($MID_A == $MID_B)"
fi

IP_A=$(kv_get "$INFO_A" IPV4)
IP_B=$(kv_get "$INFO_B" IPV4)
if [ -n "$IP_A" ] && [ "$IP_A" != "$IP_B" ]; then
  pass "distinct DHCPv4 leases across clones ($IP_A != $IP_B)"
else
  fail "clones share DHCPv4 lease ($IP_A == $IP_B)"
fi

CID_A=$(kv_get "$INFO_A" DHCP_CLIENTID)
CID_B=$(kv_get "$INFO_B" DHCP_CLIENTID)
if [ -n "$CID_A" ] && [ "$CID_A" != "$CID_B" ]; then
  if [[ "$CID_A" == ff* && "$CID_B" == ff* && ${#CID_A} -gt 10 && ${#CID_B} -gt 10 ]]; then
    DUID_A=${CID_A:10}
    DUID_B=${CID_B:10}
    if [ "$DUID_A" != "$DUID_B" ]; then
      pass "distinct DHCPv4 CLIENTIDs and DUIDs across clones (${DUID_A} != ${DUID_B})"
    else
      fail "clones share DHCPv4 DUID suffix (${DUID_A})"
    fi
  else
    pass "distinct DHCPv4 CLIENTIDs across clones ($CID_A != $CID_B)"
  fi
else
  fail "clones share or lack DHCPv4 CLIENTID (A=$CID_A B=$CID_B)"
fi

step "Verifying tool versions and /etc/profile.d/agent-box.sh on clone A"
GO_VER=$(kv_get "$TOOLS_A" GO_BIN)
if printf '%s\n' "$GO_VER" | grep -q "go${EXPECTED_GO} "; then
  pass "Go matches expected $EXPECTED_GO ($GO_VER)"
else
  fail "Go mismatch: got '$GO_VER', expected 'go${EXPECTED_GO} '"
fi
for tk in NODE NPM UV GH JAVA JQ GIT GCC; do
  tv=$(kv_get "$TOOLS_A" "$tk")
  if [ -n "$tv" ] && ! printf '%s\n' "$tv" | grep -qi 'not found'; then
    pass "$tk: $tv"
  else
    fail "$tk missing or broken: '$tv'"
  fi
done
if [ "$(kv_get "$TOOLS_A" ENV_GOCACHE)" = "/cache/go-build" ] &&
   [ "$(kv_get "$TOOLS_A" ENV_GOMODCACHE)" = "/cache/go-mod" ] &&
   [ "$(kv_get "$TOOLS_A" ENV_ANDROID_HOME)" = "/cache/android-sdk" ] &&
   [ "$(kv_get "$TOOLS_A" ENV_PLAYWRIGHT)" = "/cache/ms-playwright" ] &&
   [ "$(kv_get "$TOOLS_A" ENV_PUPPETEER)" = "/cache/puppeteer" ] &&
   [ "$(kv_get "$TOOLS_A" LINK_PUPPETEER)" = "/cache/puppeteer" ] &&
   [ "$(kv_get "$TOOLS_A" LINK_PLAYWRIGHT)" = "/cache/ms-playwright" ] &&
   [ "$(kv_get "$TOOLS_A" FILE_PROFILE)" = yes ] &&
   [ "$(kv_get "$TOOLS_A" FILE_GRADLE_INIT)" = yes ] &&
   [ "$(kv_get "$TOOLS_A" FILE_ANDROID_HELPER)" = yes ] &&
   [ "$(kv_get "$TOOLS_A" FILE_SDKMANAGER)" = yes ] &&
   [ "$(kv_get "$TOOLS_A" DIR_WORK)" = yes ]; then
  pass "profile.d exports, /root/.cache symlinks, Android SDK lazy helpers, and /work verified"
else
  fail "profile.d / cache / Android SDK check failed:\n$TOOLS_A"
fi

delete_box "$CAND_A"
delete_box "$CAND_B"

CAND_BOXRUN_MS=n/a
CAND_BOXRUN_DNS=n/a
if [ -n "$BOX_BIN" ]; then
  step "Measuring candidate end-to-end box run --new --fresh"
  measure_box_run "$CANDIDATE"
  CAND_BOXRUN_MS="${BOX_RUN_MS}ms (rc=$BOX_RUN_RC)"
  CAND_BOXRUN_DNS="${BOX_RUN_DNS:-unknown}"
  if [ "$BOX_RUN_RC" -eq 0 ] && [ "$CAND_BOXRUN_DNS" = "BOX_DNS_OK" ]; then
    pass "box run --new --fresh with BOX_IMAGE=$CANDIDATE succeeded ($CAND_BOXRUN_MS, $CAND_BOXRUN_DNS)"
  else
    fail "box run --new --fresh with BOX_IMAGE=$CANDIDATE failed ($CAND_BOXRUN_MS, $CAND_BOXRUN_DNS)"
  fi
fi

step "Summary: baseline ($BASELINE $BASELINE_FP) vs candidate ($CANDIDATE $CANDIDATE_FP)"
printf '    %-26s %-18s %-18s %-18s\n' "Milestone (elapsed / up)" "Baseline" "Candidate A" "Candidate B"
printf '    %-26s %-18s %-18s %-18s\n' "incus launch" "$(fmt_ms "$BASE_LAUNCH_MS")" "$(fmt_ms "$CAND_A_LAUNCH_MS")" "$(fmt_ms "$CAND_B_LAUNCH_MS")"
printf '    %-26s %-18s %-18s %-18s\n' "first exec" "$(fmt_ms "$BASE_EXEC_MS")/$(fmt_ms "$BASE_EXEC_UP_MS")" "$(fmt_ms "$CAND_A_EXEC_MS")/$(fmt_ms "$CAND_A_EXEC_UP_MS")" "$(fmt_ms "$CAND_B_EXEC_MS")/$(fmt_ms "$CAND_B_EXEC_UP_MS")"
printf '    %-26s %-18s %-18s %-18s\n' "DHCPv4 address" "$(fmt_ms "$BASE_DHCP_MS")/$(fmt_ms "$BASE_DHCP_UP_MS")" "$(fmt_ms "$CAND_A_DHCP_MS")/$(fmt_ms "$CAND_A_DHCP_UP_MS")" "$(fmt_ms "$CAND_B_DHCP_MS")/$(fmt_ms "$CAND_B_DHCP_UP_MS")"
printf '    %-26s %-18s %-18s %-18s\n' "resolver config arrival" "$(fmt_ms "$BASE_RESOLVER_MS")/$(fmt_ms "$BASE_RESOLVER_UP_MS")" "$(fmt_ms "$CAND_A_RESOLVER_MS")/$(fmt_ms "$CAND_A_RESOLVER_UP_MS")" "$(fmt_ms "$CAND_B_RESOLVER_MS")/$(fmt_ms "$CAND_B_RESOLVER_UP_MS")"
printf '    %-26s %-18s %-18s %-18s\n' "DNS lookup ($(dns_label))" "$(fmt_ms "$BASE_LOOKUP_MS")/$(fmt_ms "$BASE_LOOKUP_UP_MS")" "$(fmt_ms "$CAND_A_LOOKUP_MS")/$(fmt_ms "$CAND_A_LOOKUP_UP_MS")" "$(fmt_ms "$CAND_B_LOOKUP_MS")/$(fmt_ms "$CAND_B_LOOKUP_UP_MS")"
printf '    %-26s %-18s %-18s %-18s\n' "systemd boot" "$(fmt_ms "$BASE_BOOT_MS")/$(fmt_ms "$BASE_BOOT_UP_MS")" "$(fmt_ms "$CAND_A_BOOT_MS")/$(fmt_ms "$CAND_A_BOOT_UP_MS")" "$(fmt_ms "$CAND_B_BOOT_MS")/$(fmt_ms "$CAND_B_BOOT_UP_MS")"
printf '    %-26s %-18s %-18s %-18s\n' "cloud-init status" "$(kv_get "$BASE_INFO" CLOUD_INIT_STATUS)" "$(kv_get "$INFO_A" CLOUD_INIT_STATUS)" "$(kv_get "$INFO_B" CLOUD_INIT_STATUS)"
if [ -n "$BOX_BIN" ]; then
  printf '    %-26s %-18s %-18s %-18s\n' "box run --new --fresh" "$BASE_BOXRUN_MS" "$CAND_BOXRUN_MS" "-"
fi

if [ "$FAILURES" -gt 0 ]; then
  printf '\nRESULT: FAILED (%d check(s) failed)\n' "$FAILURES" >&2
  exit 1
fi
printf '\nRESULT: PASSED (all staged-image checks succeeded)\n'
