import copy
import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("dynamodb_pilot", Path(__file__).parents[1] / "dynamodb_pilot.py")
pilot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(pilot)


def fixture():
    base = {
        "schema_version": 1, "status": "passed", "mvp_threshold_applied": False,
        "started_at": "2026-11-04T00:00:10Z", "finished_at": "2026-11-04T00:00:20Z",
        "manifest_sha256": "1" * 64, "concurrency": 1, "read_repeats": 0, "region": "ap-southeast-1",
        "client": {"executable_sha256": "2" * 64}, "dataset": {"total_bytes": 100},
        "declared_environment": {"host": "same-client", "cpu": "same", "disk": "same", "network": "same", "encryption": "same", "gateway_commit": "test-release", "policy_revision": "1"},
        "cleanup": "retained_owned_prefix_for_observation",
        "phases": [{"name": n, "failed": 0, "samples": 1, "elapsed_ns": 1000, "successful_logical_bytes": 100, "successful_logical_bytes_per_second": 10, "p50_ns": 1000, "p95_ns": 1000, "max_ns": 1000} for n in ("put", "head", "verify-get", "list")],
        "samples": [{"operation": "GetObject", "sha256": "3" * 64}],
        "http_body_bytes": {"client_to_gateway": 100, "gateway_to_client": 150},
    }
    runs = {n: copy.deepcopy(base) for n in ("cloud", "edge", "direct")}
    for n in ("cloud", "edge"):
        runs[n]["target"] = "miles-warm"
        runs[n]["declared_environment"].update(profile=n, metadata="dynamodb-local")
    runs["direct"]["target"] = "local-s3-smoke"
    runs["direct"]["http_body_bytes"] = {"client_to_s3": 100, "s3_to_client": 150}
    runs["direct"]["phases"][0]["successful_logical_bytes_per_second"] = 20
    def snapshot(value):
        return "\n".join(["miles_process_start_time_seconds 1"] + [f"{k} {value}" for k in pilot.COUNTERS] + [f"{k} 0" for k in pilot.GAUGES])
    observation = {n: {
        "before_at": "2026-11-04T00:00:00Z", "after_at": "2026-11-04T00:00:30Z",
        "nodes": [{"alias": f"node-{i}", "profile": p, "before": snapshot(0), "after": snapshot(1).replace("miles_warm_read_integrity_failures_total 1", "miles_warm_read_integrity_failures_total 0")} for i, p in enumerate(("cloud", "cloud", "edge"))],
        "statistics": {"physical_complete": True, "collection_started_at": "2026-11-04T00:00:25Z", "as_of": "2026-11-04T00:00:28Z", "bytes_logical": 100, "bytes_unique_raw": 80, "bytes_unique_stored": 50, "bytes_stored": 100},
    } for n in ("cloud", "edge")}
    return runs, observation


class PilotTests(unittest.TestCase):
    def test_measured_body_routes_and_ratios_remain_distinct(self):
        runs, observations = fixture()
        runs["cloud"]["secret"] = "never-copy-secret"
        result = pilot.build_report(runs, observations)
        self.assertEqual(result["status"], "local_contract_only")
        self.assertEqual(result["put_throughput_vs_direct"]["cloud"], 0.5)
        self.assertEqual(result["observations"]["cloud"]["retained_statistics"]["dedup_ratio"], 1.25)
        self.assertEqual(result["observations"]["cloud"]["process_counter_deltas"]["miles_gateway_s3_request_body_bytes_total"], 3)
        self.assertNotIn("never-copy-secret", str(result))
        self.assertFalse(result["mvp_threshold_applied"])

    def test_mismatch_restart_stale_incomplete_and_integrity_are_rejected(self):
        mutations = [
            lambda r, o: r["edge"].update(manifest_sha256="4" * 64),
            lambda r, o: r["edge"]["client"].update(executable_sha256="5" * 64),
            lambda r, o: r["cloud"]["phases"][0].update(failed=1),
            lambda r, o: r["cloud"]["samples"][0].update(sha256=""),
            lambda r, o: r["cloud"]["http_body_bytes"].clear(),
            lambda r, o: r["cloud"]["declared_environment"].update(encryption="different"),
            lambda r, o: o["cloud"]["nodes"][0].update(after=o["cloud"]["nodes"][0]["after"].replace("start_time_seconds 1", "start_time_seconds 2")),
            lambda r, o: o["cloud"]["nodes"][0].update(after=o["cloud"]["nodes"][0]["after"].replace("observation_age_seconds 0", "observation_age_seconds 60")),
            lambda r, o: o["cloud"]["nodes"][0].update(after=o["cloud"]["nodes"][0]["after"].replace("integrity_failures_total 0", "integrity_failures_total 1")),
            lambda r, o: o["cloud"]["nodes"].pop(),
            lambda r, o: o["cloud"]["statistics"].update(physical_complete=False),
            lambda r, o: o["cloud"]["statistics"].update(collection_started_at="2026-11-04T00:00:00Z"),
            lambda r, o: o["cloud"].update(before_at="2026-11-04T00:00:11Z"),
        ]
        for mutation in mutations:
            with self.subTest(mutation=mutation):
                runs, observations = fixture()
                mutation(runs, observations)
                with self.assertRaises(ValueError):
                    pilot.build_report(runs, observations)
