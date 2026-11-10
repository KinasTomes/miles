"""
Unit and integration tests for Edge Clock RCA Analyzer (Workstream R).
"""

import json
from pathlib import Path
import tempfile
import unittest

from scripts.edge_clock_rca import EdgeClockRCAAnalyzer


class EdgeClockRCATests(unittest.TestCase):
    def setUp(self):
        # Sample synthetic fixture for fast standalone unit testing
        self.sample_result = {
            "schema_version": 1,
            "status": "failed",
            "error": "operation_failed",
            "started_at": "2026-11-08T04:24:40.742775528Z",
            "finished_at": "2026-11-08T04:26:01.96976284Z",
            "samples": [
                {"phase": "preflight-list", "operation": "ListObjectsV2", "latency_ns": 65737626},
            ]
            + [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "file": f"file-{i:03d}.bin",
                    "payload_bytes": 8388608,
                    "latency_ns": 1500000000,
                    "http_status": 200,
                }
                for i in range(1, 47)
            ]
            + [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "file": "file-047.bin",
                    "payload_bytes": 8388608,
                    "latency_ns": 1355000000,
                    "http_status": 503,
                    "error": "ServiceUnavailable",
                    "request_id": "REQ-REAL-047",
                }
            ]
            + [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "file": f"file-{i:03d}.bin",
                    "payload_bytes": 8388608,
                    "latency_ns": 25000000,
                    "http_status": 503,
                    "error": "ServiceUnavailable",
                    "request_id": f"REQ-REAL-{i:03d}",
                }
                for i in range(48, 91)
            ],
        }

        self.sample_failed_window = {
            "name": "edge-c1-t1",
            "captured_at": "2026-11-08T04:26:02.223492+00:00",
            "node_journals": [
                {
                    "alias": "edge-a",
                    "lines": [
                        json.dumps({
                            "time": "2026-11-08T04:26:01.044147001Z",
                            "level": "WARN",
                            "msg": "s3 service unavailable",
                            "request_id": "REQ-REAL-047",
                            "stage": "prepare",
                            "failure_class": "clock_uncertainty_exceeded",
                        })
                    ]
                    + [
                        json.dumps({
                            "time": f"2026-11-08T04:26:01.{i*20:03d}000000Z",
                            "level": "WARN",
                            "msg": "s3 service unavailable",
                            "request_id": f"REQ-REAL-{i:03d}",
                            "stage": "writer_admission",
                            "failure_class": "clock_uncertainty_exceeded",
                        })
                        for i in range(48, 91)
                    ],
                }
            ],
            "telemetry": {
                "after": {
                    "nodes": {
                        "edge-a": {
                            "clock": {
                                "ref_time": 1794112023.372766,
                                "interval_seconds": 16.2,
                                "max_allowed_age_seconds": 34.4,
                                "sample_age_seconds": -61.1493,
                                "stratum": 4,
                                "leap_status": "Normal",
                                "offset_seconds": 1.124e-06,
                                "drift_ppm": 28.433,
                            }
                        }
                    }
                }
            },
        }

    def test_evidence_inventory_epistemological_separation(self):
        analyzer = EdgeClockRCAAnalyzer(self.sample_result, self.sample_failed_window)
        inv = analyzer.build_evidence_inventory()
        self.assertGreaterEqual(len(inv), 6)

        categories = {item["category"] for item in inv}
        self.assertIn("Observed", categories)
        self.assertIn("Correlated", categories)
        self.assertIn("Hypothesized", categories)
        self.assertIn("Missing", categories)

        for item in inv:
            self.assertTrue(item["artifact"])
            self.assertTrue(item["details"])
            self.assertTrue(item["provenance"])

    def test_request_mapping_and_pseudonym_masking(self):
        analyzer = EdgeClockRCAAnalyzer(self.sample_result, self.sample_failed_window)
        reqs = analyzer.map_requests()
        self.assertEqual(len(reqs), 90)

        # 46 passes
        passes = [r for r in reqs if r["status"] == "PASS"]
        self.assertEqual(len(passes), 46)
        for p in passes:
            self.assertEqual(p["http_status"], 200)
            self.assertEqual(p["stage"], "committed")
            self.assertTrue(p["pseudonym"].startswith("REQ-EDGE-"))

        # 44 failures
        fails = [r for r in reqs if r["status"] == "FAIL"]
        self.assertEqual(len(fails), 44)

        # PUT #47 is first failure at stage prepare
        put47 = reqs[46]
        self.assertEqual(put47["ordinal"], 47)
        self.assertEqual(put47["pseudonym"], "REQ-EDGE-047")
        self.assertEqual(put47["status"], "FAIL")
        self.assertEqual(put47["stage"], "prepare")
        self.assertEqual(put47["failure_class"], "clock_uncertainty_exceeded")

        # PUT #48 is at stage writer_admission
        put48 = reqs[47]
        self.assertEqual(put48["ordinal"], 48)
        self.assertEqual(put48["pseudonym"], "REQ-EDGE-048")
        self.assertEqual(put48["stage"], "writer_admission")
        self.assertEqual(put48["failure_class"], "clock_uncertainty_exceeded")

    def test_timeline_cross_correlation(self):
        analyzer = EdgeClockRCAAnalyzer(self.sample_result, self.sample_failed_window)
        timeline = analyzer.analyze_timeline()

        self.assertIn("success_window", timeline)
        self.assertEqual(timeline["success_window"]["put_count"], 46)
        self.assertAlmostEqual(timeline["success_window"]["duration_seconds"], 78.85, places=1)

        self.assertIn("failure_window", timeline)
        self.assertEqual(timeline["failure_window"]["first_failure_put_ordinal"], 47)
        self.assertEqual(timeline["failure_window"]["first_failure_stage"], "prepare")
        self.assertEqual(timeline["failure_window"]["total_rejections"], 44)
        self.assertIn("explanation", timeline["telemetry_probes"])

    def test_failure_taxonomy_classification(self):
        analyzer = EdgeClockRCAAnalyzer(self.sample_result, self.sample_failed_window)
        taxonomy = analyzer.classify_failure_taxonomy()
        self.assertEqual(len(taxonomy), 5)

        codes = {t["failure_code"]: t for t in taxonomy}
        self.assertIn("sample_age_exceeded", codes)
        self.assertIn("chrony_command_duration_exceeded_1s", codes)
        self.assertIn("horizon_uncertainty_exceeded_2s", codes)
        self.assertIn("ntp_unsync_leap_stratum", codes)
        self.assertIn("err_clock_unavailable", codes)

        self.assertEqual(codes["err_clock_unavailable"]["verdict"], "CONFIRMED")
        self.assertIn("HYPOTHESIS", codes["sample_age_exceeded"]["verdict"])
        self.assertIn("HYPOTHESIS", codes["chrony_command_duration_exceeded_1s"]["verdict"])
        self.assertIn("UNLIKELY", codes["horizon_uncertainty_exceeded_2s"]["verdict"])

    def test_deterministic_simulation_missed_ntp_poll(self):
        # Under threshold (fresh)
        fresh = EdgeClockRCAAnalyzer.simulate_missed_ntp_poll(interval=16.0, current_age=30.0)
        self.assertFalse(fresh["uncertainty_exceeded"])
        self.assertEqual(fresh["error_class"], "ok")
        self.assertEqual(fresh["max_allowed_age_seconds"], 34.0)

        # Over threshold (missed poll at t=35s)
        stale = EdgeClockRCAAnalyzer.simulate_missed_ntp_poll(interval=16.0, current_age=35.0)
        self.assertTrue(stale["uncertainty_exceeded"])
        self.assertEqual(stale["error_class"], "sample_age_exceeded")

    def test_deterministic_simulation_chronyc_command_latency(self):
        # Normal fast command (< 1.0s)
        fast = EdgeClockRCAAnalyzer.simulate_chronyc_command_latency(0.045)
        self.assertFalse(fast["timed_out"])
        self.assertEqual(fast["error_class"], "ok")

        # Stalled command (> 1.0s)
        slow = EdgeClockRCAAnalyzer.simulate_chronyc_command_latency(1.250)
        self.assertTrue(slow["timed_out"])
        self.assertEqual(slow["error_class"], "chrony_command_duration_exceeded_1s")

    def test_deterministic_simulation_sequential_collector_skew(self):
        node_delays = [
            ("cloud-a", 20.4851),
            ("cloud-b", 35.7872),
            ("edge-a", 4.8770),
        ]
        res = EdgeClockRCAAnalyzer.simulate_sequential_collector_skew(node_delays)

        # cloud-a naive age is negative (~ -20.48s)
        self.assertAlmostEqual(res["cloud-a"]["naive_calculated_age"], -20.4851, places=3)
        self.assertTrue(res["cloud-a"]["is_negative_artifact"])

        # cloud-b naive age is cumulative negative (~ -56.27s)
        self.assertAlmostEqual(res["cloud-b"]["naive_calculated_age"], -56.2723, places=3)
        self.assertTrue(res["cloud-b"]["is_negative_artifact"])

        # edge-a naive age is cumulative negative (~ -61.15s)
        self.assertAlmostEqual(res["edge-a"]["naive_calculated_age"], -61.1493, places=3)
        self.assertTrue(res["edge-a"]["is_negative_artifact"])

        # In all cases, true isolated age is zero or positive
        for node, data in res.items():
            self.assertEqual(data["true_isolated_age"], 0.0)

    def test_generate_and_export_rca_report(self):
        analyzer = EdgeClockRCAAnalyzer(self.sample_result, self.sample_failed_window)
        with tempfile.TemporaryDirectory() as tmpdir:
            out_md = Path(tmpdir) / "test-rca-report.md"
            analyzer.export_report(out_md)
            self.assertTrue(out_md.exists())

            content = out_md.read_text(encoding="utf-8")
            self.assertIn("# Edge Clock Stability & Uncertainty Exceeded RCA Follow-Up", content)
            self.assertIn("REQ-EDGE-047", content)
            self.assertIn("clock_uncertainty_exceeded", content)
            self.assertIn("Mathematical Explanation of Negative Sample Age", content)
            self.assertIn("```mermaid", content)
            self.assertIn("Root Cause Failure Taxonomy & Distinction Table", content)

    def test_real_live_evidence_files_if_present(self):
        result_path = Path("/home/ubuntu/.local/state/miles/dynamodb-pilot/recheck/live-run-20261108-0345/edge-c1-t1-result.json")
        failed_window_path = Path("/home/ubuntu/.local/state/miles/dynamodb-pilot/recheck/live-run-20261108-0345/edge-c1-t1-failed-window.json")

        if result_path.exists() and failed_window_path.exists():
            analyzer = EdgeClockRCAAnalyzer.from_files(result_path, failed_window_path)
            reqs = analyzer.map_requests()
            self.assertEqual(len(reqs), 90)
            self.assertEqual(reqs[46]["pseudonym"], "REQ-EDGE-047")
            self.assertEqual(reqs[46]["status"], "FAIL")
            self.assertEqual(reqs[46]["stage"], "prepare")
            self.assertEqual(reqs[47]["stage"], "writer_admission")

            inv = analyzer.build_evidence_inventory()
            self.assertGreaterEqual(len(inv), 6)


if __name__ == "__main__":
    unittest.main()
