"""Unit tests for runner-v1 protocol logic, sizing, cost math, and golden fixtures."""

from __future__ import annotations

import ast
import base64
from datetime import datetime, timedelta, timezone
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest

MODAL_DIR = Path(__file__).resolve().parent
FIXTURES_DIR = MODAL_DIR.parent.parent / "docs" / "protocol" / "runner-v1"

if str(MODAL_DIR) not in sys.path:
    sys.path.insert(0, str(MODAL_DIR))

import core  # noqa: E402
import rates  # noqa: E402


def _load_fixture(name: str) -> dict:
    return json.loads((FIXTURES_DIR / name).read_text(encoding="utf-8"))


class StdlibAndStaticSafetyTest(unittest.TestCase):
    def test_no_modal_import_in_core_or_rates(self) -> None:
        self.assertNotIn("modal", sys.modules)
        self.assertEqual(rates.RATES_UPDATED, "2026-11-27")
        self.assertEqual(rates.SANDBOX_CPU_MEM_MULTIPLIER, 3.0)

    def test_runner_has_no_postponed_annotations_local_import_hazard(self) -> None:
        """Prevent P0 FastAPI startup bug: future annotations + function-local FastAPI imports."""
        src = (MODAL_DIR / "agent_box_runner.py").read_text(encoding="utf-8")
        tree = ast.parse(src)
        has_future_annotations = any(
            isinstance(node, ast.ImportFrom)
            and node.module == "__future__"
            and any(alias.name == "annotations" for alias in node.names)
            for node in tree.body
        )
        self.assertFalse(
            has_future_annotations,
            "agent_box_runner.py must not enable `from __future__ import annotations` "
            "because FastAPI resolves stringified route annotations in module globals.",
        )

        # Also verify app_factory exists and route handler annotations are resolvable.
        fn_map = {n.name: n for n in tree.body if isinstance(n, ast.FunctionDef)}
        self.assertIn("app_factory", fn_map)
        app_fn = fn_map["app_factory"]
        route_handlers = [
            n
            for n in app_fn.body
            if isinstance(n, (ast.FunctionDef, ast.AsyncFunctionDef)) and n.decorator_list
        ]
        self.assertGreaterEqual(len(route_handlers), 10)

        # Ensure sandbox creation never mounts the shared writable /jobs volume and never uses Sandbox.open.
        self.assertNotIn("sb.open(", src)
        self.assertNotIn(".open(\"/tmp/", src)
        for node in ast.walk(tree):
            if (
                isinstance(node, ast.Call)
                and isinstance(node.func, ast.Attribute)
                and node.func.attr == "create"
            ):
                src_seg = ast.get_source_segment(src, node) or ""
                if "Sandbox.create" in src_seg:
                    self.assertNotIn("volumes", src_seg)

    def test_app_factory_with_stubbed_modal_when_fastapi_available(self) -> None:
        try:
            import fastapi  # noqa: F401
        except ImportError:
            self.skipTest("fastapi not installed in stdlib-only environment")

        import types

        fake_modal = types.ModuleType("modal")

        class _Dummy:
            def __init__(self, *args, **kwargs):
                pass

            def __getattr__(self, _name):
                return lambda *a, **kw: self

        class _App(_Dummy):
            def function(self, *args, **kwargs):
                return lambda fn: fn

        fake_modal.App = _App
        fake_modal.Volume = type("Volume", (), {"from_name": staticmethod(lambda *a, **kw: _Dummy())})
        fake_modal.Dict = type("Dict", (), {"from_name": staticmethod(lambda *a, **kw: _Dummy())})
        fake_modal.Image = type("Image", (), {"debian_slim": staticmethod(lambda *a, **kw: _Dummy())})
        fake_modal.Retries = _Dummy
        fake_modal.asgi_app = lambda *a, **kw: (lambda fn: fn)

        prev = sys.modules.get("modal")
        sys.modules["modal"] = fake_modal
        try:
            import importlib

            runner = importlib.import_module("agent_box_runner")
            api = runner.app_factory(backend=object())
            paths = {r.path for r in api.routes}
            self.assertIn("/v1/probe", paths)
            self.assertIn("/v1/jobs", paths)
            self.assertIn("/v1/jobs/{job_id}/file", paths)
        finally:
            if prev is None:
                sys.modules.pop("modal", None)
            else:
                sys.modules["modal"] = prev
            sys.modules.pop("agent_box_runner", None)


class GoldenFixturesTest(unittest.TestCase):
    def test_all_fixtures_discovered(self) -> None:
        names = sorted(p.name for p in FIXTURES_DIR.glob("*.json"))
        self.assertEqual(
            names,
            [
                "cancel.json",
                "config.json",
                "error-budget.json",
                "error-busy.json",
                "error-gone.json",
                "error-paused.json",
                "job-done.json",
                "job-list.json",
                "job-queued.json",
                "job-spec.json",
                "log-chunk.json",
                "pause.json",
                "probe.json",
                "stop.json",
            ],
        )

    def test_probe_fixture_schema(self) -> None:
        fixture = _load_fixture("probe.json")
        now = datetime(2026, 11, 27, 15, 10, 0, tzinfo=timezone.utc)
        state = {
            "config": dict(core.DEFAULT_CONFIG),
            "paused": False,
            "jobs": {
                "a.20261127-150000-1111": {
                    "id": "a.20261127-150000-1111",
                    "state": "running",
                    "size": "medium",
                    "mode": "function",
                    "startedAt": "2026-11-27T15:09:40Z",
                },
                "b.20261127-150001-2222": {
                    "id": "b.20261127-150001-2222",
                    "state": "queued",
                    "size": "small",
                    "mode": "function",
                },
            },
            "costs": {"cost:2026-11:prev": 3.12},
        }
        got = core.build_probe_response(state, now=now)
        self.assertEqual(set(got.keys()), set(fixture.keys()))
        self.assertEqual(set(got["budget"].keys()), set(fixture["budget"].keys()))
        self.assertEqual(got["protocol"], fixture["protocol"])
        self.assertEqual(got["sizes"], fixture["sizes"])
        self.assertEqual(got["running"], 2)
        self.assertEqual(got["budget"]["month"], "2026-11")
        self.assertEqual(got["budget"]["spentUSD"], 3.12)

    def test_job_spec_and_queued_fixtures(self) -> None:
        spec_fx = _load_fixture("job-spec.json")
        queued_fx = _load_fixture("job-queued.json")
        store: dict = {}
        status, body = core.submit_job(store, spec_fx)
        self.assertEqual(status, 201)
        self.assertEqual(body, queued_fx)

    def test_job_done_and_job_list_fixtures_schema(self) -> None:
        done_fx = _load_fixture("job-done.json")
        list_fx = _load_fixture("job-list.json")
        now = datetime(2026, 11, 27, 15, 5, 12, tzinfo=timezone.utc)
        job = {
            "id": done_fx["id"],
            "state": "done",
            "exit": 0,
            "attempt": 1,
            "startedAt": "2026-11-27T15:04:08Z",
            "endedAt": "2026-11-27T15:05:12Z",
            "logBytes": 1842,
            "tail": done_fx["tail"],
            "mode": "function",
            "cost": {"usd": 0.012, "cpuSec": 64.0, "gpuSec": 0},
        }
        state = {"config": dict(core.DEFAULT_CONFIG), "jobs": {job["id"]: job}, "costs": {"cost:2026-11:j": 3.132}}
        got = core.build_job_response(job, state, now=now)
        self.assertEqual(set(got.keys()), set(done_fx.keys()))
        self.assertEqual(set(got["cost"].keys()), set(done_fx["cost"].keys()))
        self.assertEqual(set(got["budget"].keys()), set(done_fx["budget"].keys()))
        self.assertEqual(set(got.keys()), set(list_fx["jobs"][0].keys()))

    def test_log_chunk_fixture_exact_match(self) -> None:
        fixture = _load_fixture("log-chunk.json")
        tail_bytes = fixture["data"].encode("utf-8")
        prefix_len = fixture["logBytes"] - len(tail_bytes)
        full_log = b"x" * prefix_len + tail_bytes
        got = core.build_log_chunk(full_log, offset=prefix_len, limit=core.MAX_LOG_CHUNK, paused=False, stopped=True)
        self.assertEqual(got, fixture)

    def test_config_fixture(self) -> None:
        fixture = _load_fixture("config.json")
        ok, status, err, msg, merged = core.validate_config_update(core.DEFAULT_CONFIG, fixture)
        self.assertTrue(ok, (status, err, msg))
        self.assertEqual(core.public_config(merged), fixture)

    def test_error_envelopes_match_fixtures(self) -> None:
        paused_fx = _load_fixture("error-paused.json")
        budget_fx = _load_fixture("error-budget.json")
        busy_fx = _load_fixture("error-busy.json")
        gone_fx = _load_fixture("error-gone.json")

        spec = _load_fixture("job-spec.json")
        now = datetime(2026, 11, 27, 15, 0, 0, tzinfo=timezone.utc)

        ok, status, code, msg = core.check_admission({"paused": True, "config": core.DEFAULT_CONFIG}, spec, now)
        self.assertFalse(ok)
        self.assertEqual(status, 423)
        self.assertEqual(core.error_envelope(code, msg), paused_fx)

        ok, status, code, msg = core.check_admission(
            {"paused": False, "config": core.DEFAULT_CONFIG, "spentUSD": 25.0, "runningUSD": 0.0},
            spec,
            now,
        )
        self.assertFalse(ok)
        self.assertEqual(status, 402)
        self.assertEqual(core.error_envelope(code, msg), budget_fx)

        ok, status, code, msg = core.check_admission(
            {"paused": False, "config": core.DEFAULT_CONFIG, "spentUSD": 0.0, "runningUSD": 0.0, "running": 4},
            spec,
            now,
        )
        self.assertFalse(ok)
        self.assertEqual(status, 409)
        self.assertEqual(core.error_envelope(code, msg), busy_fx)

        self.assertEqual(
            core.error_envelope("gone", "Sandbox is no longer kept; files are unavailable."),
            gone_fx,
        )

    def test_control_response_shapes(self) -> None:
        cancel_fx = _load_fixture("cancel.json")
        pause_fx = _load_fixture("pause.json")
        stop_fx = _load_fixture("stop.json")
        self.assertEqual(set(cancel_fx.keys()), {"cancelled", "exit"})
        self.assertEqual(set(pause_fx.keys()), {"paused"})
        self.assertEqual(set(stop_fx.keys()), {"stopped"})


class SizingAndCostTest(unittest.TestCase):
    def test_size_table_mirrors_go_sizes(self) -> None:
        self.assertEqual(core.SIZES["small"].vcpus, 2)
        self.assertEqual(core.SIZES["small"].cpu_cores, 1.0)
        self.assertEqual(core.SIZES["small"].mem_gib, 2)
        self.assertEqual(core.SIZES["medium"].vcpus, 4)
        self.assertEqual(core.SIZES["medium"].cpu_cores, 2.0)
        self.assertEqual(core.SIZES["medium"].mem_gib, 4)
        self.assertEqual(core.SIZES["large"].vcpus, 8)
        self.assertEqual(core.SIZES["large"].cpu_cores, 4.0)
        self.assertEqual(core.SIZES["large"].mem_gib, 8)
        self.assertEqual(core.SIZES["gpu"].vcpus, 6)
        self.assertEqual(core.SIZES["gpu"].cpu_cores, 3.0)
        self.assertEqual(core.SIZES["gpu"].mem_gib, 8)
        self.assertTrue(core.SIZES["gpu"].gpu)

    def test_function_and_sandbox_cost_math(self) -> None:
        fn_cost = core.estimate_job_cost("medium", 100.0, mode="function")
        expected_fn = round(2.0 * 100.0 * rates.CPU_CORE_SEC_USD + 4 * 100.0 * rates.MEM_GIB_SEC_USD, 6)
        self.assertEqual(fn_cost["usd"], expected_fn)
        self.assertEqual(fn_cost["cpuSec"], 200.0)
        self.assertEqual(fn_cost["gpuSec"], 0)

        sb_cost = core.estimate_job_cost("medium", 100.0, mode="sandbox")
        self.assertAlmostEqual(sb_cost["usd"], round(expected_fn * 3.0, 6), places=6)
        self.assertEqual(sb_cost["cpuSec"], 200.0)
        self.assertEqual(sb_cost["gpuSec"], 0)

    def test_gpu_rates_1x_in_both_modes(self) -> None:
        for gpu_type, gpu_rate in rates.GPU_RATES_USD_PER_SEC.items():
            fn = core.estimate_job_cost("gpu", 60.0, mode="function", gpu_type=gpu_type)
            sb = core.estimate_job_cost("gpu", 60.0, mode="sandbox", gpu_type=gpu_type)
            cpu_mem_fn = (3.0 * rates.CPU_CORE_SEC_USD + 8 * rates.MEM_GIB_SEC_USD) * 60.0
            self.assertEqual(fn["usd"], round(cpu_mem_fn + gpu_rate * 60.0, 6))
            self.assertEqual(sb["usd"], round(cpu_mem_fn * 3.0 + gpu_rate * 60.0, 6))
            self.assertEqual(fn["gpuSec"], 60.0)
            self.assertEqual(sb["gpuSec"], 60.0)

    def test_volume_monthly_cost_ledger_and_kept_sandbox_spend(self) -> None:
        now = datetime(2026, 11, 27, 16, 10, 0, tzinfo=timezone.utc)
        with tempfile.TemporaryDirectory() as tmp:
            costs_dir = Path(tmp) / "_costs"
            core.write_cost_record(costs_dir, "2026-11", "a.20261127-150000-0001", 1.25, settled_at="2026-11-27T15:05:00Z")
            core.write_cost_record(costs_dir, "2026-11", "b.20261127-151000-0002", 0.50)
            # Even if Dict lost all cost:* keys after 7 idle days, Volume ledger recovers them.
            store = {
                "config": dict(core.DEFAULT_CONFIG),
                "job:sb.20261127-160000-0003": {
                    "id": "sb.20261127-160000-0003",
                    "state": "done",
                    "mode": "sandbox",
                    "size": "medium",
                    "startedAt": "2026-11-27T16:00:00Z",
                    "endedAt": "2026-11-27T16:01:00Z",
                    "keepUntil": "2026-11-27T16:15:00Z",
                },
            }
            snap = core.snapshot_dict_state(store, costs_root=costs_dir, now=now)
            budget = core.compute_budget(snap, now=now)
            self.assertEqual(budget["spentUSD"], 1.75)
            # Kept sandbox has been alive 600s since startedAt and isn't settled yet, so runningUSD reflects it.
            expected_kept = core.estimate_job_cost("medium", 600.0, mode="sandbox")["usd"]
            self.assertEqual(budget["runningUSD"], expected_kept)


class ValidationAndLifecycleTest(unittest.TestCase):
    def _spec(self, **overrides) -> dict:
        base = _load_fixture("job-spec.json")
        base.update(overrides)
        return base

    def test_valid_job_id(self) -> None:
        self.assertTrue(core.valid_job_id("lantern.20261127-150405-a1b2"))
        self.assertTrue(core.valid_job_id("a.20261127-000000-0"))
        self.assertFalse(core.valid_job_id("Lantern.20261127-150405-a1b2"))
        self.assertFalse(core.valid_job_id("lantern_1.20261127-150405-a1b2"))
        self.assertFalse(core.valid_job_id("lantern.20261127-150405-xyz"))
        self.assertFalse(core.valid_job_id("h1-pc~lantern.20261127-150405-a1b2"))

    def test_validate_spec_bounds_overrides_and_sandbox_24h_cap(self) -> None:
        ok, _, _, _, norm = core.validate_spec(self._spec(timeoutSec=0), core.DEFAULT_CONFIG)
        self.assertTrue(ok)
        self.assertEqual(norm["timeoutSec"], 3600)
        self.assertEqual(norm["mode"], "function")

        ok, _, _, _, norm = core.validate_spec(self._spec(mode="sandbox"), core.DEFAULT_CONFIG)
        self.assertTrue(ok)
        self.assertEqual(norm["mode"], "sandbox")

        # Function GPU normalizes gpuType to deployed worker GPU type.
        ok, _, _, _, norm = core.validate_spec(
            self._spec(size="gpu", gpuType="H100"),
            core.DEFAULT_CONFIG,
            deployed_gpu_type="T4",
        )
        self.assertTrue(ok)
        self.assertEqual(norm["gpuType"], "T4")

        ok, status, err, _, _ = core.validate_spec(self._spec(timeoutSec=3601), core.DEFAULT_CONFIG)
        self.assertFalse(ok)
        self.assertEqual((status, err), (400, "timeout_cap"))

        # Modal 24h (86400s) sandbox lifetime ceiling: timeoutSec + keepSec > 86400 must fail.
        sb_cfg = {**core.DEFAULT_CONFIG, "maxTimeoutSec": 86400, "keepSec": 900, "mode": "sandbox"}
        ok, status, err, _, _ = core.validate_spec(self._spec(timeoutSec=86000, mode="sandbox"), sb_cfg)
        self.assertFalse(ok)
        self.assertEqual((status, err), (400, "timeout_cap"))

        for bad in (
            self._spec(timeoutSec=-1),
            self._spec(timeoutSec=True),
            self._spec(size="xlarge"),
            self._spec(cmd=[]),
            self._spec(cmd=[""]),
            self._spec(repo="other"),
            self._spec(mode="vm"),
            self._spec(env={"K": 123}),
            self._spec(source={"commit": "bad", "dirty": False, "snapshotCommit": "a" * 40}),
        ):
            ok, status, err, _, _ = core.validate_spec(bad, core.DEFAULT_CONFIG)
            self.assertFalse(ok)
            self.assertEqual((status, err), (400, "invalid"))

    def test_clean_env_and_privilege_drop_kwargs(self) -> None:
        host_env = {
            "PATH": "/usr/local/bin:/usr/bin",
            "HOME": "/root",
            "LANG": "C.UTF-8",
            "MODAL_TOKEN_ID": "ak-secret",
            "MODAL_TOKEN_SECRET": "as-secret",
            "MODAL_IDENTITY_TOKEN": "oidc-jwt",
            "AWS_SECRET_ACCESS_KEY": "nope",
        }
        cleaned = core.build_clean_env(host_env, {"CGO_ENABLED": "1"})
        self.assertEqual(cleaned["PATH"], "/usr/local/bin:/usr/bin")
        self.assertEqual(cleaned["CGO_ENABLED"], "1")
        for secret_key in ("MODAL_TOKEN_ID", "MODAL_TOKEN_SECRET", "MODAL_IDENTITY_TOKEN", "AWS_SECRET_ACCESS_KEY"):
            self.assertNotIn(secret_key, cleaned)

        self.assertEqual(
            core.build_subprocess_kwargs(0),
            {"start_new_session": True, "user": "agent", "group": "agent"},
        )
        self.assertEqual(
            core.build_subprocess_kwargs(1000),
            {"start_new_session": True},
        )

    def test_cancellation_tombstone_overrides_racing_worker_write(self) -> None:
        now = datetime(2026, 11, 27, 16, 0, 0, tzinfo=timezone.utc)
        store = {
            "config": dict(core.DEFAULT_CONFIG),
            core.cancelled_entry_key("a.20261127-150000-0001"): True,
            core.job_entry_key("a.20261127-150000-0001"): {
                "id": "a.20261127-150000-0001",
                "state": "done",
                "exit": 0,
                "mode": "function",
                "startedAt": "2026-11-27T15:00:00Z",
                "endedAt": "2026-11-27T15:01:00Z",
            },
        }
        snap = core.snapshot_dict_state(store, now=now)
        rec = snap["jobs"]["a.20261127-150000-0001"]
        self.assertEqual(rec["state"], "cancelled")
        self.assertEqual(rec["exit"], 137)

    def test_admission_includes_running_job_accrued_spend(self) -> None:
        now = datetime(2026, 11, 27, 16, 0, 0, tzinfo=timezone.utc)
        started = now - timedelta(seconds=1000)
        state = {
            "paused": False,
            "config": {**core.DEFAULT_CONFIG, "capUSD": 1.0},
            "costs": {"cost:2026-11:old": 0.90},
            "jobs": {
                "gpu.20261127-150000-0001": {
                    "id": "gpu.20261127-150000-0001",
                    "state": "running",
                    "size": "gpu",
                    "mode": "function",
                    "gpuType": "T4",
                    "startedAt": core.format_rfc3339(started),
                }
            },
        }
        ok, status, code, _ = core.check_admission(state, self._spec(), now)
        self.assertFalse(ok)
        self.assertEqual((status, code), (402, "budget"))

    def test_idempotent_duplicate_submit_survives_config_changes(self) -> None:
        store: dict = {}
        spec = self._spec(timeoutSec=900)
        t0 = datetime(2026, 11, 27, 15, 0, 0, tzinfo=timezone.utc)
        status1, body1 = core.submit_job(store, spec, now=t0)
        self.assertEqual(status1, 201)
        self.assertEqual(body1, {"id": spec["id"], "state": "queued"})

        # Lower maxTimeoutSec below 900 and pause the runner: duplicate id must still return 200.
        store["config"] = {**core.DEFAULT_CONFIG, "maxTimeoutSec": 60}
        store["paused"] = True
        status2, body2 = core.submit_job(store, spec, now=t0 + timedelta(seconds=5))
        self.assertEqual(status2, 200)
        self.assertEqual(body2["id"], spec["id"])
        self.assertEqual(body2["state"], "queued")

    def test_state_transitions_and_preemption_guard(self) -> None:
        rec = core.new_job_record(
            {
                "id": "lantern.20261127-150405-a1b2",
                "repo": "lantern",
                "ref": "20261127-150405-a1b2",
                "cmd": ["echo", "hi"],
                "timeoutSec": 60,
                "size": "small",
                "mode": "sandbox",
            },
            now=datetime(2026, 11, 27, 15, 0, 0, tzinfo=timezone.utc),
        )
        core.transition_job(rec, "running", now=datetime(2026, 11, 27, 15, 0, 2, tzinfo=timezone.utc), attempt=1)
        self.assertEqual(rec["attempt"], 1)
        with self.assertRaises(ValueError):
            core.transition_job(rec, "queued")

        core.transition_job(
            rec,
            "done",
            now=datetime(2026, 11, 27, 15, 0, 12, tzinfo=timezone.utc),
            exit_code=0,
            log_bytes=3,
            tail="hi\n",
            keep_sec=900,
        )
        self.assertEqual(rec["state"], "done")
        self.assertEqual(rec["exit"], 0)
        self.assertEqual(rec["keepUntil"], "2026-11-27T15:15:12Z")
        with self.assertRaises(ValueError):
            core.transition_job(rec, "running")


class LogChunkAndPathSanitizationTest(unittest.TestCase):
    def test_parse_int_query(self) -> None:
        self.assertEqual(core.parse_int_query(None, "limit", 20, min_value=1, max_value=200), 20)
        self.assertEqual(core.parse_int_query("500", "limit", 20, min_value=1, max_value=200), 200)
        self.assertEqual(core.parse_int_query("0", "offset", 0, min_value=0), 0)
        # Empty query values (?offset=, ?limit=, ?wait=) must reject with ValueError (400 invalid).
        for param_name, min_val in (("offset", 0), ("limit", 1), ("wait", 0)):
            with self.assertRaises(ValueError):
                core.parse_int_query("", param_name, 0, min_value=min_val)
        for bad in ("abc", "1.5", "-1", "1_0", " 1"):
            with self.assertRaises(ValueError):
                core.parse_int_query(bad, "offset", 0, min_value=0)

    def test_log_chunk_paging_and_invalid_utf8(self) -> None:
        raw = b"a\xffb\xe2\x82\xac"
        c1 = core.build_log_chunk(raw, offset=0, limit=3, stopped=True)
        self.assertEqual(c1["offset"], 3)
        self.assertEqual(c1["data"], "a\ufffdb")
        self.assertEqual(c1["dataBase64"], base64.b64encode(b"a\xffb").decode("ascii"))
        self.assertFalse(c1["eof"])
        self.assertTrue(c1["stopped"])
        self.assertEqual(c1["logBytes"], len(raw))

        c2 = core.build_log_chunk(raw, offset=3, limit=10, stopped=True)
        self.assertEqual(c2["offset"], len(raw))
        self.assertEqual(c2["data"], "€")
        self.assertNotIn("dataBase64", c2)
        self.assertTrue(c2["eof"])

    def test_sanitize_rel_path_and_safe_tar_blocks_traversal_symlinks_and_handles_dashes(self) -> None:
        self.assertEqual(core.sanitize_rel_path("./out/report.xml"), "out/report.xml")
        self.assertEqual(core.sanitize_rel_path("."), ".")
        self.assertEqual(core.sanitize_rel_path("-rf"), "-rf")
        self.assertEqual(core.sanitize_rel_path("--checkpoint=1"), "--checkpoint=1")

        for bad in ("", "/etc/passwd", "../secret", "a/../../b", "C:\\x", "a\\b", "a\x00b"):
            with self.assertRaises(ValueError):
                core.sanitize_rel_path(bad)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "work"
            outside = Path(tmp) / "outside.txt"
            root.mkdir()
            outside.write_text("secret", encoding="utf-8")
            (root / "-rf").write_text("dash", encoding="utf-8")
            (root / "--checkpoint=1").write_text("ckpt", encoding="utf-8")

            # Leading-dash filenames archive safely without flag injection.
            for dash_name in ("-rf", "--checkpoint=1"):
                tar_bytes = core.create_safe_tar_bytes(root, dash_name)
                with tarfile.open(fileobj=io.BytesIO(tar_bytes), mode="r:") as tf:
                    self.assertEqual(tf.getnames(), [dash_name])

            link = root / "escape"
            try:
                os.symlink(outside, link)
            except (OSError, NotImplementedError):
                return
            with self.assertRaises(ValueError):
                core.sanitize_rel_path("escape", root=root)
            with self.assertRaises(ValueError):
                core.create_safe_tar_bytes(root, "escape")

    @unittest.skipIf(os.name == "nt", "dir_fd/O_NOFOLLOW requires POSIX")
    def test_actual_sandbox_tar_script_toctou_free(self) -> None:
        """Execute the actual SANDBOX_TAR_SCRIPT used by agent_box_runner against a real tree."""
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "work"
            outside = Path(tmp) / "outside.txt"
            root.mkdir()
            outside.write_text("secret", encoding="utf-8")
            (root / "nested").mkdir()
            (root / "nested" / "out.txt").write_text("hello\n", encoding="utf-8")
            (root / "-rf").write_text("dash-file", encoding="utf-8")
            os.symlink(outside, root / "escape-link")
            os.symlink(outside, root / "nested" / "inner-escape")

            # Regular nested file succeeds.
            res = subprocess.run(
                [sys.executable, "-c", core.SANDBOX_TAR_SCRIPT, str(root), "nested/out.txt"],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(res.returncode, 0, res.stderr)
            with tarfile.open(fileobj=io.BytesIO(base64.b64decode(res.stdout)), mode="r:") as tf:
                self.assertEqual(tf.getnames(), ["nested/out.txt"])
                self.assertEqual(tf.extractfile("nested/out.txt").read(), b"hello\n")

            # Leading-dash filename archives safely without flag injection.
            res_dash = subprocess.run(
                [sys.executable, "-c", core.SANDBOX_TAR_SCRIPT, str(root), "-rf"],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(res_dash.returncode, 0, res_dash.stderr)
            with tarfile.open(fileobj=io.BytesIO(base64.b64decode(res_dash.stdout)), mode="r:") as tf:
                self.assertEqual(tf.getnames(), ["-rf"])

            # Directory walk drops symlink children rather than following them.
            res_dir = subprocess.run(
                [sys.executable, "-c", core.SANDBOX_TAR_SCRIPT, str(root), "nested"],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(res_dir.returncode, 0, res_dir.stderr)
            with tarfile.open(fileobj=io.BytesIO(base64.b64decode(res_dir.stdout)), mode="r:") as tf:
                self.assertEqual(tf.getnames(), ["nested", "nested/out.txt"])

            # Direct symlink or traversal exits with code 2 (400 invalid).
            for bad_rel in ("escape-link", "nested/inner-escape", "../outside.txt"):
                res_bad = subprocess.run(
                    [sys.executable, "-c", core.SANDBOX_TAR_SCRIPT, str(root), bad_rel],
                    capture_output=True,
                    text=True,
                    check=False,
                )
                self.assertEqual(res_bad.returncode, 2, (bad_rel, res_bad.returncode, res_bad.stderr))

            # Missing file exits with code 3 (404 not_found).
            res_missing = subprocess.run(
                [sys.executable, "-c", core.SANDBOX_TAR_SCRIPT, str(root), "does-not-exist"],
                capture_output=True,
                text=True,
                check=False,
            )
            self.assertEqual(res_missing.returncode, 3)


class ConfigUpdateTest(unittest.TestCase):
    def test_config_validation_and_sandbox_24h_cap(self) -> None:
        ok, _, _, _, merged = core.validate_config_update(
            core.DEFAULT_CONFIG,
            {"capUSD": 50, "keepSec": 300, "mode": "sandbox"},
        )
        self.assertTrue(ok)
        self.assertEqual(merged["capUSD"], 50.0)
        self.assertEqual(merged["keepSec"], 300)
        self.assertEqual(merged["mode"], "sandbox")

        ok, status, code, _, _ = core.validate_config_update(
            core.DEFAULT_CONFIG,
            {"maxTimeoutSec": 86400, "keepSec": 900, "mode": "sandbox"},
        )
        self.assertFalse(ok)
        self.assertEqual((status, code), (400, "timeout_cap"))

        for bad in (
            "not-a-dict",
            {"unknown": 1},
            {"maxConcurrent": 0},
            {"maxConcurrent": True},
            {"maxTimeoutSec": 0},
            {"keepSec": -1},
            {"mode": "ec2"},
            {"capUSD": -5},
        ):
            ok, status, code, _, _ = core.validate_config_update(core.DEFAULT_CONFIG, bad)
            self.assertFalse(ok)
            self.assertEqual((status, code), (400, "invalid"))


if __name__ == "__main__":
    unittest.main()
