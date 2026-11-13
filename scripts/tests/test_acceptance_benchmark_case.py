"""Tests for 12 Warm benchmark case integration into acceptance process and runner."""

import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

ROOT = Path(__file__).resolve().parents[2]
from deploy.acceptance.common import ROOT, Run, digest
from deploy.acceptance import process, run, workload


class BenchmarkCaseAcceptanceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="miles-bench-case-test-")
        self.addCleanup(self.temp.cleanup)
        self.path = Path(self.temp.name)
        self.path.chmod(0o700)
        (self.path / "config.json").write_text(
            json.dumps(
                {
                    "account_id": "123456789012",
                    "deploy_role_arn": "arn:aws:iam::123456789012:role/test/deploy",
                    "runtime_boundary_arn": "arn:aws:iam::123456789012:policy/test/Boundary",
                    "backend_bucket": "fixture-state-123456789012",
                    "ami_id": "ami-0123456789abcdef0",
                }
            )
        )
        (self.path / "run.json").write_text(
            json.dumps(
                {
                    "run_id": "focused-fixture",
                    "source": "a" * 40,
                    "native_config": "/fixture/config",
                    "native_credentials": "/fixture/credentials",
                }
            )
        )
        self.r = Run(self.path)
        self.r.write(
            "init-complete.json",
            {
                "source": "a" * 40,
                "config_sha256": digest(self.path / "config.json"),
            },
        )
        import datetime as dt
        self.r.write(
            "paid-window.json",
            {"cleanup_by": (Run.now() if hasattr(Run, "now") else dt.datetime.now(dt.timezone.utc) + dt.timedelta(hours=4)).isoformat()},
        )
        self.r.write("active-outputs.json", {"fixture": True})

    def test_all_12_warm_cases_and_3_reps_are_allowlisted_in_process_commands(self):
        matrix_file = ROOT / "benchmarks/workload-matrix.json"
        matrix = json.loads(matrix_file.read_text())
        warm_cases = [c["id"] for c in matrix["cases"] if c.get("runner") == "warmbench"]
        self.assertEqual(len(warm_cases), 12)

        for case_id in warm_cases:
            for rep in (1, 2, 3):
                cmd = ("benchmark-case", "--case-id", case_id, "--repetition", str(rep))
                self.assertIn(cmd, process.COMMANDS)

    def test_benchmark_case_is_in_tests_and_writes(self):
        self.assertIn("benchmark-case", process.TESTS)
        self.assertIn("benchmark-case", process.WRITES)
        self.assertIn("benchmark-case", run.MUTATIONS)

    def test_recipe_validation_accepts_warm_benchmark_case(self):
        for name in process.COST_REVIEW:
            self.r.write(name, {"fixture": True})
        recipe = {
            "version": 1,
            "steps": [
                {"id": "review", "review": sorted(process.COST_REVIEW)},
                {
                    "id": "bench-step",
                    "command": [
                        "benchmark-case",
                        "--case-id",
                        "warm-cloud-c1",
                        "--repetition",
                        "1",
                    ],
                    "timeout_seconds": 1500,
                    "approval": "review",
                },
            ],
            "cleanup_steps": [
                {"id": "collect-clean", "command": ["collect"], "timeout_seconds": 60}
            ],
        }
        process.validate(recipe)

    def test_recipe_validation_rejects_unallowlisted_benchmark_case_or_rep(self):
        for bad_cmd in [
            ["benchmark-case", "--case-id", "warm-cloud-c1", "--repetition", "4"],
            ["benchmark-case", "--case-id", "cold-cloud-ir", "--repetition", "1"],
            ["benchmark-case", "--case-id", "nonexistent-case", "--repetition", "1"],
        ]:
            recipe = {
                "version": 1,
                "steps": [
                    {
                        "id": "bad-step",
                        "command": bad_cmd,
                        "timeout_seconds": 60,
                    }
                ],
                "cleanup_steps": [
                    {"id": "collect-clean", "command": ["collect"], "timeout_seconds": 60}
                ],
            }
            with self.subTest(bad_cmd=bad_cmd), self.assertRaises(ValueError):
                process.validate(recipe)

    def test_run_parser_benchmark_case(self):
        p = run.parser()
        args = p.parse_args(
            [
                "--run-dir",
                str(self.path),
                "benchmark-case",
                "--case-id",
                "warm-cloud-c1",
                "--repetition",
                "2",
                "--confirm-run",
                "focused-fixture",
            ]
        )
        self.assertEqual(args.command, "benchmark-case")
        self.assertEqual(args.case_id, "warm-cloud-c1")
        self.assertEqual(args.repetition, 2)
        self.assertEqual(args.confirm_run, "focused-fixture")

    def test_workload_benchmark_case_rejects_unsupported_runner_or_invalid_rep(self):
        with mock.patch.object(self.r, "identity"):
            with self.assertRaises(ValueError):
                workload.benchmark_case(self.r, "cold-cloud-ir", 1)
            with self.assertRaises(ValueError):
                workload.benchmark_case(self.r, "warm-cloud-c1", 0)
            with self.assertRaises(ValueError):
                workload.benchmark_case(self.r, "warm-cloud-c1", 4)

    def test_workload_benchmark_case_rejects_duplicate_slot_run(self):
        slot = "warm-cloud-c1-rep1"
        self.r.write(f"trials/{slot}/result.json", {"status": "passed"})
        with mock.patch.object(self.r, "identity"):
            with self.assertRaisesRegex(ValueError, "already executed"):
                workload.benchmark_case(self.r, "warm-cloud-c1", 1)

    def test_client_benchmark_rejects_malformed_slot_name(self):
        from deploy.acceptance import client
        for bad_name in ["c1", "warm-cloud-c1", "warm-cloud-c1-rep4", "../escape-rep1"]:
            with self.subTest(bad_name=bad_name), self.assertRaises(ValueError):
                client.benchmark({"name": bad_name})


if __name__ == "__main__":
    unittest.main()
