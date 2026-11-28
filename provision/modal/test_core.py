"""Unit tests for runner-v1 protocol logic, sizing, cost math, and golden fixtures."""

from __future__ import annotations

import base64
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import sys
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


class StdlibOnlyTest(unittest.TestCase):
    def test_no_modal_import_in_core_or_rates(self) -> None:
        self.assertNotIn("modal", sys.modules)
        self.assertEqual(rates.RATES_UPDATED, "2026-11-27")
        self.assertEqual(rates.SANDBOX_CPU_MEM_MULTIPLIER, 3.0)


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

    def test_validate_spec_bounds_and_overrides(self) -> None:
        ok, status, err, _, norm = core.validate_spec(self._spec(timeoutSec=0), core.DEFAULT_CONFIG)
        self.assertTrue(ok)
        self.assertEqual(norm["timeoutSec"], 3600)
        self.assertEqual(norm["mode"], "function")

        ok, status, err, _, norm = core.validate_spec(self._spec(mode="sandbox"), core.DEFAULT_CONFIG)
        self.assertTrue(ok)
        self.assertEqual(norm["mode"], "sandbox")

        ok, status, err, _, _ = core.validate_spec(self._spec(timeoutSec=3601), core.DEFAULT_CONFIG)
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
        # Running T4 job for 1000s accrues > $0.22, pushing spent+running past $1.00.
        ok, status, code, _ = core.check_admission(state, self._spec(), now)
        self.assertFalse(ok)
        self.assertEqual((status, code), (402, "budget"))

    def test_idempotent_duplicate_submit(self) -> None:
        store: dict = {}
        spec = self._spec()
        t0 = datetime(2026, 11, 27, 15, 0, 0, tzinfo=timezone.utc)
        status1, body1 = core.submit_job(store, spec, now=t0)
        self.assertEqual(status1, 201)
        self.assertEqual(body1, {"id": spec["id"], "state": "queued"})

        # Even if paused later, resubmitting the same job id returns 200 with existing record.
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

    def test_sanitize_rel_path_blocks_traversal_and_symlinks(self) -> None:
        self.assertEqual(core.sanitize_rel_path("./out/report.xml"), "out/report.xml")
        self.assertEqual(core.sanitize_rel_path("."), ".")

        for bad in ("", "/etc/passwd", "../secret", "a/../../b", "C:\\x", "a\\b", "a\x00b"):
            with self.assertRaises(ValueError):
                core.sanitize_rel_path(bad)

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp) / "work"
            outside = Path(tmp) / "outside.txt"
            root.mkdir()
            outside.write_text("secret", encoding="utf-8")
            inside = root / "ok.txt"
            inside.write_text("ok", encoding="utf-8")
            self.assertEqual(core.sanitize_rel_path("ok.txt", root=root), "ok.txt")

            link = root / "escape"
            try:
                os.symlink(outside, link)
            except (OSError, NotImplementedError):
                return
            with self.assertRaises(ValueError):
                core.sanitize_rel_path("escape", root=root)


class ConfigUpdateTest(unittest.TestCase):
    def test_config_validation(self) -> None:
        ok, _, _, _, merged = core.validate_config_update(
            core.DEFAULT_CONFIG,
            {"capUSD": 50, "keepSec": 300, "mode": "sandbox"},
        )
        self.assertTrue(ok)
        self.assertEqual(merged["capUSD"], 50.0)
        self.assertEqual(merged["keepSec"], 300)
        self.assertEqual(merged["mode"], "sandbox")

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
