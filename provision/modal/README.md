# Modal Cloud Runner (`runner-v1`)

Implements the [`runner-v1`](../../docs/runner-protocol.md) HTTP+JSON backend on Modal.

## Requirements

- Tested against `modal>=1.6.0` (uses `Sandbox.exec` stdin/stdout streaming rather than the removed `Sandbox.open` API).
- Python 3.12+ stdlib for local unit tests (`core.py`, `rates.py`, `test_core.py`).

## Files

- [`rates.py`](rates.py): Dated (2026-11-27) Modal compute rates (1x Functions, 3x Sandboxes, GPU rates).
- [`core.py`](core.py): Pure-stdlib protocol validation, sizing (`internal/mode/size.go`), admission guardrails, cost math, Volume cost ledger helpers, log chunking, and TOCTOU-free tar export script. Zero `modal` import so tests run anywhere.
- [`agent_box_runner.py`](agent_box_runner.py): Modal app (`agent-box-runner`), `app_factory`, ASGI endpoint (`requires_proxy_auth=True`, `max_containers=1`), `modal.Volume("agent-box-jobs")`, `modal.Dict("agent-box-state")`, privilege-dropped 1x size functions (`small`, `medium`, `large`, `gpu`), and isolated 3x Sandboxes (`mode: "sandbox"`, no `/jobs` mount).
- [`test_core.py`](test_core.py): `unittest` suite verified against all golden fixtures in [`docs/protocol/runner-v1/`](../../docs/protocol/runner-v1/).
- [`smoke.sh`](smoke.sh): End-to-end smoke check against a deployed Modal URL.

## Execution Tiers

- **Function mode (default, 1x rates):** Routes jobs to `run_small`, `run_medium`, `run_large`, or `run_gpu` with `retries=modal.Retries(max_retries=2)`. Commands execute as unprivileged `agent` user with an allowlisted env and `0700` `/jobs`. Preemption restarts append `[box] restarted after preemption (attempt N)` to the job log. Files are not retained after completion (`GET /v1/jobs/{id}/file` returns `409 gone`).
- **Sandbox mode (`mode: "sandbox"`, 3x CPU/RAM, 1x GPU):** Non-preemptible `modal.Sandbox` (without `/jobs` mount) retained for `keepSec` seconds after completion so `GET /v1/jobs/{id}/file?path=rel` can stream tar archives via atomic `O_NOFOLLOW` descriptor walks.

## Deploy & Test

Run local unit tests (stdlib only, no Modal account needed):

```sh
python3 -m py_compile provision/modal/*.py
python3 -m unittest discover -s provision/modal -p 'test_*.py' -v
```

Deploy to Modal (`modal>=1.6.0`):

```sh
modal deploy provision/modal/agent_box_runner.py
```

Run the live smoke test against a deployed endpoint:

```sh
RUNNER_URL=https://<workspace>--agent-box-runner-web-app.modal.run \
MODAL_KEY=<key> MODAL_SECRET=<secret> \
sh provision/modal/smoke.sh
```
