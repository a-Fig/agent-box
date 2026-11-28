"""Modal runner-v1 backend (`agent-box-runner`).

Exposes the HTTP+JSON endpoints defined in docs/runner-protocol.md via a
Modal ASGI web endpoint (`requires_proxy_auth=True`) backed by:
  - 1x Modal Functions per size (`small`, `medium`, `large`, `gpu`) with preemption retries
  - 3x Modal Sandboxes (opt-in `mode: "sandbox"`) with post-job `keepSec` file retention
  - `modal.Volume("agent-box-jobs")` for git bundles and streaming job logs
  - `modal.Dict("agent-box-state")` for jobs, config, pause flag, and per-job monthly costs
"""

from __future__ import annotations

import asyncio
import json
import os
from pathlib import Path
import shlex
import signal
import subprocess
import sys
import tempfile
import time
from typing import Any

import modal

# Ensure core.py and rates.py are importable both locally and inside Modal containers (/root).
_HERE = Path(__file__).resolve().parent
if str(_HERE) not in sys.path:
    sys.path.insert(0, str(_HERE))

import core  # noqa: E402
import rates  # noqa: E402

APP_NAME = "agent-box-runner"
JOBS_VOLUME_NAME = "agent-box-jobs"
STATE_DICT_NAME = "agent-box-state"
JOBS_MOUNT = "/jobs"
COMMIT_INTERVAL_SEC = 5.0
MAX_FUNCTION_TIMEOUT_SEC = 86400

# Toolchain versions mirror go.mod and provision/agent-base.yaml (built by provision/setup-host.sh).
GO_VERSION = "1.27.4"  # matches go.mod
NODE_MAJOR = "22"  # matches .github/workflows/test.yml and agent-base Node LTS
GPU_TYPE = os.environ.get("AGENT_BOX_GPU_TYPE", rates.DEFAULT_GPU_TYPE)

app = modal.App(APP_NAME)
jobs_volume = modal.Volume.from_name(JOBS_VOLUME_NAME, create_if_missing=True)
state_dict = modal.Dict.from_name(STATE_DICT_NAME, create_if_missing=True)

base_image = (
    modal.Image.debian_slim(python_version="3.12")
    # System packages mirroring provision/agent-base.yaml (apt_pkgs).
    .apt_install(
        "git",
        "build-essential",
        "curl",
        "ca-certificates",
        "unzip",
        "jq",
        "openjdk-17-jdk-headless",
        "tar",
        "gzip",
    )
    # Go toolchain matching go.mod (mirroring provision/agent-base.yaml's GO_VERSION install).
    .run_commands(
        f'arch=$(dpkg --print-architecture) && '
        f'curl -fsSL "https://dl.google.com/go/go{GO_VERSION}.linux-${{arch}}.tar.gz" -o /tmp/go.tgz && '
        f'tar -C /usr/local -xzf /tmp/go.tgz && rm -f /tmp/go.tgz',
        # Node 22 matching .github/workflows/test.yml and agent-base.yaml.
        f"curl -fsSL https://deb.nodesource.com/setup_{NODE_MAJOR}.x | bash - && apt-get install -y -qq nodejs",
        # uv matching provision/agent-base.yaml.
        "curl -LsSf https://astral.sh/uv/install.sh | env UV_INSTALL_DIR=/usr/local/bin UV_NO_MODIFY_PATH=1 sh",
    )
    .pip_install("fastapi[standard]", "python-multipart")
    .env(
        {
            "PATH": "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            "GOPATH": "/root/go",
        }
    )
    .add_local_file(str(_HERE / "rates.py"), "/root/rates.py")
    .add_local_file(str(_HERE / "core.py"), "/root/core.py")
)


def _job_dir(job_id: str) -> Path:
    return Path(JOBS_MOUNT) / job_id


def _log_path(job_id: str) -> Path:
    return _job_dir(job_id) / "job.log"


def _bundle_path(job_id: str) -> Path:
    return _job_dir(job_id) / "bundle.git"


def _read_tail_and_size(log_file: Path) -> tuple[int, str]:
    if not log_file.exists():
        return 0, ""
    data = log_file.read_bytes()
    return len(data), core.compute_tail(data)


def _record_monthly_cost(job_id: str, job: dict[str, Any]) -> None:
    cost_usd = float((job.get("cost") or {}).get("usd", 0.0))
    ts = core.parse_rfc3339(job.get("endedAt")) or core.utc_now()
    state_dict[core.cost_entry_key(core.month_key(ts), job_id)] = cost_usd


def _is_cancelled(job_id: str) -> bool:
    rec = state_dict.get(core.job_entry_key(job_id))
    return isinstance(rec, dict) and rec.get("state") == "cancelled"


def _prepare_command(cmd: list[str]) -> list[str]:
    if len(cmd) == 1:
        return ["bash", "-lc", f"set -o pipefail; {cmd[0]}"]
    return list(cmd)


def _execute_in_workspace(job_id: str, rec: dict[str, Any]) -> int:
    """Unpack the job's git bundle into a fresh temp worktree and stream output to job.log."""
    log_file = _log_path(job_id)
    log_file.parent.mkdir(parents=True, exist_ok=True)
    bundle = _bundle_path(job_id)

    with tempfile.TemporaryDirectory(prefix="agent-box-") as tmpdir:
        workdir = Path(tmpdir) / rec["repo"]
        workdir.mkdir(parents=True, exist_ok=True)

        unpack_script = (
            f"git init -q {shlex.quote(str(workdir))} && "
            f"git -C {shlex.quote(str(workdir))} fetch -q {shlex.quote(str(bundle))} "
            f"{shlex.quote('refs/agent-box/' + rec['ref'])} && "
            f"git -C {shlex.quote(str(workdir))} checkout -q -f FETCH_HEAD"
        )
        unpack = subprocess.run(
            ["bash", "-lc", unpack_script],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        if unpack.returncode != 0:
            with log_file.open("ab") as fh:
                fh.write(f"[box] failed to unpack bundle: {unpack.stdout}\n".encode("utf-8", errors="replace"))
            jobs_volume.commit()
            return unpack.returncode or 1

        env = os.environ.copy()
        for k, v in (rec.get("env") or {}).items():
            env[str(k)] = str(v)

        proc = subprocess.Popen(
            _prepare_command(rec["cmd"]),
            cwd=str(workdir),
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )

        # Drain stdout in a background thread so timeout/cancel checks run on a 1s/5s cadence.
        import threading

        def _drain() -> None:
            assert proc.stdout is not None
            with log_file.open("ab") as fh:
                while True:
                    chunk = proc.stdout.read(4096)
                    if not chunk:
                        break
                    fh.write(chunk)
                    fh.flush()

        reader = threading.Thread(target=_drain, daemon=True)
        reader.start()

        start_mono = time.monotonic()
        last_commit = start_mono
        timeout_sec = int(rec.get("timeoutSec", 3600))
        timed_out = False
        cancelled = False

        while proc.poll() is None:
            now_mono = time.monotonic()
            if now_mono - start_mono >= timeout_sec:
                timed_out = True
                break
            if _is_cancelled(job_id):
                cancelled = True
                break
            if now_mono - last_commit >= COMMIT_INTERVAL_SEC:
                jobs_volume.commit()
                log_bytes, tail = _read_tail_and_size(log_file)
                cur = state_dict.get(core.job_entry_key(job_id))
                if isinstance(cur, dict) and cur.get("state") == "running":
                    cur["logBytes"] = log_bytes
                    cur["tail"] = tail
                    state_dict[core.job_entry_key(job_id)] = cur
                last_commit = now_mono
            time.sleep(0.5)

        if timed_out or cancelled:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            proc.wait(timeout=10)
            if timed_out:
                with log_file.open("ab") as fh:
                    fh.write(f"\n[box] timed out after {timeout_sec}s\n".encode("utf-8"))

        reader.join(timeout=5)
        jobs_volume.commit()

        if cancelled:
            return 137
        if timed_out:
            return 124
        return int(proc.returncode or 0)


def _run_function_job(job_id: str) -> None:
    jobs_volume.reload()
    key = core.job_entry_key(job_id)
    rec = state_dict.get(key)
    if not isinstance(rec, dict) or rec.get("state") == "cancelled":
        return

    log_file = _log_path(job_id)
    log_file.parent.mkdir(parents=True, exist_ok=True)

    if rec.get("state") == "running":
        attempt = int(rec.get("attempt", 1)) + 1
        rec["attempt"] = attempt
        with log_file.open("ab") as fh:
            fh.write(f"[box] restarted after preemption (attempt {attempt})\n".encode("utf-8"))
        jobs_volume.commit()
    else:
        core.transition_job(rec, "running", now=core.utc_now(), attempt=1)
    state_dict[key] = rec

    exit_code = _execute_in_workspace(job_id, rec)
    log_bytes, tail = _read_tail_and_size(log_file)

    latest = state_dict.get(key)
    if not isinstance(latest, dict) or latest.get("state") == "cancelled":
        return
    final_state = "cancelled" if exit_code == 137 and _is_cancelled(job_id) else "done"
    core.transition_job(
        latest,
        final_state,
        now=core.utc_now(),
        exit_code=exit_code,
        log_bytes=log_bytes,
        tail=tail,
    )
    _record_monthly_cost(job_id, latest)
    state_dict[key] = latest


@app.function(
    image=base_image,
    cpu=core.SIZES["small"].cpu_cores,
    memory=core.SIZES["small"].memory_mib,
    timeout=MAX_FUNCTION_TIMEOUT_SEC,
    volumes={JOBS_MOUNT: jobs_volume},
    retries=modal.Retries(max_retries=2),
)
def run_small(job_id: str) -> None:
    _run_function_job(job_id)


@app.function(
    image=base_image,
    cpu=core.SIZES["medium"].cpu_cores,
    memory=core.SIZES["medium"].memory_mib,
    timeout=MAX_FUNCTION_TIMEOUT_SEC,
    volumes={JOBS_MOUNT: jobs_volume},
    retries=modal.Retries(max_retries=2),
)
def run_medium(job_id: str) -> None:
    _run_function_job(job_id)


@app.function(
    image=base_image,
    cpu=core.SIZES["large"].cpu_cores,
    memory=core.SIZES["large"].memory_mib,
    timeout=MAX_FUNCTION_TIMEOUT_SEC,
    volumes={JOBS_MOUNT: jobs_volume},
    retries=modal.Retries(max_retries=2),
)
def run_large(job_id: str) -> None:
    _run_function_job(job_id)


@app.function(
    image=base_image,
    cpu=core.SIZES["gpu"].cpu_cores,
    memory=core.SIZES["gpu"].memory_mib,
    gpu=GPU_TYPE,
    timeout=MAX_FUNCTION_TIMEOUT_SEC,
    volumes={JOBS_MOUNT: jobs_volume},
    retries=modal.Retries(max_retries=2),
)
def run_gpu(job_id: str) -> None:
    _run_function_job(job_id)


FUNCTION_BY_SIZE = {
    "small": run_small,
    "medium": run_medium,
    "large": run_large,
    "gpu": run_gpu,
}


@app.function(
    image=base_image,
    cpu=0.25,
    memory=256,
    timeout=MAX_FUNCTION_TIMEOUT_SEC,
    volumes={JOBS_MOUNT: jobs_volume},
    retries=0,
)
def run_sandbox_job(job_id: str) -> None:
    """Supervise a non-preemptible 3x Modal Sandbox and retain it for `keepSec`."""
    jobs_volume.reload()
    key = core.job_entry_key(job_id)
    rec = state_dict.get(key)
    if not isinstance(rec, dict) or rec.get("state") == "cancelled":
        return

    cfg = core.normalize_config(state_dict.get("config"))
    keep_sec = int(cfg.get("keepSec", 0))
    timeout_sec = int(rec.get("timeoutSec", 3600))
    size_spec = core.SIZES[rec["size"]]
    gpu_arg = rec.get("gpuType", GPU_TYPE) if size_spec.gpu else None

    sb_kwargs: dict[str, Any] = {
        "image": base_image,
        "cpu": size_spec.cpu_cores,
        "memory": size_spec.memory_mib,
        "timeout": timeout_sec + keep_sec + 60,
        "volumes": {JOBS_MOUNT: jobs_volume},
        "app": app,
    }
    if gpu_arg:
        sb_kwargs["gpu"] = gpu_arg

    sb = modal.Sandbox.create("sleep", str(timeout_sec + keep_sec + 60), **sb_kwargs)
    rec["sandboxId"] = sb.object_id
    core.transition_job(rec, "running", now=core.utc_now(), attempt=1)
    state_dict[key] = rec

    log_file = _log_path(job_id)
    log_file.parent.mkdir(parents=True, exist_ok=True)
    workdir = f"/work/{rec['repo']}"
    bundle = str(_bundle_path(job_id))
    cmd_args = _prepare_command(rec["cmd"])
    env_prefix = " ".join(f"{shlex.quote(k)}={shlex.quote(str(v))}" for k, v in (rec.get("env") or {}).items())
    cmd_str = " ".join(shlex.quote(arg) for arg in cmd_args)
    if env_prefix:
        cmd_str = f"env {env_prefix} {cmd_str}"

    inner = (
        f"mkdir -p {shlex.quote(workdir)} && "
        f"git init -q {shlex.quote(workdir)} && "
        f"git -C {shlex.quote(workdir)} fetch -q {shlex.quote(bundle)} "
        f"{shlex.quote('refs/agent-box/' + rec['ref'])} && "
        f"git -C {shlex.quote(workdir)} checkout -q -f FETCH_HEAD && "
        f"cd {shlex.quote(workdir)} && exec {cmd_str} 2>&1"
    )

    exit_code = 0
    timed_out = False
    cancelled = False
    try:
        proc = sb.exec("bash", "-lc", inner, timeout=timeout_sec)
        last_commit = time.monotonic()
        with log_file.open("ab") as fh:
            for chunk in proc.stdout:
                data = chunk.encode("utf-8", errors="replace") if isinstance(chunk, str) else chunk
                fh.write(data)
                fh.flush()
                if _is_cancelled(job_id):
                    cancelled = True
                    break
                if time.monotonic() - last_commit >= COMMIT_INTERVAL_SEC:
                    jobs_volume.commit()
                    log_bytes, tail = _read_tail_and_size(log_file)
                    cur = state_dict.get(key)
                    if isinstance(cur, dict) and cur.get("state") == "running":
                        cur["logBytes"] = log_bytes
                        cur["tail"] = tail
                        state_dict[key] = cur
                    last_commit = time.monotonic()

        if cancelled:
            sb.terminate()
            exit_code = 137
        else:
            exit_code = int(proc.wait())
    except Exception as exc:  # Modal TimeoutError or Sandbox failure
        if "timeout" in type(exc).__name__.lower():
            timed_out = True
            exit_code = 124
            with log_file.open("ab") as fh:
                fh.write(f"\n[box] timed out after {timeout_sec}s\n".encode("utf-8"))
        else:
            exit_code = 1
            with log_file.open("ab") as fh:
                fh.write(f"\n[box] sandbox error: {exc}\n".encode("utf-8"))
    finally:
        jobs_volume.commit()

    log_bytes, tail = _read_tail_and_size(log_file)
    latest = state_dict.get(key)
    if isinstance(latest, dict) and latest.get("state") not in core.TERMINAL_STATES:
        final_state = "cancelled" if cancelled else "done"
        core.transition_job(
            latest,
            final_state,
            now=core.utc_now(),
            exit_code=exit_code,
            log_bytes=log_bytes,
            tail=tail,
            keep_sec=0 if (cancelled or timed_out) else keep_sec,
        )
        _record_monthly_cost(job_id, latest)
        state_dict[key] = latest

    if keep_sec > 0 and not cancelled and not timed_out:
        deadline = time.monotonic() + keep_sec
        while time.monotonic() < deadline:
            if _is_cancelled(job_id):
                break
            time.sleep(min(5.0, max(0.5, deadline - time.monotonic())))
    try:
        sb.terminate()
    except Exception:
        pass


def _cancel_backing_execution(rec: dict[str, Any]) -> None:
    sb_id = rec.get("sandboxId")
    if sb_id:
        try:
            modal.Sandbox.from_id(sb_id).terminate()
        except Exception:
            pass
    call_id = rec.get("callId")
    if call_id:
        try:
            modal.FunctionCall.from_id(call_id).cancel()
        except Exception:
            pass


@app.function(
    image=base_image,
    cpu=0.5,
    memory=512,
    min_containers=0,
    volumes={JOBS_MOUNT: jobs_volume},
)
@modal.asgi_app(requires_proxy_auth=True)
def web_app():
    from fastapi import FastAPI, HTTPException, Query, Request
    from fastapi.responses import JSONResponse, Response

    api = FastAPI(title="agent-box-runner", version="1")

    def _err(status: int, code: str, message: str) -> JSONResponse:
        return JSONResponse(status_code=status, content=core.error_envelope(code, message))

    @api.get("/v1/probe")
    async def probe() -> JSONResponse:
        state = core.snapshot_dict_state(state_dict)
        return JSONResponse(status_code=200, content=core.build_probe_response(state))

    @api.post("/v1/jobs")
    async def submit_job(request: Request) -> JSONResponse:
        try:
            form = await request.form()
        except Exception:
            return _err(400, "invalid", "Request must be multipart/form-data with 'spec' and 'bundle'.")

        spec_part = form.get("spec")
        bundle_part = form.get("bundle")
        if spec_part is None or bundle_part is None:
            return _err(400, "invalid", "Multipart form must contain 'spec' and 'bundle' parts.")

        try:
            raw_spec = await spec_part.read() if hasattr(spec_part, "read") else str(spec_part).encode("utf-8")
            spec_obj = json.loads(raw_spec.decode("utf-8"))
        except Exception:
            return _err(400, "invalid", "Part 'spec' must be valid JSON.")

        try:
            bundle_bytes = await bundle_part.read() if hasattr(bundle_part, "read") else bytes(bundle_part)
        except Exception:
            return _err(400, "invalid", "Could not read 'bundle' part.")
        if not bundle_bytes:
            return _err(400, "invalid", "Part 'bundle' must not be empty.")

        state = core.snapshot_dict_state(state_dict)
        ok, status, err_code, msg, norm = core.validate_spec(spec_obj, state["config"])
        if not ok or norm is None:
            return _err(status, err_code, msg)

        job_id = norm["id"]
        existing = state_dict.get(core.job_entry_key(job_id))
        if isinstance(existing, dict):
            return JSONResponse(status_code=200, content=core.build_job_response(existing, state))

        ok, status, err_code, msg = core.check_admission(state, norm)
        if not ok:
            return _err(status, err_code, msg)

        bundle_file = _bundle_path(job_id)
        bundle_file.parent.mkdir(parents=True, exist_ok=True)
        bundle_file.write_bytes(bundle_bytes)
        jobs_volume.commit()

        rec = core.new_job_record(norm)
        state_dict[core.job_entry_key(job_id)] = rec

        if norm["mode"] == "sandbox":
            call = run_sandbox_job.spawn(job_id)
        else:
            call = FUNCTION_BY_SIZE[norm["size"]].spawn(job_id)
        rec["callId"] = call.object_id
        state_dict[core.job_entry_key(job_id)] = rec

        return JSONResponse(status_code=201, content={"id": job_id, "state": "queued"})

    @api.get("/v1/jobs")
    async def list_jobs(limit: int = Query(default=core.DEFAULT_LIST_LIMIT)) -> JSONResponse:
        if limit < 1:
            return _err(400, "invalid", "limit must be a positive integer.")
        clamped = min(limit, core.MAX_LIST_LIMIT)
        state = core.snapshot_dict_state(state_dict)
        now = core.utc_now()
        ordered = sorted(
            state["jobs"].values(),
            key=lambda j: j.get("createdAt") or j.get("startedAt") or "",
            reverse=True,
        )[:clamped]
        return JSONResponse(
            status_code=200,
            content={"jobs": [core.build_job_response(j, state, now) for j in ordered]},
        )

    @api.get("/v1/jobs/{job_id}")
    async def get_job(job_id: str, wait: int = Query(default=0)) -> JSONResponse:
        if wait < 0:
            return _err(400, "invalid", "wait must be a non-negative integer.")
        key = core.job_entry_key(job_id)
        rec = state_dict.get(key)
        if not isinstance(rec, dict):
            return _err(404, "not_found", f"Unknown job {job_id!r}.")

        initial_state = rec.get("state")
        wait_sec = min(wait, core.MAX_POLL_WAIT_SEC)
        if wait_sec > 0 and initial_state in core.ACTIVE_STATES:
            deadline = time.monotonic() + wait_sec
            while time.monotonic() < deadline:
                await asyncio.sleep(0.5)
                latest = state_dict.get(key)
                if not isinstance(latest, dict):
                    break
                rec = latest
                cur_state = rec.get("state")
                if cur_state in core.TERMINAL_STATES or cur_state != initial_state:
                    break

        state = core.snapshot_dict_state(state_dict)
        return JSONResponse(status_code=200, content=core.build_job_response(rec, state))

    @api.get("/v1/jobs/{job_id}/log")
    async def get_job_log(
        job_id: str,
        offset: int = Query(default=0),
        limit: int = Query(default=core.MAX_LOG_CHUNK),
    ) -> JSONResponse:
        if offset < 0 or limit < 1:
            return _err(400, "invalid", "offset must be >= 0 and limit must be >= 1.")
        rec = state_dict.get(core.job_entry_key(job_id))
        if not isinstance(rec, dict):
            return _err(404, "not_found", f"Unknown job {job_id!r}.")

        jobs_volume.reload()
        log_file = _log_path(job_id)
        data = log_file.read_bytes() if log_file.exists() else b""
        chunk = core.build_log_chunk(
            data,
            offset=offset,
            limit=limit,
            paused=False,
            stopped=rec.get("state") in core.TERMINAL_STATES,
        )
        return JSONResponse(status_code=200, content=chunk)

    @api.post("/v1/jobs/{job_id}/cancel")
    async def cancel_job(job_id: str) -> JSONResponse:
        key = core.job_entry_key(job_id)
        rec = state_dict.get(key)
        if not isinstance(rec, dict):
            return _err(404, "not_found", f"Unknown job {job_id!r}.")

        if rec.get("state") in core.ACTIVE_STATES:
            core.transition_job(rec, "cancelled", now=core.utc_now(), exit_code=137)
            _record_monthly_cost(job_id, rec)
            state_dict[key] = rec
            _cancel_backing_execution(rec)
            return JSONResponse(status_code=200, content={"cancelled": True, "exit": 137})

        return JSONResponse(
            status_code=200,
            content={"cancelled": rec.get("state") == "cancelled", "exit": rec.get("exit")},
        )

    @api.get("/v1/jobs/{job_id}/file")
    async def pull_file(job_id: str, path: str = Query(default="")) -> Response:
        rec = state_dict.get(core.job_entry_key(job_id))
        if not isinstance(rec, dict):
            return _err(404, "not_found", f"Unknown job {job_id!r}.")

        try:
            rel = core.sanitize_rel_path(path)
        except ValueError as exc:
            return _err(400, "invalid", str(exc))

        if rec.get("mode") != "sandbox":
            return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")

        keep_until = core.parse_rfc3339(rec.get("keepUntil"))
        if rec.get("state") in core.TERMINAL_STATES and (
            keep_until is None or core.utc_now() > keep_until
        ):
            return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")

        sb_id = rec.get("sandboxId")
        if not sb_id:
            return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")

        workdir = f"/work/{rec['repo']}"
        try:
            sb = modal.Sandbox.from_id(sb_id)
            check_py = (
                "import os, sys; "
                "root = os.path.realpath(sys.argv[1]); "
                "target = os.path.realpath(os.path.join(root, sys.argv[2])); "
                "sys.exit(0 if (target == root or target.startswith(root + os.sep)) and os.path.exists(target) else "
                "(3 if not os.path.exists(target) else 2))"
            )
            chk = sb.exec("python3", "-c", check_py, workdir, rel)
            rc = chk.wait()
            if rc == 2:
                return _err(400, "invalid", "path escapes workspace via symlink")
            if rc == 3:
                return _err(404, "not_found", f"Path {rel!r} not found in sandbox.")
            if rc != 0:
                return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")

            import base64

            tar_proc = sb.exec(
                "bash",
                "-lc",
                f"tar -cf - -C {shlex.quote(workdir)} {shlex.quote(rel)} | base64 -w0",
            )
            encoded = "".join(
                c.decode("ascii") if isinstance(c, bytes) else c for c in tar_proc.stdout
            )
            if tar_proc.wait() != 0:
                return _err(404, "not_found", f"Could not archive {rel!r} from sandbox.")
            tar_bytes = base64.b64decode(encoded)
            return Response(content=tar_bytes, status_code=200, media_type="application/x-tar")
        except HTTPException:
            raise
        except Exception:
            return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")

    @api.post("/v1/pause")
    async def set_pause(request: Request) -> JSONResponse:
        try:
            body = await request.json()
        except Exception:
            return _err(400, "invalid", "Request body must be JSON.")
        if not isinstance(body, dict) or not isinstance(body.get("paused"), bool):
            return _err(400, "invalid", "Request must include boolean 'paused'.")
        state_dict["paused"] = body["paused"]
        return JSONResponse(status_code=200, content={"paused": body["paused"]})

    @api.post("/v1/stop")
    async def emergency_stop() -> JSONResponse:
        state = core.snapshot_dict_state(state_dict)
        now = core.utc_now()
        stopped = 0
        for job_id, rec in state["jobs"].items():
            if rec.get("state") in core.ACTIVE_STATES:
                core.transition_job(rec, "cancelled", now=now, exit_code=137)
                _record_monthly_cost(job_id, rec)
                state_dict[core.job_entry_key(job_id)] = rec
                _cancel_backing_execution(rec)
                stopped += 1
        return JSONResponse(status_code=200, content={"stopped": stopped})

    @api.put("/v1/config")
    async def update_config(request: Request) -> JSONResponse:
        try:
            body = await request.json()
        except Exception:
            return _err(400, "invalid", "Request body must be JSON.")
        current = core.normalize_config(state_dict.get("config"))
        ok, status, err_code, msg, merged = core.validate_config_update(current, body)
        if not ok or merged is None:
            return _err(status, err_code, msg)
        state_dict["config"] = merged
        return JSONResponse(status_code=200, content=core.public_config(merged))

    return api
