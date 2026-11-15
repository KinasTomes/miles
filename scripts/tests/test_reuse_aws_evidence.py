"""Offline, synthetic-only tests of historical AWS evidence import safety."""

import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("reuse_aws_evidence", ROOT / "scripts/reuse_aws_evidence.py")
reuse = importlib.util.module_from_spec(spec)
spec.loader.exec_module(reuse)


def test_run():
    files = [{"path": f"file-{i:03d}.bin", "group": "synthetic", "version": i,
              "sha256": hashlib.sha256(f"data{i}".encode()).hexdigest(), "bytes": 8} for i in range(90)]
    phases, samples = [], []
    for name in reuse.PHASES:
        n = 1 if name in ("preflight-list", "list") else 90
        operation = reuse.OPERATIONS[name]
        for f in (files[:1] if n == 1 else files):
            sample = {"phase": name, "operation": operation, "latency_ns": 10,
                      "payload_bytes": f["bytes"] if name == "put" or operation == "GetObject" else 0}
            if operation != "ListObjectsV2":
                sample.update(file=f["path"], group=f["group"])
            if operation == "GetObject":
                sample["sha256"] = f["sha256"]
            samples.append(sample)
        phases.append({"name": name, "samples": n, "planned": n, "dispatched": n,
                       "completed": n, "skipped": 0, "failed": 0, "elapsed_ns": n * 100,
                       "successful_logical_bytes": 720 if name == "put" or operation == "GetObject" else 0,
                       "successful_logical_bytes_per_second": (720 * 1e9 / (n * 100)) if name == "put" or operation == "GetObject" else 0.0,
                       "p95_ns": 10})
    return {"schema_version": 1, "mvp_threshold_applied": False, "status": "passed",
            "target": "miles-warm", "region": "ap-southeast-1", "started_at": "2026-11-12T00:00:00Z",
            "finished_at": "2026-11-12T00:05:00Z", "manifest_sha256": "c" * 64,
            "concurrency": 1, "read_repeats": 3, "cleanup": "retained_owned_prefix_for_observation",
            "dataset": {"profile": "backup30", "total_bytes": 720, "files": files},
            "client": {"executable_sha256": "d" * 64, "vcs.revision": "a" * 40},
            "declared_environment": {"profile": "edge", "metadata": "dynamodb",
                                     "gateway_commit": "b" * 40, "policy_revision": "1"},
            "phases": phases, "samples": samples,
            "credentials_file": "must-not-copy", "prefix": "private-raw-object-key",
            "request_ids": ["never-export-raw-request-id"]}


def test_clock(status="OK", verified=True, ending="2026-11-12T00:06:00Z"):
    return {"status": status, "clock_evidence_verified": verified,
            "window_start_utc": "2026-11-11T23:59:00Z", "window_end_utc": ending,
            "chrony_sample_count": 123, "private_host": "must-never-export"}


class ReuseEvidenceTests(unittest.TestCase):
    def test_verified_workload_and_allowlisted_output(self):
        record = reuse.sanitize_run(test_run(), "0" * 64, test_clock(), "e" * 64)
        self.assertTrue(record["warm_workload_verified"])
        self.assertEqual(record["get_sha256_verified"], 360)
        self.assertEqual(record["get_missing_or_invalid"], 0)
        self.assertTrue(record["clock"]["window_encloses_run"])
        self.assertTrue(record["clock"]["reported_verified"])
        serialized = json.dumps(record)
        for fragment in ("must-not-copy", "private-raw-object-key", "never-export-raw-request-id",
                         "private_host", "file-001.bin"):
            self.assertNotIn(fragment, serialized)
        self.assertFalse(record["benchmark_threshold_applied"])
        self.assertFalse(record["matched_direct_s3"])
        self.assertFalse(record["repetitions_accepted"])

    def test_clock_verdict_and_window_coverage_remain_distinct(self):
        record = reuse.sanitize_run(test_run(), "0" * 64,
                                    test_clock(ending="2026-11-12T00:04:59Z"), "f" * 64)
        self.assertTrue(record["clock"]["reported_verified"])
        self.assertFalse(record["clock"]["window_encloses_run"])
        self.assertEqual(reuse.sanitize_run(test_run(), "0" * 64, test_clock("UNVERIFIED", False))["clock"]["status"], "UNVERIFIED")

    def test_interrupted_status_cannot_be_counted_as_workload_pass(self):
        run = test_run()
        run["status"] = "failed"
        run["phases"] = run["phases"][:2]
        run["samples"] = [s for s in run["samples"] if s["phase"] in ("preflight-list", "put")]
        result = reuse.sanitize_run(run, "0" * 64)
        self.assertFalse(result["warm_workload_verified"])
        self.assertEqual(result["get_sha256_verified"], 0)

    def test_unsupported_status_and_synthetic_incomplete_pass_are_rejected(self):
        for mutation in (lambda r: r.update(status="running"),
                         lambda r: r.update(target="direct-s3"),
                         lambda r: r.update(mvp_threshold_applied=True),
                         lambda r: r.update(concurrency=True),
                         lambda r: r.update(read_repeats=0),
                         lambda r: r["declared_environment"].update(profile="cloud"),
                         lambda r: r["dataset"].update(total_bytes=721),
                         lambda r: r["samples"][1].update(file="../../secret"),
                         lambda r: r["phases"][1].update(dispatched=89),
                         lambda r: r["phases"][1].update(p95_ns=11),
                         lambda r: r["phases"][1].update(successful_logical_bytes=719),
                         lambda r: r["phases"][1].update(successful_logical_bytes_per_second=8.0),
                         lambda r: r["samples"][182].update(sha256="0" * 64),
                         lambda r: r["phases"].pop()):
            with self.subTest(mutation=mutation):
                run = test_run()
                mutation(run)
                with self.assertRaises((ValueError, KeyError)):
                    reuse.sanitize_run(run, "0" * 64)

    def test_symlink_duplicate_keys_and_traversal_are_rejected(self):
        with tempfile.TemporaryDirectory() as tmp:
            base = Path(tmp)
            real = base / "edge-c1-result.json"
            real.write_text('{"ok":1,"ok":2}')
            with self.assertRaises(ValueError):
                reuse.local_json(real, 4096)
            real.write_text('{}')
            alias = base / "edge-c1-telemetry-summary.json"
            alias.symlink_to(real)
            with self.assertRaises(OSError):
                reuse.local_json(alias, 4096)
            self.assertIsNone(reuse.parse_roots(["pr86=" + str(base)]).get("pr76-early"))
            with self.assertRaises(ValueError):
                reuse.build({"schema_version": 1,"runs": [{"id": "x", "source": "pr86",
                              "result": "../private.json", "clock": None}]}, {"pr86": base})
            with self.assertRaises(ValueError):
                reuse.build({"schema_version": 1,"runs": [{"id": "x", "source": "pr86",
                              "result": real.name, "clock": "../private.json"}]}, {"pr86": base})

    def test_build_rejects_duplicate_input_and_time_window(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp)
            (p / "edge-c1-result.json").write_text(json.dumps(test_run()))
            run_copy = test_run()
            # Changing only a raw note does not create a new trial window.
            run_copy["private_note"] = "different"
            (p / "edge-c4-result.json").write_text(json.dumps(run_copy))
            record = {"id": "first", "source": "pr86", "result": "edge-c1-result.json", "clock": None}
            with self.assertRaisesRegex(ValueError, "duplicate input result"):
                reuse.build({"schema_version": 1, "runs": [record, dict(record, id="again")]}, {"pr86": p})
            with self.assertRaisesRegex(ValueError, "duplicate artifact or workload window"):
                reuse.build({"schema_version": 1, "runs": [record,
                    {"id": "second", "source": "pr86", "result": "edge-c4-result.json", "clock": None}]}, {"pr86": p})

    def test_committed_public_summary_is_bounded_and_nonaccepting(self):
        inventory = json.loads((ROOT / "benchmarks/evidence/aws-history-selection.json").read_text())
        report = json.loads((ROOT / "benchmarks/evidence/aws-historical-edge-summary.json").read_text())
        self.assertEqual(report["number_of_runs"], len(inventory["runs"]))
        self.assertEqual(report["verified_warm_runs"], 11)
        self.assertEqual(report["incomplete_or_failed_runs"], 5)
        self.assertEqual(report["historically_observed_matrix_case_ids"], ["warm-edge-c1", "warm-edge-c4"])
        self.assertFalse(report["aws_invoked"])
        self.assertFalse(report["benchmark_accepted"])
        self.assertEqual({x["id"] for x in report["runs"]}, {x["id"] for x in inventory["runs"]})
        self.assertEqual(len({x["artifact_sha256"] for x in report["runs"]}), 16)
        for item in report["runs"]:
            self.assertFalse(item["matched_direct_s3"])
            self.assertFalse(item["repetitions_accepted"])
            self.assertFalse(item["benchmark_threshold_applied"])
            self.assertRegex(item["runtime_sha"], r"^[0-9a-f]{40}$")
            if item["warm_workload_verified"]:
                self.assertEqual(item["get_sha256_verified"], 360)
        published = json.dumps(report).lower()
        for forbidden in ("arn:aws", "s3://", "https://", "access_key", "secret_key",
                          "credentials_file", "request_id", "private_key", "/home/ubuntu/.local"):
            self.assertNotIn(forbidden, published)

    def test_cli_explicit_roots_and_output_no_overwrite(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp)
            source = p / "edge-c1-result.json"
            source.write_text(json.dumps(test_run()))
            selection = p / "selection.json"
            selection.write_text(json.dumps({"schema_version": 1,
                                             "runs": [{"id": "demo", "source": "pr86",
                                                       "result": source.name, "clock": None}]}))
            out = p / "report.json"
            argv = ["--selection", str(selection), "--root", "pr86=" + str(p), "--out", str(out)]
            self.assertEqual(reuse.main(argv), 0)
            report = json.loads(out.read_text())
            self.assertEqual(report["verified_warm_runs"], 1)
            self.assertEqual(report["runs"][0]["origin_pr"], 86)
            self.assertEqual(out.stat().st_mode & 0o777, 0o600)
            self.assertEqual(reuse.main(argv), 1)


if __name__ == "__main__":
    unittest.main()
