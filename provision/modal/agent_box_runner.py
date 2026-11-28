"""Modal runner-v1 backend (`agent-box-runner`).

Exposes the HTTP+JSON endpoints defined in docs/runner-protocol.md via a
Modal ASGI web endpoint (`requires_proxy_auth=True`) backed by:
  - 1x Modal Functions per size (`small`, `medium`, `large`, `gpu`) with preemption retries
  - 3x Modal Sandboxes (opt-in `mode: "sandbox"`, isolated without `/jobs` volume mount)
  - `modal.Volume("agent-box-jobs")` for git bundles, streaming job logs, and monthly cost ledgers
  - `modal.Dict("agent-box-state")` for live job metadata, config, pause flag, and cancel tombstones
"""

import asyncio
import base64
import inspect
import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
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
COSTS_DIR = f"{JOBS_MOUNT}/_costs"
COMMIT_INTERVAL_SEC = 5.0
MAX_FUNCTION_TIMEOUT_SEC = 86400

# Toolchain versions mirror go.mod and provision/agent-base.yaml (built by provision/setup-host.sh).
GO_VERSION = "1.27.4"  # matches go.mod
NODE_MAJOR = "22"  # matches .github/workflows/test.yml and agent-base Node LTS
DEPLOYED_GPU_TYPE = os.environ.get("AGENT_BOX_GPU_TYPE", rates.DEFAULT_GPU_TYPE)
if DEPLOYED_GPU_TYPE not in rates.GPU_RATES_USD_PER_SEC:
    raise ValueError(f"Unsupported AGENT_BOX_GPU_TYPE {DEPLOYED_GPU_TYPE!r}")

app = modal.App(APP_NAME)
jobs_volume = modal.Volume.from_name(JOBS_VOLUME_NAME, create_if_missing=True)
state_dict = modal.Dict.from_name(STATE_DICT_NAME, create_if_missing=True)
_ADMISSION_LOCK = asyncio.Lock()

# Executed via `sb.exec("python3", "-c", ...)` to read raw binary log bytes across Modal's text stdout.
_SANDBOX_LOG_READ_SCRIPT = (
    "import base64, os, sys\n"
    "path, off = sys.argv[1], int(sys.argv[2])\n"
    "if not os.path.exists(path):\n"
    "    sys.exit(0)\n"
    "with open(path, 'rb') as f:\n"
    "    f.seek(off)\n"
    "    data = f.read(1048576)\n"
    "if data:\n"
    "    sys.stdout.write(base64.b64encode(data).decode('ascii'))\n"
)

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
        # Unprivileged user for Function mode so untrusted jobs cannot tamper with /jobs.
        "id -u agent >/dev/null 2>&1 || useradd -m -s /bin/bash agent",
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


def _record_monthly_cost_best_effort(job_id: str, job: dict[str, Any]) -> None:
    """Persist settled job spend to the Volume ledger and Dict cache without failing state changes."""
    cost_usd = float((job.get("cost") or {}).get("usd", 0.0))
    ts = core.parse_rfc3339(job.get("endedAt")) or core.utc_now()
    month = core.month_key(ts)
    try:
        core.write_cost_record(
            COSTS_DIR,
            month,
            job_id,
            cost_usd,
            settled_at=core.format_rfc3339(ts),
        )
        jobs_volume.commit()
    except Exception as exc:
        print(f"[box] warning: failed to commit cost ledger for {job_id}: {exc}", file=sys.stderr)
    try:
        state_dict[core.cost_entry_key(month, job_id)] = cost_usd
    except Exception as exc:
        print(f"[box] warning: failed to update Dict cost cache for {job_id}: {exc}", file=sys.stderr)


def _is_cancelled(job_id: str) -> bool:
    if state_dict.get(core.cancelled_entry_key(job_id)):
        return True
    rec = state_dict.get(core.job_entry_key(job_id))
    return isinstance(rec, dict) and rec.get("state") == "cancelled"


def _save_job_if_not_cancelled(job_id: str, rec: dict[str, Any]) -> bool:
    """Persist `rec` only if no cancellation tombstone exists; return False when aborted."""
    if state_dict.get(core.cancelled_entry_key(job_id)):
        return False
    state_dict[core.job_entry_key(job_id)] = rec
    return True


def _prepare_command(cmd: list[str]) -> list[str]:
    if len(cmd) == 1:
        return ["bash", "-lc", f"set -o pipefail; {cmd[0]}"]
    return list(cmd)


def _mark_job_lost(job_id: str, message: str, *, log_file: Path | None = None) -> None:
    """Persist `lost` status first so a Volume read/write failure can never strand an active job."""
    if state_dict.get(core.cancelled_entry_key(job_id)):
        return
    key = core.job_entry_key(job_id)
    rec = state_dict.get(key)
    if not isinstance(rec, dict) or rec.get("state") in core.TERMINAL_STATES:
        return
    log_bytes = rec.get("logBytes", 0)
    tail = rec.get("tail", "")
    if log_file is not None:
        try:
            if log_file.exists():
                log_bytes, tail = _read_tail_and_size(log_file)
        except Exception as exc:
            print(f"[box] warning: failed to read log in _mark_job_lost for {job_id}: {exc}", file=sys.stderr)
    core.transition_job(
        rec,
        "lost",
        now=core.utc_now(),
        log_bytes=log_bytes,
        tail=tail,
        error_message=message,
    )
    if _save_job_if_not_cancelled(job_id, rec):
        _record_monthly_cost_best_effort(job_id, rec)


def _restrict_jobs_mount() -> None:
    """Fail-closed enforcement that Function containers isolate /jobs from the `agent` user."""
    core.restrict_jobs_mount(JOBS_MOUNT)


def _execute_in_workspace(job_id: str, rec: dict[str, Any]) -> int:
    """Copy bundle to an isolated temp dir, drop privileges, and stream output to job.log."""
    log_file = _log_path(job_id)
    log_file.parent.mkdir(parents=True, exist_ok=True)
    volume_bundle = _bundle_path(job_id)

    with tempfile.TemporaryDirectory(prefix=f"agent-box-{job_id}-") as tmpdir:
        tmp_path = Path(tmpdir)
        isolated_bundle = tmp_path / "repo.bundle"
        shutil.copyfile(volume_bundle, isolated_bundle)

        work_parent = tmp_path / "work"
        workdir = work_parent / rec["repo"]
        workdir.mkdir(parents=True, exist_ok=True)

        euid = os.geteuid()
        if euid == 0:
            for p in (tmp_path, isolated_bundle, work_parent, workdir):
                shutil.chown(p, user="agent", group="agent")
            os.chmod(tmp_path, 0o700)
            os.chmod(isolated_bundle, 0o600)
            os.chmod(work_parent, 0o700)
            os.chmod(workdir, 0o700)

        clean_env = core.build_clean_env(os.environ, rec.get("env"))
        if euid == 0:
            user_env = rec.get("env") or {}
            clean_env["HOME"] = user_env.get("HOME", "/home/agent")
            clean_env["USER"] = "agent"
            clean_env["GOPATH"] = user_env.get("GOPATH", "/home/agent/go")

        popen_kwargs = core.build_subprocess_kwargs(euid)
        unpack_script = (
            f"git init -q {shlex.quote(str(workdir))} && "
            f"git -C {shlex.quote(str(workdir))} fetch -q {shlex.quote(str(isolated_bundle))} "
            f"{shlex.quote('refs/agent-box/' + rec['ref'])} && "
            f"git -C {shlex.quote(str(workdir))} checkout -q -f FETCH_HEAD"
        )
        unpack = subprocess.run(
            ["bash", "-lc", unpack_script],
            env=clean_env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            **popen_kwargs,
        )
        isolated_bundle.unlink(missing_ok=True)
        if unpack.returncode != 0:
            with log_file.open("ab") as fh:
                fh.write(f"[box] failed to unpack bundle: {unpack.stdout}\n".encode("utf-8", errors="replace"))
            jobs_volume.commit()
            return unpack.returncode or 1

        proc = subprocess.Popen(
            _prepare_command(rec["cmd"]),
            cwd=str(workdir),
            env=clean_env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            **popen_kwargs,
        )

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
                    _save_job_if_not_cancelled(job_id, cur)
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
    log_file = _log_path(job_id)
    completed = False
    try:
        _restrict_jobs_mount()
        jobs_volume.reload()
        if _is_cancelled(job_id):
            completed = True
            return

        key = core.job_entry_key(job_id)
        rec = state_dict.get(key)
        if not isinstance(rec, dict) or rec.get("state") in core.TERMINAL_STATES:
            completed = True
            return

        if rec.get("size") == "gpu":
            rec["gpuType"] = DEPLOYED_GPU_TYPE

        log_file.parent.mkdir(parents=True, exist_ok=True)
        if rec.get("state") == "running":
            attempt = int(rec.get("attempt", 1)) + 1
            rec["attempt"] = attempt
            with log_file.open("ab") as fh:
                fh.write(f"[box] restarted after preemption (attempt {attempt})\n".encode("utf-8"))
            jobs_volume.commit()
        else:
            core.transition_job(rec, "running", now=core.utc_now(), attempt=1)
        if not _save_job_if_not_cancelled(job_id, rec):
            completed = True
            return

        exit_code = _execute_in_workspace(job_id, rec)
        log_bytes, tail = _read_tail_and_size(log_file)

        if _is_cancelled(job_id):
            completed = True
            return
        latest = state_dict.get(key)
        if not isinstance(latest, dict) or latest.get("state") in core.TERMINAL_STATES:
            completed = True
            return
        if latest.get("size") == "gpu":
            latest["gpuType"] = DEPLOYED_GPU_TYPE

        final_state = "cancelled" if exit_code == 137 and _is_cancelled(job_id) else "done"
        core.transition_job(
            latest,
            final_state,
            now=core.utc_now(),
            exit_code=exit_code,
            log_bytes=log_bytes,
            tail=tail,
        )
        if _save_job_if_not_cancelled(job_id, latest):
            _record_monthly_cost_best_effort(job_id, latest)
        completed = True
    except Exception as exc:
        try:
            log_file.parent.mkdir(parents=True, exist_ok=True)
            with log_file.open("ab") as fh:
                fh.write(f"\n[box] worker error: {exc}\n".encode("utf-8", errors="replace"))
            jobs_volume.commit()
        except Exception:
            pass
        _mark_job_lost(job_id, str(exc), log_file=log_file)
        completed = True
        raise
    finally:
        if not completed:
            _mark_job_lost(job_id, "worker exited unexpectedly", log_file=log_file)


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
    gpu=DEPLOYED_GPU_TYPE,
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


def _upload_bundle_to_sandbox(sb: Any, bundle_bytes: bytes) -> None:
    """Stream bundle bytes into /tmp/repo.bundle inside the Sandbox, draining each chunk."""
    core.upload_bundle_to_sandbox(sb, bundle_bytes)


def _drain_sandbox_log(sb: Any, remote_log: str, local_log: Path, offset: int) -> int:
    """Read newly appended raw bytes from the sandbox log via base64 stdout (lossless UTF-8/binary)."""
    while True:
        try:
            proc = sb.exec("python3", "-c", _SANDBOX_LOG_READ_SCRIPT, remote_log, str(offset))
            encoded = "".join(c.decode("ascii") if isinstance(c, bytes) else c for c in proc.stdout)
            if proc.wait() != 0 or not encoded:
                return offset
            data = base64.b64decode(encoded)
        except Exception:
            return offset
        if not data:
            return offset
        with local_log.open("ab") as lf:
            lf.write(data)
        offset += len(data)
        if len(data) < 1048576:
            return offset


@app.function(
    image=base_image,
    cpu=0.25,
    memory=256,
    timeout=MAX_FUNCTION_TIMEOUT_SEC,
    volumes={JOBS_MOUNT: jobs_volume},
    retries=0,
)
def run_sandbox_job(job_id: str) -> None:
    """Supervise an isolated 3x Modal Sandbox (no /jobs mount) and retain for `keepSec`."""
    log_file = _log_path(job_id)
    log_file.parent.mkdir(parents=True, exist_ok=True)
    key = core.job_entry_key(job_id)
    sb = None
    completed = False

    try:
        jobs_volume.reload()
        if _is_cancelled(job_id):
            completed = True
            return

        rec = state_dict.get(key)
        if not isinstance(rec, dict) or rec.get("state") in core.TERMINAL_STATES:
            completed = True
            return

        cfg = core.normalize_config(state_dict.get("config"))
        keep_sec = int(cfg.get("keepSec", 0))
        timeout_sec = int(rec.get("timeoutSec", 3600))
        total_timeout = min(core.MAX_SANDBOX_LIFETIME_SEC, timeout_sec + keep_sec)
        size_spec = core.SIZES[rec["size"]]
        gpu_arg = rec.get("gpuType", DEPLOYED_GPU_TYPE) if size_spec.gpu else None

        sb_kwargs: dict[str, Any] = {
            "image": base_image,
            "cpu": size_spec.cpu_cores,
            "memory": size_spec.memory_mib,
            "timeout": total_timeout,
            "app": app,
        }
        if gpu_arg:
            sb_kwargs["gpu"] = gpu_arg

        # Never mount /jobs inside the Sandbox so untrusted code cannot access other jobs or _costs.
        sb = modal.Sandbox.create("sleep", str(total_timeout), **sb_kwargs)
        state_dict[core.sandbox_entry_key(job_id)] = sb.object_id
        rec["sandboxId"] = sb.object_id
        core.transition_job(rec, "running", now=core.utc_now(), attempt=1)
        if not _save_job_if_not_cancelled(job_id, rec):
            sb.terminate()
            completed = True
            return

        bundle_bytes = _bundle_path(job_id).read_bytes()
        _upload_bundle_to_sandbox(sb, bundle_bytes)

        workdir = f"/work/{rec['repo']}"
        remote_log = "/tmp/agent-box-job.log"
        cmd_args = _prepare_command(rec["cmd"])
        env_prefix = " ".join(
            f"{shlex.quote(k)}={shlex.quote(str(v))}" for k, v in (rec.get("env") or {}).items()
        )
        cmd_str = " ".join(shlex.quote(arg) for arg in cmd_args)
        if env_prefix:
            cmd_str = f"env {env_prefix} {cmd_str}"

        inner = (
            f"mkdir -p -m 0755 /work && "
            f"mkdir -p {shlex.quote(workdir)} && "
            f"git init -q {shlex.quote(workdir)} && "
            f"git -C {shlex.quote(workdir)} fetch -q /tmp/repo.bundle "
            f"{shlex.quote('refs/agent-box/' + rec['ref'])} && "
            f"git -C {shlex.quote(workdir)} checkout -q -f FETCH_HEAD && "
            f"rm -f /tmp/repo.bundle && "
            f": > {remote_log} && "
            f"cd {shlex.quote(workdir)} && ( {cmd_str} ) >{remote_log} 2>&1"
        )

        proc = sb.exec("bash", "-lc", inner, timeout=timeout_sec)
        remote_offset = 0
        last_commit = time.monotonic()
        start_mono = last_commit
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
            remote_offset = _drain_sandbox_log(sb, remote_log, log_file, remote_offset)
            if now_mono - last_commit >= COMMIT_INTERVAL_SEC:
                jobs_volume.commit()
                log_bytes, tail = _read_tail_and_size(log_file)
                cur = state_dict.get(key)
                if isinstance(cur, dict) and cur.get("state") == "running":
                    cur["logBytes"] = log_bytes
                    cur["tail"] = tail
                    _save_job_if_not_cancelled(job_id, cur)
                last_commit = now_mono
            time.sleep(1.0)

        if cancelled:
            sb.terminate()
            exit_code = 137
        elif timed_out:
            exit_code = 124
            remote_offset = _drain_sandbox_log(sb, remote_log, log_file, remote_offset)
            with log_file.open("ab") as fh:
                fh.write(f"\n[box] timed out after {timeout_sec}s\n".encode("utf-8"))
        else:
            exit_code = int(proc.wait())
            remote_offset = _drain_sandbox_log(sb, remote_log, log_file, remote_offset)
        jobs_volume.commit()

        log_bytes, tail = _read_tail_and_size(log_file)
        effective_keep = 0
        if not _is_cancelled(job_id):
            latest = state_dict.get(key)
            if isinstance(latest, dict) and latest.get("state") not in core.TERMINAL_STATES:
                final_state = "cancelled" if cancelled else "done"
                effective_keep = 0 if (cancelled or timed_out) else keep_sec
                core.transition_job(
                    latest,
                    final_state,
                    now=core.utc_now(),
                    exit_code=exit_code,
                    log_bytes=log_bytes,
                    tail=tail,
                    keep_sec=effective_keep,
                )
                if _save_job_if_not_cancelled(job_id, latest) and effective_keep == 0:
                    _record_monthly_cost_best_effort(job_id, latest)

        # Hold the sandbox for keepSec, then settle total sandbox lifetime cost before terminating.
        if effective_keep > 0 and not _is_cancelled(job_id):
            try:
                deadline = time.monotonic() + effective_keep
                while time.monotonic() < deadline:
                    if _is_cancelled(job_id):
                        break
                    time.sleep(min(5.0, max(0.5, deadline - time.monotonic())))
            finally:
                settled_at = core.utc_now()
                latest = state_dict.get(key)
                if isinstance(latest, dict) and not _is_cancelled(job_id):
                    started = core.parse_rfc3339(latest.get("startedAt"))
                    total_dur = max(0.0, (settled_at - started).total_seconds()) if started else 0.0
                    latest["cost"] = core.estimate_job_cost(
                        latest.get("size", "medium"),
                        total_dur,
                        "sandbox",
                        latest.get("gpuType", DEPLOYED_GPU_TYPE),
                    )
                    latest["keepUntil"] = core.format_rfc3339(settled_at)
                    if _save_job_if_not_cancelled(job_id, latest):
                        _record_monthly_cost_best_effort(
                            job_id,
                            {**latest, "endedAt": core.format_rfc3339(settled_at)},
                        )
        completed = True
    except Exception as exc:
        try:
            log_file.parent.mkdir(parents=True, exist_ok=True)
            with log_file.open("ab") as fh:
                fh.write(f"\n[box] sandbox error: {exc}\n".encode("utf-8", errors="replace"))
            jobs_volume.commit()
        except Exception:
            pass
        _mark_job_lost(job_id, str(exc), log_file=log_file)
        completed = True
        raise
    finally:
        if sb is not None:
            try:
                sb.terminate()
            except Exception:
                pass
        if not completed:
            _mark_job_lost(job_id, "sandbox supervisor exited unexpectedly", log_file=log_file)


def _cancel_backing_execution(job_id: str, rec: dict[str, Any]) -> None:
    sb_id = state_dict.get(core.sandbox_entry_key(job_id)) or rec.get("sandboxId")
    if sb_id:
        try:
            modal.Sandbox.from_id(sb_id).terminate()
        except Exception:
            pass
    call_id = state_dict.get(core.call_entry_key(job_id))
    if call_id:
        try:
            modal.FunctionCall.from_id(call_id).cancel()
        except Exception:
            pass


class ModalBackend:
    """Modal-backed storage and job execution adapter for app_factory."""

    def load_state(self) -> dict[str, Any]:
        try:
            jobs_volume.reload()
            costs_root: str | None = COSTS_DIR
        except Exception as exc:
            print(f"[box] warning: Volume reload failed during load_state: {exc}", file=sys.stderr)
            costs_root = COSTS_DIR if os.path.isdir(COSTS_DIR) else None
        return core.snapshot_dict_state(state_dict, costs_root=costs_root)

    def get_job(self, job_id: str) -> dict[str, Any] | None:
        rec = state_dict.get(core.job_entry_key(job_id))
        if not isinstance(rec, dict):
            return None
        cancelled = bool(state_dict.get(core.cancelled_entry_key(job_id)))
        return core.apply_cancellation_tombstone(rec, cancelled)

    def save_job(self, job_id: str, rec: dict[str, Any]) -> None:
        state_dict[core.job_entry_key(job_id)] = rec

    def set_paused(self, paused: bool) -> None:
        state_dict["paused"] = paused

    def get_config(self) -> dict[str, Any]:
        return core.normalize_config(state_dict.get("config"))

    def set_config(self, cfg: dict[str, Any]) -> None:
        state_dict["config"] = cfg

    def write_bundle_and_spawn(self, norm: dict[str, Any], bundle_bytes: bytes) -> None:
        job_id = norm["id"]
        bundle_file = _bundle_path(job_id)
        bundle_file.parent.mkdir(parents=True, exist_ok=True)
        bundle_file.write_bytes(bundle_bytes)
        jobs_volume.commit()

        rec = core.new_job_record(norm)
        state_dict[core.job_entry_key(job_id)] = rec
        try:
            if norm["mode"] == "sandbox":
                call = run_sandbox_job.spawn(job_id)
            else:
                call = FUNCTION_BY_SIZE[norm["size"]].spawn(job_id)
            state_dict[core.call_entry_key(job_id)] = call.object_id
        except Exception as exc:
            _mark_job_lost(job_id, f"spawn failed: {exc}")
            raise

    def read_log(self, job_id: str) -> bytes:
        jobs_volume.reload()
        log_file = _log_path(job_id)
        return log_file.read_bytes() if log_file.exists() else b""

    def cancel_job(self, job_id: str, rec: dict[str, Any]) -> None:
        # Write tombstone + cancelled state and terminate backing compute BEFORE best-effort Volume spend.
        state_dict[core.cancelled_entry_key(job_id)] = True
        core.transition_job(rec, "cancelled", now=core.utc_now(), exit_code=137)
        state_dict[core.job_entry_key(job_id)] = rec
        _cancel_backing_execution(job_id, rec)
        _record_monthly_cost_best_effort(job_id, rec)

    def pull_sandbox_tar(self, rec: dict[str, Any], rel: str) -> tuple[int, str, bytes]:
        sb_id = state_dict.get(core.sandbox_entry_key(rec["id"])) or rec.get("sandboxId")
        if not sb_id:
            return 409, "gone", b""
        sb = modal.Sandbox.from_id(sb_id)
        proc = sb.exec("python3", "-c", core.SANDBOX_TAR_SCRIPT, "/work", rec["repo"], rel)
        encoded = "".join(c.decode("ascii") if isinstance(c, bytes) else c for c in proc.stdout)
        rc = proc.wait()
        if rc == 2:
            return 400, "invalid", b""
        if rc == 3:
            return 404, "not_found", b""
        if rc != 0:
            return 409, "gone", b""
        return 200, "", base64.b64decode(encoded)


def app_factory(backend: Any = None, admission_lock: asyncio.Lock | None = None):
    """Build the runner-v1 FastAPI ASGI application cleanly without forward-ref annotation pitfalls."""
    from fastapi import FastAPI, Request
    from fastapi.exceptions import RequestValidationError
    from fastapi.responses import JSONResponse, Response

    svc = backend if backend is not None else ModalBackend()
    lock = admission_lock if admission_lock is not None else _ADMISSION_LOCK
    api = FastAPI(title="agent-box-runner", version="1")

    def _err(status: int, code: str, message: str) -> JSONResponse:
        return JSONResponse(status_code=status, content=core.error_envelope(code, message))

    @api.exception_handler(RequestValidationError)
    async def validation_error_handler(request: Request, exc: RequestValidationError) -> JSONResponse:
        del request
        return _err(400, "invalid", f"Invalid request: {exc.errors()[0].get('msg', 'malformed parameters')}")

    @api.get("/v1/probe")
    async def probe() -> JSONResponse:
        state = svc.load_state()
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

        async with lock:
            state = svc.load_state()
            existing = core.find_existing_job(
                {core.job_entry_key(k): v for k, v in state["jobs"].items()}, spec_obj
            )
            if existing is not None:
                return JSONResponse(status_code=200, content=core.build_job_response(dict(existing), state))

            ok, status, err_code, msg, norm = core.validate_spec(
                spec_obj, state["config"], deployed_gpu_type=DEPLOYED_GPU_TYPE
            )
            if not ok or norm is None:
                return _err(status, err_code, msg)

            ok, status, err_code, msg = core.check_admission(state, norm)
            if not ok:
                return _err(status, err_code, msg)

            try:
                svc.write_bundle_and_spawn(norm, bundle_bytes)
            except Exception as exc:
                return _err(500, "spawn_failed", f"Failed to schedule job: {exc}")

        return JSONResponse(status_code=201, content={"id": norm["id"], "state": "queued"})

    @api.get("/v1/jobs")
    async def list_jobs(request: Request) -> JSONResponse:
        try:
            clamped = core.parse_int_query(
                request.query_params.get("limit"),
                "limit",
                core.DEFAULT_LIST_LIMIT,
                min_value=1,
                max_value=core.MAX_LIST_LIMIT,
            )
        except ValueError as exc:
            return _err(400, "invalid", str(exc))

        state = svc.load_state()
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
    async def get_job(request: Request, job_id: str) -> JSONResponse:
        try:
            wait_sec = core.parse_int_query(
                request.query_params.get("wait"),
                "wait",
                0,
                min_value=0,
                max_value=core.MAX_POLL_WAIT_SEC,
            )
        except ValueError as exc:
            return _err(400, "invalid", str(exc))

        rec = svc.get_job(job_id)
        if rec is None:
            return _err(404, "not_found", f"Unknown job {job_id!r}.")

        initial_state = rec.get("state")
        if wait_sec > 0 and initial_state in core.ACTIVE_STATES:
            deadline = time.monotonic() + wait_sec
            while time.monotonic() < deadline:
                await asyncio.sleep(0.5)
                latest = svc.get_job(job_id)
                if latest is None:
                    break
                rec = latest
                cur_state = rec.get("state")
                if cur_state in core.TERMINAL_STATES or cur_state != initial_state:
                    break

        state = svc.load_state()
        return JSONResponse(status_code=200, content=core.build_job_response(rec, state))

    @api.get("/v1/jobs/{job_id}/log")
    async def get_job_log(request: Request, job_id: str) -> JSONResponse:
        try:
            offset = core.parse_int_query(request.query_params.get("offset"), "offset", 0, min_value=0)
            limit = core.parse_int_query(
                request.query_params.get("limit"),
                "limit",
                core.MAX_LOG_CHUNK,
                min_value=1,
                max_value=core.MAX_LOG_CHUNK,
            )
        except ValueError as exc:
            return _err(400, "invalid", str(exc))

        rec = svc.get_job(job_id)
        if rec is None:
            return _err(404, "not_found", f"Unknown job {job_id!r}.")

        data = svc.read_log(job_id)
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
        rec = svc.get_job(job_id)
        if rec is None:
            return _err(404, "not_found", f"Unknown job {job_id!r}.")

        if rec.get("state") in core.ACTIVE_STATES:
            svc.cancel_job(job_id, rec)
            return JSONResponse(status_code=200, content={"cancelled": True, "exit": 137})

        return JSONResponse(
            status_code=200,
            content={"cancelled": rec.get("state") == "cancelled", "exit": rec.get("exit")},
        )

    @api.get("/v1/jobs/{job_id}/file")
    async def pull_file(request: Request, job_id: str) -> Response:
        rec = svc.get_job(job_id)
        if rec is None:
            return _err(404, "not_found", f"Unknown job {job_id!r}.")

        try:
            rel = core.sanitize_rel_path(request.query_params.get("path", ""))
        except ValueError as exc:
            return _err(400, "invalid", str(exc))

        if rec.get("mode") != "sandbox" or rec.get("state") != "done":
            return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")

        keep_until = core.parse_rfc3339(rec.get("keepUntil"))
        if keep_until is None or core.utc_now() > keep_until:
            return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")

        try:
            status, err_code, tar_bytes = svc.pull_sandbox_tar(rec, rel)
        except Exception:
            return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")

        if status == 400:
            return _err(400, err_code, "path escapes workspace via symlink")
        if status == 404:
            return _err(404, err_code, f"Path {rel!r} not found in sandbox.")
        if status != 200:
            return _err(409, "gone", "Sandbox is no longer kept; files are unavailable.")
        return Response(content=tar_bytes, status_code=200, media_type="application/x-tar")

    @api.post("/v1/pause")
    async def set_pause(request: Request) -> JSONResponse:
        try:
            body = await request.json()
        except Exception:
            return _err(400, "invalid", "Request body must be JSON.")
        if not isinstance(body, dict) or not isinstance(body.get("paused"), bool):
            return _err(400, "invalid", "Request must include boolean 'paused'.")
        svc.set_paused(body["paused"])
        return JSONResponse(status_code=200, content={"paused": body["paused"]})

    @api.post("/v1/stop")
    async def emergency_stop() -> JSONResponse:
        state = svc.load_state()
        stopped = 0
        for job_id, rec in state["jobs"].items():
            if rec.get("state") in core.ACTIVE_STATES:
                svc.cancel_job(job_id, rec)
                stopped += 1
        return JSONResponse(status_code=200, content={"stopped": stopped})

    @api.put("/v1/config")
    async def update_config(request: Request) -> JSONResponse:
        try:
            body = await request.json()
        except Exception:
            return _err(400, "invalid", "Request body must be JSON.")
        current = svc.get_config()
        ok, status, err_code, msg, merged = core.validate_config_update(current, body)
        if not ok or merged is None:
            return _err(status, err_code, msg)
        svc.set_config(merged)
        return JSONResponse(status_code=200, content=core.public_config(merged))

    return api


# Single-container web endpoint serializes admission checks alongside _ADMISSION_LOCK.
@app.function(
    image=base_image,
    cpu=0.5,
    memory=512,
    min_containers=0,
    max_containers=1,
    volumes={JOBS_MOUNT: jobs_volume},
)
@modal.asgi_app(requires_proxy_auth=True)
def web_app():
    return app_factory()
