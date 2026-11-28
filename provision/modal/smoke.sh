#!/usr/bin/env sh
# Live smoke test for a deployed Modal agent-box-runner endpoint.
# Exercises: probe, submit echo job + wait + log read, cancel, and monthly cap refusal.
#
# Usage:
#   RUNNER_URL=https://<workspace>--agent-box-runner-web-app.modal.run \
#   MODAL_KEY=... MODAL_SECRET=... \
#   sh provision/modal/smoke.sh
set -eu

: "${RUNNER_URL:?set RUNNER_URL (e.g. https://...modal.run)}"
: "${MODAL_KEY:?set MODAL_KEY}"
: "${MODAL_SECRET:?set MODAL_SECRET}"

BASE_URL=${RUNNER_URL%/}
TMP_DIR=$(mktemp -d)
ORIG_CFG=""
cleanup() {
  if [ -n "$ORIG_CFG" ]; then
    curl -fsS -X PUT "$BASE_URL/v1/config" \
      -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET" \
      -H "Content-Type: application/json" -d "$ORIG_CFG" >/dev/null 2>&1 || true
  fi
  rm -rf "$TMP_DIR"
}
trap cleanup EXIT INT TERM

echo "1. Probing $BASE_URL/v1/probe ..."
PROBE=$(curl -fsS "$BASE_URL/v1/probe" \
  -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET")
printf '%s\n' "$PROBE"
ORIG_CFG=$(printf '%s' "$PROBE" | python3 -c '
import json, sys
p = json.load(sys.stdin)
print(json.dumps({
    "capUSD": p["budget"]["capUSD"],
    "maxConcurrent": p["maxConcurrent"],
    "maxTimeoutSec": p["maxTimeoutSec"],
    "keepSec": 0,
    "mode": "function",
}))
')

# Build a minimal valid git bundle containing one commit at refs/agent-box/<ref>.
REPO_DIR="$TMP_DIR/repo"
git init -q "$REPO_DIR"
git -C "$REPO_DIR" config user.email "smoke@example.invalid"
git -C "$REPO_DIR" config user.name "smoke"
printf 'smoke\n' > "$REPO_DIR/README"
git -C "$REPO_DIR" add README
git -C "$REPO_DIR" commit -q -m "smoke"
SNAP=$(git -C "$REPO_DIR" rev-parse HEAD)

make_spec_and_bundle() {
  ref=$1
  cmd_json=$2
  git -C "$REPO_DIR" update-ref "refs/agent-box/$ref" "$SNAP"
  git -C "$REPO_DIR" bundle create "$TMP_DIR/$ref.bundle" "refs/agent-box/$ref" >/dev/null 2>&1
  python3 - "$ref" "$SNAP" "$cmd_json" > "$TMP_DIR/$ref.spec.json" <<'PY'
import json, sys
ref, snap, cmd_json = sys.argv[1], sys.argv[2], sys.argv[3]
print(json.dumps({
    "id": f"smoke.{ref}",
    "repo": "smoke",
    "ref": ref,
    "cmd": json.loads(cmd_json),
    "env": {},
    "timeoutSec": 120,
    "size": "small",
    "reason": "runner-v1 live smoke test",
    "source": {"commit": snap, "dirty": False, "snapshotCommit": snap},
}))
PY
}

STAMP=$(date -u +%Y%m%d-%H%M%S)

echo "2. Submitting echo job ..."
REF1="${STAMP}-0001"
make_spec_and_bundle "$REF1" '["echo", "hi"]'
CODE=$(curl -sS -o "$TMP_DIR/sub1.json" -w '%{http_code}' -X POST "$BASE_URL/v1/jobs" \
  -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET" \
  -F "spec=@$TMP_DIR/$REF1.spec.json;type=application/json" \
  -F "bundle=@$TMP_DIR/$REF1.bundle;type=application/octet-stream")
[ "$CODE" = "201" ] || { echo "Expected 201, got $CODE: $(cat "$TMP_DIR/sub1.json")" >&2; exit 1; }

STATE=""
i=0
while [ "$i" -lt 12 ]; do
  JOB_JSON=$(curl -fsS "$BASE_URL/v1/jobs/smoke.$REF1?wait=25" \
    -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET")
  STATE=$(printf '%s' "$JOB_JSON" | python3 -c 'import json, sys; print(json.load(sys.stdin)["state"])')
  case "$STATE" in
    done|cancelled|lost) break ;;
  esac
  i=$((i + 1))
done
[ "$STATE" = "done" ] || { echo "Expected done, got $STATE: $JOB_JSON" >&2; exit 1; }

echo "3. Reading job log ..."
LOG_JSON=$(curl -fsS "$BASE_URL/v1/jobs/smoke.$REF1/log?offset=0" \
  -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET")
printf '%s\n' "$LOG_JSON"
printf '%s' "$LOG_JSON" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert "hi" in d["data"], f"missing hi in log: {d}"
'

echo "4. Submitting and cancelling sleep job ..."
REF2="${STAMP}-0002"
make_spec_and_bundle "$REF2" '["sleep", "60"]'
CODE=$(curl -sS -o "$TMP_DIR/sub2.json" -w '%{http_code}' -X POST "$BASE_URL/v1/jobs" \
  -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET" \
  -F "spec=@$TMP_DIR/$REF2.spec.json;type=application/json" \
  -F "bundle=@$TMP_DIR/$REF2.bundle;type=application/octet-stream")
[ "$CODE" = "201" ] || { echo "Expected 201, got $CODE" >&2; exit 1; }
CANCEL_JSON=$(curl -fsS -X POST "$BASE_URL/v1/jobs/smoke.$REF2/cancel" \
  -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET")
printf '%s\n' "$CANCEL_JSON"
printf '%s' "$CANCEL_JSON" | python3 -c '
import json, sys
d = json.load(sys.stdin)
assert d.get("cancelled") is True and d.get("exit") == 137, d
'

echo "5. Testing monthly budget cap refusal (402 budget) ..."
curl -fsS -X PUT "$BASE_URL/v1/config" \
  -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET" \
  -H "Content-Type: application/json" -d '{"capUSD": 0}' >/dev/null
REF3="${STAMP}-0003"
make_spec_and_bundle "$REF3" '["echo", "blocked"]'
CODE=$(curl -sS -o "$TMP_DIR/sub3.json" -w '%{http_code}' -X POST "$BASE_URL/v1/jobs" \
  -H "Modal-Key: $MODAL_KEY" -H "Modal-Secret: $MODAL_SECRET" \
  -F "spec=@$TMP_DIR/$REF3.spec.json;type=application/json" \
  -F "bundle=@$TMP_DIR/$REF3.bundle;type=application/octet-stream")
[ "$CODE" = "402" ] || { echo "Expected 402, got $CODE: $(cat "$TMP_DIR/sub3.json")" >&2; exit 1; }
cat "$TMP_DIR/sub3.json"
echo
echo "Smoke test passed."
