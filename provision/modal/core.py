"""Pure-stdlib runner-v1 protocol logic, validation, sizing, and cost math.

Zero third-party or `modal` imports so unit tests run anywhere with `python3 -m unittest`.
Matches docs/runner-protocol.md, docs/protocol/runner-v1/*.json, and internal/mode/size.go.
"""

from __future__ import annotations

import base64
from dataclasses import dataclass
from datetime import datetime, timezone
import math
import os
from pathlib import PurePosixPath, PureWindowsPath
import re
from typing import Any, Mapping, MutableMapping

from rates import (
    CPU_CORE_SEC_USD,
    DEFAULT_GPU_TYPE,
    GPU_RATES_USD_PER_SEC,
    MEM_GIB_SEC_USD,
    SANDBOX_CPU_MEM_MULTIPLIER,
)

PROTOCOL_VERSION = 1
TAIL_BYTES = 2048  # dash.TailBytes (2 KiB)
MAX_LOG_CHUNK = 65536  # dash.MaxLogChunk (64 KiB)
MAX_POLL_WAIT_SEC = 25
DEFAULT_LIST_LIMIT = 20
MAX_LIST_LIMIT = 200

MODES = ("function", "sandbox")
ACTIVE_STATES = frozenset({"queued", "running"})
TERMINAL_STATES = frozenset({"done", "cancelled", "lost"})
ALL_STATES = ACTIVE_STATES | TERMINAL_STATES

# Exact regexes matching cmd/box/box.go (validBox, validJob) and internal/dash/types.go.
VALID_BOX_RE = re.compile(r"^[a-z][a-z0-9-]{0,62}$")
VALID_REF_RE = re.compile(r"^[0-9]{8}-[0-9]{6}-[0-9a-f]+$")
VALID_JOB_ID_RE = re.compile(r"^[a-z][a-z0-9-]*\.[0-9]{8}-[0-9]{6}-[0-9a-f]+$")
VALID_HEX_OID_RE = re.compile(r"^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$")

DEFAULT_CONFIG: dict[str, Any] = {
    "capUSD": 25.0,
    "creditUSD": 30.0,
    "warnAt": 0.75,
    "maxConcurrent": 4,
    "maxTimeoutSec": 3600,
    "keepSec": 0,
    "mode": "function",
    "gpuType": DEFAULT_GPU_TYPE,
}

MUTABLE_CONFIG_KEYS = frozenset(
    {"capUSD", "creditUSD", "warnAt", "maxConcurrent", "maxTimeoutSec", "keepSec", "mode", "gpuType"}
)
CONFIG_RESPONSE_KEYS = ("capUSD", "maxConcurrent", "maxTimeoutSec", "keepSec", "mode")


@dataclass(frozen=True)
class SizeSpec:
    """Resource spec mirroring internal/mode/size.go (FullCPUs -> Modal physical cores)."""

    name: str
    vcpus: int
    mem_gib: int
    disk_gib: int
    gpu: bool = False

    @property
    def cpu_cores(self) -> float:
        """Modal allocates physical cores where 1 physical core = 2 vCPUs."""
        return self.vcpus / 2.0

    @property
    def memory_mib(self) -> int:
        return self.mem_gib * 1024


# Mirrors internal/mode/size.go FullCPUs (2, 4, 8, 6 vCPUs -> 1.0, 2.0, 4.0, 3.0 physical cores).
SIZES: dict[str, SizeSpec] = {
    "small": SizeSpec(name="small", vcpus=2, mem_gib=2, disk_gib=18, gpu=False),
    "medium": SizeSpec(name="medium", vcpus=4, mem_gib=4, disk_gib=30, gpu=False),
    "large": SizeSpec(name="large", vcpus=8, mem_gib=8, disk_gib=40, gpu=False),
    "gpu": SizeSpec(name="gpu", vcpus=6, mem_gib=8, disk_gib=40, gpu=True),
}
SIZE_ORDER = ("small", "medium", "large", "gpu")


def utc_now(now: datetime | None = None) -> datetime:
    """Return an aware UTC datetime."""
    if now is None:
        return datetime.now(timezone.utc)
    if now.tzinfo is None:
        return now.replace(tzinfo=timezone.utc)
    return now.astimezone(timezone.utc)


def format_rfc3339(dt: datetime) -> str:
    """Format a datetime as RFC 3339 UTC with Z suffix."""
    return utc_now(dt).strftime("%Y-%m-%dT%H:%M:%SZ")


def parse_rfc3339(value: str | None) -> datetime | None:
    """Parse an RFC 3339 timestamp string into an aware UTC datetime."""
    if not value or not isinstance(value, str):
        return None
    raw = value[:-1] + "+00:00" if value.endswith("Z") else value
    try:
        dt = datetime.fromisoformat(raw)
    except ValueError:
        return None
    return utc_now(dt)


def month_key(now: datetime | None = None) -> str:
    """Return YYYY-MM UTC month key."""
    return utc_now(now).strftime("%Y-%m")


def cost_entry_key(month: str, job_id: str) -> str:
    """Return the Dict key for one job's settled monthly spend."""
    return f"cost:{month}:{job_id}"


def job_entry_key(job_id: str) -> str:
    """Return the Dict key for one job record."""
    return f"job:{job_id}"


def error_envelope(code: str, message: str) -> dict[str, str]:
    """Build a uniform runner-v1 error response payload."""
    return {"error": code, "message": message}


def valid_box_name(name: Any) -> bool:
    """Check whether name matches cmd/box validBox."""
    return isinstance(name, str) and bool(VALID_BOX_RE.match(name))


def valid_ref(ref: Any) -> bool:
    """Check whether ref matches YYYYMMDD-HHMMSS-<hex>."""
    return isinstance(ref, str) and bool(VALID_REF_RE.match(ref))


def valid_job_id(job_id: Any) -> bool:
    """Check whether job_id matches cmd/box validJob."""
    return isinstance(job_id, str) and bool(VALID_JOB_ID_RE.match(job_id))


def _is_int(value: Any) -> bool:
    return isinstance(value, int) and not isinstance(value, bool)


def _is_finite_number(value: Any) -> bool:
    return isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value)


def normalize_config(raw: Mapping[str, Any] | None = None) -> dict[str, Any]:
    """Merge partial/stored config with DEFAULT_CONFIG."""
    cfg = dict(DEFAULT_CONFIG)
    if raw:
        for k, v in raw.items():
            if k in cfg and v is not None:
                cfg[k] = v
    return cfg


def public_config(config: Mapping[str, Any]) -> dict[str, Any]:
    """Return the mutable runner-v1 config view matching config.json."""
    cfg = normalize_config(config)
    return {k: cfg[k] for k in CONFIG_RESPONSE_KEYS}


def validate_config_update(
    current: Mapping[str, Any], patch: Any
) -> tuple[bool, int, str, str, dict[str, Any] | None]:
    """Validate PUT /v1/config payload and return (ok, status, err_code, message, merged_config)."""
    if not isinstance(patch, Mapping):
        return False, 400, "invalid", "Config body must be a JSON object.", None

    unknown = sorted(k for k in patch if k not in MUTABLE_CONFIG_KEYS)
    if unknown:
        return False, 400, "invalid", f"Unknown config key(s): {', '.join(unknown)}.", None

    merged = normalize_config(current)
    for key, val in patch.items():
        if key in ("capUSD", "creditUSD"):
            if not _is_finite_number(val) or val < 0:
                return False, 400, "invalid", f"{key} must be a non-negative number.", None
            merged[key] = float(val)
        elif key == "warnAt":
            if not _is_finite_number(val) or val < 0 or val > 1:
                return False, 400, "invalid", "warnAt must be between 0 and 1.", None
            merged[key] = float(val)
        elif key == "maxConcurrent":
            if not _is_int(val) or val < 1 or val > 100:
                return False, 400, "invalid", "maxConcurrent must be an integer between 1 and 100.", None
            merged[key] = val
        elif key == "maxTimeoutSec":
            if not _is_int(val) or val < 1 or val > 86400:
                return False, 400, "invalid", "maxTimeoutSec must be an integer between 1 and 86400.", None
            merged[key] = val
        elif key == "keepSec":
            if not _is_int(val) or val < 0 or val > 86400:
                return False, 400, "invalid", "keepSec must be an integer between 0 and 86400.", None
            merged[key] = val
        elif key == "mode":
            if val not in MODES:
                return False, 400, "invalid", "mode must be 'function' or 'sandbox'.", None
            merged[key] = val
        elif key == "gpuType":
            if not isinstance(val, str) or val not in GPU_RATES_USD_PER_SEC:
                return (
                    False,
                    400,
                    "invalid",
                    f"Unsupported gpuType {val!r}; expected one of {', '.join(GPU_RATES_USD_PER_SEC)}.",
                    None,
                )
            merged[key] = val

    return True, 200, "", "", merged


def _valid_source_revision(source: Any) -> bool:
    if not isinstance(source, Mapping):
        return False
    snap = source.get("snapshotCommit")
    commit = source.get("commit", "")
    dirty = source.get("dirty")
    if not isinstance(snap, str) or not VALID_HEX_OID_RE.match(snap):
        return False
    if not isinstance(commit, str):
        return False
    if commit == "":
        return dirty is None
    if not VALID_HEX_OID_RE.match(commit):
        return False
    return dirty is None or isinstance(dirty, bool)


def validate_spec(
    spec: Any, config: Mapping[str, Any] | None = None
) -> tuple[bool, int, str, str, dict[str, Any] | None]:
    """Validate a runner-v1 job spec and return normalized fields.

    Returns (ok, status_code, error_code, message, normalized_spec).
    """
    cfg = normalize_config(config)
    if not isinstance(spec, Mapping):
        return False, 400, "invalid", "Job spec must be a JSON object.", None

    job_id = spec.get("id")
    repo = spec.get("repo")
    ref = spec.get("ref")
    cmd = spec.get("cmd")
    size = spec.get("size")

    if not valid_job_id(job_id):
        return False, 400, "invalid", "Invalid or missing job id; expected <repo>.<YYYYMMDD-HHMMSS-hex>.", None
    if not valid_box_name(repo):
        return False, 400, "invalid", "Invalid or missing repo name.", None
    if not valid_ref(ref):
        return False, 400, "invalid", "Invalid or missing bundle ref.", None
    if job_id != f"{repo}.{ref}":
        return False, 400, "invalid", "Job id must match <repo>.<ref>.", None

    if not isinstance(cmd, list) or len(cmd) == 0 or not all(isinstance(c, str) for c in cmd):
        return False, 400, "invalid", "cmd must be a non-empty list of strings.", None
    if cmd[0] == "":
        return False, 400, "invalid", "cmd[0] must not be empty.", None

    if not isinstance(size, str) or size not in SIZES:
        return (
            False,
            400,
            "invalid",
            f"Invalid size {size!r}; expected one of {', '.join(SIZE_ORDER)}.",
            None,
        )

    raw_env = spec.get("env")
    if raw_env is None:
        env: dict[str, str] = {}
    elif isinstance(raw_env, Mapping) and all(
        isinstance(k, str) and k != "" and isinstance(v, str) for k, v in raw_env.items()
    ):
        env = dict(raw_env)
    else:
        return False, 400, "invalid", "env must be a string-to-string map.", None

    max_timeout = int(cfg["maxTimeoutSec"])
    raw_timeout = spec.get("timeoutSec", 0)
    if raw_timeout is None:
        raw_timeout = 0
    if not _is_int(raw_timeout) or raw_timeout < 0:
        return False, 400, "invalid", "timeoutSec must be a non-negative integer.", None
    if raw_timeout > max_timeout:
        return (
            False,
            400,
            "timeout_cap",
            f"Requested timeoutSec ({raw_timeout}) exceeds runner maxTimeoutSec ({max_timeout}).",
            None,
        )
    timeout_sec = max_timeout if raw_timeout == 0 else raw_timeout

    reason = spec.get("reason", "")
    if reason is None:
        reason = ""
    if not isinstance(reason, str):
        return False, 400, "invalid", "reason must be a string.", None

    source = spec.get("source")
    if source is not None and not _valid_source_revision(source):
        return False, 400, "invalid", "source must be a valid SourceRevision.", None

    mode = spec.get("mode")
    if mode is None:
        mode = cfg["mode"]
    elif mode not in MODES:
        return False, 400, "invalid", "mode must be 'function' or 'sandbox'.", None

    gpu_type = spec.get("gpuType")
    if gpu_type is None:
        gpu_type = cfg.get("gpuType", DEFAULT_GPU_TYPE)
    elif not isinstance(gpu_type, str) or gpu_type not in GPU_RATES_USD_PER_SEC:
        return (
            False,
            400,
            "invalid",
            f"Unsupported gpuType {gpu_type!r}; expected one of {', '.join(GPU_RATES_USD_PER_SEC)}.",
            None,
        )

    normalized: dict[str, Any] = {
        "id": job_id,
        "repo": repo,
        "ref": ref,
        "cmd": list(cmd),
        "env": env,
        "timeoutSec": timeout_sec,
        "size": size,
        "reason": reason,
        "mode": mode,
        "gpuType": gpu_type,
    }
    if source is not None:
        normalized["source"] = {
            "commit": source["commit"],
            "dirty": source["dirty"],
            "snapshotCommit": source["snapshotCommit"],
        }
    return True, 200, "", "", normalized


def estimate_job_cost(
    size: str,
    duration_sec: float,
    mode: str = "function",
    gpu_type: str = DEFAULT_GPU_TYPE,
) -> dict[str, float | int]:
    """Calculate Modal compute cost for a job run.

    1x Function: cpu_cores * sec * cpu_rate + mem_gib * sec * mem_rate (+ gpu_rate * sec)
    3x Sandbox: CPU and memory rates are multiplied by SANDBOX_CPU_MEM_MULTIPLIER (3.0); GPU stays 1x.
    """
    if size not in SIZES:
        raise ValueError(f"unknown size {size!r}")
    if mode not in MODES:
        raise ValueError(f"unknown mode {mode!r}")
    if gpu_type not in GPU_RATES_USD_PER_SEC:
        raise ValueError(f"unknown gpu_type {gpu_type!r}")

    sec = max(0.0, float(duration_sec))
    spec = SIZES[size]
    mult = SANDBOX_CPU_MEM_MULTIPLIER if mode == "sandbox" else 1.0

    cpu_cost = spec.cpu_cores * sec * CPU_CORE_SEC_USD * mult
    mem_cost = spec.mem_gib * sec * MEM_GIB_SEC_USD * mult
    gpu_cost = GPU_RATES_USD_PER_SEC[gpu_type] * sec if spec.gpu else 0.0

    total_usd = round(cpu_cost + mem_cost + gpu_cost, 6)
    cpu_sec = round(spec.cpu_cores * sec, 3)
    gpu_sec: float | int = round(sec, 3) if spec.gpu else 0
    return {
        "usd": total_usd,
        "cpuSec": cpu_sec,
        "gpuSec": gpu_sec,
    }


def _running_elapsed_sec(job: Mapping[str, Any], now: datetime) -> float:
    started = parse_rfc3339(job.get("startedAt"))
    if started is None:
        return 0.0
    return max(0.0, (utc_now(now) - started).total_seconds())


def compute_budget(state: Mapping[str, Any], now: datetime | None = None) -> dict[str, Any]:
    """Compute the monthly budget snapshot from state (settled costs + active running spend)."""
    ts = utc_now(now)
    cfg = normalize_config(state.get("config"))
    month = month_key(ts)

    if "spentUSD" in state and state["spentUSD"] is not None:
        spent_usd = float(state["spentUSD"])
    else:
        costs = state.get("costs") or {}
        prefix = f"cost:{month}:"
        spent_usd = sum(
            float(v)
            for k, v in costs.items()
            if (k == month or k.startswith(prefix) or ":" not in k)
        )

    if "runningUSD" in state and state["runningUSD"] is not None:
        running_usd = float(state["runningUSD"])
    else:
        jobs = state.get("jobs") or {}
        job_iter = jobs.values() if isinstance(jobs, Mapping) else jobs
        running_usd = 0.0
        for job in job_iter:
            if job.get("state") == "running":
                est = estimate_job_cost(
                    job.get("size", "medium"),
                    _running_elapsed_sec(job, ts),
                    job.get("mode", "function"),
                    job.get("gpuType", cfg.get("gpuType", DEFAULT_GPU_TYPE)),
                )
                running_usd += float(est["usd"])

    return {
        "month": month,
        "spentUSD": round(spent_usd, 6),
        "runningUSD": round(running_usd, 6),
        "capUSD": cfg["capUSD"],
        "creditUSD": cfg["creditUSD"],
        "warnAt": cfg["warnAt"],
    }


def count_active_jobs(state: Mapping[str, Any]) -> int:
    """Count queued and running jobs in state."""
    if "running" in state and state["running"] is not None:
        return int(state["running"])
    jobs = state.get("jobs") or {}
    job_iter = jobs.values() if isinstance(jobs, Mapping) else jobs
    return sum(1 for j in job_iter if j.get("state") in ACTIVE_STATES)


def check_admission(
    state: Mapping[str, Any],
    spec: Mapping[str, Any],
    now: datetime | None = None,
) -> tuple[bool, int, str, str]:
    """Enforce runner-v1 pause, monthly spend cap, and concurrency limits.

    Returns (ok, status_code, error_code, message).
    """
    del spec  # Reserved for per-size or GPU admission checks if needed later.
    cfg = normalize_config(state.get("config"))
    if bool(state.get("paused", False)):
        return False, 423, "paused", "Runner is paused; not accepting new jobs."

    budget = compute_budget(state, now)
    cap_usd = float(cfg["capUSD"])
    if budget["spentUSD"] + budget["runningUSD"] >= cap_usd:
        return False, 402, "budget", f"Monthly spend cap (${cap_usd:.2f}) reached."

    active = count_active_jobs(state)
    max_conc = int(cfg["maxConcurrent"])
    if active >= max_conc:
        return False, 409, "busy", f"Runner is at maxConcurrent ({active}/{max_conc})."

    return True, 200, "", ""


def build_probe_response(state: Mapping[str, Any], now: datetime | None = None) -> dict[str, Any]:
    """Build GET /v1/probe response matching docs/protocol/runner-v1/probe.json."""
    cfg = normalize_config(state.get("config"))
    return {
        "protocol": PROTOCOL_VERSION,
        "sizes": list(SIZE_ORDER),
        "running": count_active_jobs(state),
        "maxConcurrent": int(cfg["maxConcurrent"]),
        "paused": bool(state.get("paused", False)),
        "maxTimeoutSec": int(cfg["maxTimeoutSec"]),
        "budget": compute_budget(state, now),
    }


def compute_tail(data: bytes, limit: int = TAIL_BYTES) -> str:
    """Decode the trailing `limit` bytes of log output cleanly as UTF-8."""
    if not data:
        return ""
    slice_bytes = data[-limit:]
    if len(data) > limit:
        # Skip up to 3 leading UTF-8 continuation bytes (10xxxxxx) after truncation.
        skip = 0
        while skip < min(3, len(slice_bytes)) and (slice_bytes[skip] & 0xC0) == 0x80:
            skip += 1
        if skip:
            slice_bytes = slice_bytes[skip:]
    return slice_bytes.decode("utf-8", errors="replace")


def build_job_response(
    job: Mapping[str, Any],
    state: Mapping[str, Any],
    now: datetime | None = None,
) -> dict[str, Any]:
    """Build a runner-v1 Job JSON object matching job-done.json / job-list.json."""
    ts = utc_now(now)
    cfg = normalize_config(state.get("config"))
    mode = job.get("mode", cfg["mode"])
    size = job.get("size", "medium")
    gpu_type = job.get("gpuType", cfg.get("gpuType", DEFAULT_GPU_TYPE))

    if job.get("state") == "running":
        cost = estimate_job_cost(size, _running_elapsed_sec(job, ts), mode, gpu_type)
    elif job.get("cost") is not None:
        cost = dict(job["cost"])
    else:
        cost = {"usd": 0.0, "cpuSec": 0.0, "gpuSec": 0}

    return {
        "id": job["id"],
        "state": job["state"],
        "exit": job.get("exit"),
        "attempt": int(job.get("attempt", 1)),
        "startedAt": job.get("startedAt"),
        "endedAt": job.get("endedAt"),
        "logBytes": int(job.get("logBytes", 0)),
        "tail": job.get("tail", ""),
        "mode": mode,
        "cost": cost,
        "budget": compute_budget(state, ts),
    }


def build_log_chunk(
    data: bytes | str,
    offset: int = 0,
    limit: int = MAX_LOG_CHUNK,
    *,
    paused: bool = False,
    stopped: bool = False,
) -> dict[str, Any]:
    """Build a dash.LogChunk JSON object matching docs/protocol/runner-v1/log-chunk.json."""
    raw = data.encode("utf-8") if isinstance(data, str) else bytes(data)
    if offset < 0:
        raise ValueError("offset must be non-negative")
    clamped_limit = max(1, min(int(limit), MAX_LOG_CHUNK))
    total = len(raw)
    chunk = raw[offset : offset + clamped_limit]
    next_offset = offset + len(chunk)

    try:
        text = chunk.decode("utf-8", errors="strict")
        valid_utf8 = True
    except UnicodeDecodeError:
        text = chunk.decode("utf-8", errors="replace")
        valid_utf8 = False

    payload: dict[str, Any] = {
        "offset": next_offset,
        "data": text,
        "eof": next_offset >= total,
        "paused": bool(paused),
    }
    if stopped:
        payload["stopped"] = True
    if total > 0:
        payload["logBytes"] = total
    if not valid_utf8:
        payload["dataBase64"] = base64.b64encode(chunk).decode("ascii")
    return payload


def sanitize_rel_path(rel_path: Any, root: str | os.PathLike[str] | None = None) -> str:
    """Validate and normalize a relative path for /v1/jobs/{id}/file?path=rel.

    Rejects empty paths, NUL bytes, backslashes, absolute paths, Windows drive paths,
    `..` traversal segments, and (when `root` is provided) symlink escapes outside `root`.
    """
    if not isinstance(rel_path, str) or not rel_path:
        raise ValueError("path must be a non-empty relative path")
    if "\x00" in rel_path:
        raise ValueError("path must not contain NUL bytes")
    if "\\" in rel_path:
        raise ValueError("path must use POSIX forward slashes")

    posix = PurePosixPath(rel_path)
    win = PureWindowsPath(rel_path)
    if posix.is_absolute() or win.is_absolute() or win.drive:
        raise ValueError("absolute paths are not allowed")

    clean_parts: list[str] = []
    for part in rel_path.split("/"):
        if part in ("", "."):
            continue
        if part == "..":
            raise ValueError("parent traversal ('..') is not allowed")
        clean_parts.append(part)

    normalized = "/".join(clean_parts) if clean_parts else "."

    if root is not None:
        root_real = os.path.realpath(os.fspath(root))
        target_real = os.path.realpath(os.path.join(root_real, *clean_parts))
        if target_real != root_real and not target_real.startswith(root_real + os.sep):
            raise ValueError("path escapes workspace via symlink")

    return normalized


def new_job_record(spec: Mapping[str, Any], now: datetime | None = None) -> dict[str, Any]:
    """Create an initial queued job record from a validated spec."""
    ts = format_rfc3339(utc_now(now))
    return {
        "id": spec["id"],
        "repo": spec["repo"],
        "ref": spec["ref"],
        "cmd": list(spec["cmd"]),
        "env": dict(spec.get("env") or {}),
        "timeoutSec": int(spec["timeoutSec"]),
        "size": spec["size"],
        "reason": spec.get("reason", ""),
        "source": spec.get("source"),
        "mode": spec["mode"],
        "gpuType": spec.get("gpuType", DEFAULT_GPU_TYPE),
        "state": "queued",
        "exit": None,
        "attempt": 1,
        "createdAt": ts,
        "startedAt": None,
        "endedAt": None,
        "logBytes": 0,
        "tail": "",
        "cost": {"usd": 0.0, "cpuSec": 0.0, "gpuSec": 0},
        "callId": None,
        "sandboxId": None,
        "keepUntil": None,
    }


def transition_job(
    job: MutableMapping[str, Any],
    new_state: str,
    *,
    now: datetime | None = None,
    exit_code: int | None = None,
    attempt: int | None = None,
    log_bytes: int | None = None,
    tail: str | None = None,
    keep_sec: int = 0,
) -> MutableMapping[str, Any]:
    """Transition a job record along valid runner-v1 state edges."""
    if new_state not in ALL_STATES:
        raise ValueError(f"unknown state {new_state!r}")

    current = job.get("state")
    if current in TERMINAL_STATES:
        raise ValueError(f"cannot transition terminal job from {current!r} to {new_state!r}")
    if current == "running" and new_state == "queued":
        raise ValueError("cannot transition job from 'running' back to 'queued'")

    ts = utc_now(now)
    ts_str = format_rfc3339(ts)

    if new_state == "running":
        if job.get("startedAt") is None:
            job["startedAt"] = ts_str
        if attempt is not None:
            job["attempt"] = int(attempt)
    elif new_state in TERMINAL_STATES:
        if job.get("startedAt") is None and new_state == "done":
            job["startedAt"] = ts_str
        job["endedAt"] = ts_str
        if new_state == "cancelled":
            job["exit"] = 137 if exit_code is None else int(exit_code)
        elif exit_code is not None:
            job["exit"] = int(exit_code)

        started = parse_rfc3339(job.get("startedAt"))
        duration = max(0.0, (ts - started).total_seconds()) if started else 0.0
        job["cost"] = estimate_job_cost(
            job.get("size", "medium"),
            duration,
            job.get("mode", "function"),
            job.get("gpuType", DEFAULT_GPU_TYPE),
        )
        if job.get("mode") == "sandbox" and keep_sec > 0 and new_state == "done":
            keep_dt = datetime.fromtimestamp(ts.timestamp() + keep_sec, tz=timezone.utc)
            job["keepUntil"] = format_rfc3339(keep_dt)

    if log_bytes is not None:
        job["logBytes"] = int(log_bytes)
    if tail is not None:
        job["tail"] = tail
    job["state"] = new_state
    return job


def snapshot_dict_state(store: Mapping[str, Any]) -> dict[str, Any]:
    """Build a read-only state snapshot from the backing Dict keyspace."""
    jobs: dict[str, dict[str, Any]] = {}
    costs: dict[str, float] = {}
    for k, v in store.items():
        if isinstance(k, str):
            if k.startswith("job:") and isinstance(v, Mapping):
                jobs[k[4:]] = dict(v)
            elif k.startswith("cost:") and _is_finite_number(v):
                costs[k] = float(v)
    return {
        "config": normalize_config(store.get("config")),
        "paused": bool(store.get("paused", False)),
        "jobs": jobs,
        "costs": costs,
    }


def submit_job(
    store: MutableMapping[str, Any],
    spec: Any,
    now: datetime | None = None,
) -> tuple[int, dict[str, Any]]:
    """Validate, deduplicate, and admit a POST /v1/jobs submission against `store`."""
    ts = utc_now(now)
    state = snapshot_dict_state(store)
    ok, status, err_code, msg, norm = validate_spec(spec, state["config"])
    if not ok or norm is None:
        return status, error_envelope(err_code, msg)

    job_id = norm["id"]
    existing = store.get(job_entry_key(job_id))
    if isinstance(existing, Mapping):
        return 200, build_job_response(existing, state, ts)

    ok, status, err_code, msg = check_admission(state, norm, ts)
    if not ok:
        return status, error_envelope(err_code, msg)

    rec = new_job_record(norm, ts)
    store[job_entry_key(job_id)] = rec
    return 201, {"id": job_id, "state": "queued"}
