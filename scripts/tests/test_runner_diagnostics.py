import json
import unittest

from scripts.runner_diagnostics import (
    build_failed_window_payload,
    build_node_telemetry_snapshot,
    correlate_failures,
    execute_with_runcommand_fallback,
    parse_chrony_tracking,
    parse_gateway_journal_lines,
    parse_prometheus_metrics,
    sanitize_benchmark_failure,
)


class RunnerDiagnosticsTests(unittest.TestCase):
    def test_json_and_text_journal_parsing(self):
        json_line = json.dumps({
            "time": "2026-11-05T14:31:54Z",
            "level": "WARN",
            "msg": "s3 service unavailable",
            "request_id": "F4D42F0CD2CD743E",
            "node_id": "cloud-b",
            "method": "PUT",
            "operation": "PutObject",
            "stage": "prepare",
            "failure_class": "backend_unavailable"
        })
        text_line = "2026/11/05 14:32:00 ERROR s3 server failure request_id=3EE76450EAC2C164 node_id=cloud-a method=PUT operation=PutObject stage=credential_lookup failure_class=backend_unavailable"

        raw_journal = f"{json_line}\nsome random noise line\n{text_line}\n"
        parsed = parse_gateway_journal_lines(raw_journal)

        self.assertEqual(len(parsed), 2)
        self.assertEqual(parsed[0]["request_id"], "F4D42F0CD2CD743E")
        self.assertEqual(parsed[0]["node_id"], "cloud-b")
        self.assertEqual(parsed[0]["stage"], "prepare")
        self.assertEqual(parsed[0]["failure_class"], "backend_unavailable")
        self.assertEqual(parsed[0]["msg"], "s3 service unavailable")

        self.assertEqual(parsed[1]["request_id"], "3EE76450EAC2C164")
        self.assertEqual(parsed[1]["node_id"], "cloud-a")
        self.assertEqual(parsed[1]["stage"], "credential_lookup")
        self.assertEqual(parsed[1]["failure_class"], "backend_unavailable")
        self.assertEqual(parsed[1]["msg"], "s3 server failure")

    def test_malformed_and_truncated_records(self):
        raw = (
            "{incomplete json\n"
            "{\"msg\": \"other message\", \"request_id\": \"NO_MATCH\"}\n"
            "{\"msg\": \"s3 server failure\", \"request_id\": \"VALID_REQ\", \"node_id\": \"cloud-a\", \"stage\": \"object_get_stream\", \"failure_class\": \"clock_uncertainty_exceeded\"}\n"
            "ERROR s3 server failure truncated_line_without_required_fields\n"
            "\n"
        )
        parsed = parse_gateway_journal_lines(raw)
        self.assertEqual(len(parsed), 1)
        self.assertEqual(parsed[0]["request_id"], "VALID_REQ")
        self.assertEqual(parsed[0]["stage"], "object_get_stream")
        self.assertEqual(parsed[0]["failure_class"], "clock_uncertainty_exceeded")

    def test_ssm_session_manager_fallback_to_runcommand(self):
        def failing_session():
            raise RuntimeError("SSM Session Manager error: Standard_Stream not found")

        def successful_runcommand():
            return {"exit_code": 0, "stdout": "metrics_captured", "status": "Success"}

        result = execute_with_runcommand_fallback(failing_session, successful_runcommand)
        self.assertEqual(result["status"], "Success")
        self.assertEqual(result["stdout"], "metrics_captured")

    def test_session_manager_unrelated_exception_not_suppressed(self):
        def unexpected_error():
            raise ValueError("bad configuration parameter")

        def runcommand_fallback():
            return {"status": "should_not_run"}

        with self.assertRaises(ValueError):
            execute_with_runcommand_fallback(unexpected_error, runcommand_fallback)

    def test_correlate_failures_complete_evidence(self):
        client_failures = [
            {"request_id": "REQ-79-STREAM", "http_status": 200},
            {"request_id": "REQ-81-GATE", "http_status": 500},
            {"request_id": "ALB-TIMEOUT-503", "http_status": 503},
        ]
        gateway_records = [
            {"request_id": "REQ-79-STREAM", "node_id": "cloud-a", "stage": "object_get_stream", "failure_class": "clock_uncertainty_exceeded"},
            {"request_id": "REQ-81-GATE", "node_id": "cloud-a", "stage": "object_get", "failure_class": "clock_uncertainty_exceeded"},
        ]
        node_status = {"cloud-a": "ok", "cloud-b": "ok", "edge-a": "ok"}

        correlated = correlate_failures(client_failures, gateway_records, node_status=node_status)
        self.assertEqual(len(correlated), 3)

        self.assertEqual(correlated[0]["source"], "gateway")
        self.assertEqual(correlated[0]["stage"], "object_get_stream")
        self.assertEqual(correlated[0]["failure_class"], "clock_uncertainty_exceeded")

        self.assertEqual(correlated[1]["source"], "gateway")
        self.assertEqual(correlated[1]["stage"], "object_get")
        self.assertEqual(correlated[1]["failure_class"], "clock_uncertainty_exceeded")

        # When collector succeeded on all nodes, unjoined 503 is truly upstream ALB
        self.assertEqual(correlated[2]["source"], "upstream_no_gateway_request")

    def test_correlate_failures_incomplete_evidence_marks_uncollected(self):
        client_failures = [
            {"request_id": "ALB-503-UNKNOWN", "http_status": 503},
        ]
        gateway_records = []
        # Node collector failed on cloud-a!
        node_status = {"cloud-a": "error", "cloud-b": "ok"}

        correlated = correlate_failures(client_failures, gateway_records, node_status=node_status)
        self.assertEqual(len(correlated), 1)
        # MUST NOT claim upstream when node journal was not successfully collected
        self.assertEqual(correlated[0]["source"], "uncollected_evidence")

    def test_sanitize_benchmark_failure_protects_secrets(self):
        mock_result = {
            "status": "failed",
            "error": "some benchmark error",
            "phases": {
                "verify-get": {
                    "samples": [
                        {
                            "request_id": "REQ-FAIL-1",
                            "status_code": 500,
                            "error": "InternalError",
                            "secret_token": "SHOULD_NOT_BE_EXPOSED",
                            "bytes_read": 1234,
                            "duration_ms": 45.2,
                        },
                        {
                            "request_id": "REQ-PASS-2",
                            "status_code": 200,
                            "bytes_read": 8388608,
                        }
                    ]
                }
            }
        }
        sanitized = sanitize_benchmark_failure(mock_result)
        self.assertEqual(sanitized["status"], "failed")
        self.assertEqual(len(sanitized["failures"]), 1)
        f0 = sanitized["failures"][0]
        self.assertEqual(f0["request_id"], "REQ-FAIL-1")
        self.assertEqual(f0["http_status"], 500)
        self.assertEqual(f0["bytes_read"], 1234)
        self.assertNotIn("secret_token", f0)

    def test_sanitize_benchmark_failure_toplevel_samples(self):
        # warmbench outputs samples at the root level rather than phases dict
        mock_result = {
            "status": "failed",
            "samples": [
                {
                    "phase": "backup30",
                    "request_id": "REQ-WARMBENCH-FAIL",
                    "status_code": 500,
                    "error": "InternalError",
                    "secret_token": "SECRET_DATA",
                    "bytes_read": 0,
                    "duration_ms": 12.5,
                },
                {
                    "phase": "backup30",
                    "request_id": "REQ-WARMBENCH-PASS",
                    "status_code": 200,
                    "bytes_read": 1024,
                }
            ]
        }
        sanitized = sanitize_benchmark_failure(mock_result)
        self.assertEqual(sanitized["status"], "failed")
        self.assertEqual(len(sanitized["failures"]), 1)
        f0 = sanitized["failures"][0]
        self.assertEqual(f0["phase"], "backup30")
        self.assertEqual(f0["request_id"], "REQ-WARMBENCH-FAIL")
        self.assertEqual(f0["http_status"], 500)
        self.assertNotIn("secret_token", f0)

    def test_parse_prometheus_metrics(self):
        raw = (
            "# HELP miles_ready Readiness gauge\n"
            "# TYPE miles_ready gauge\n"
            "miles_ready 1\n"
            "miles_s3_errors_total{stage=\"writer_admission\"} 7\n"
            "process_resident_memory_bytes 12345678\n"
            "malformed_line\n"
        )
        parsed = parse_prometheus_metrics(raw)
        self.assertEqual(parsed["miles_ready"], 1)
        self.assertEqual(parsed["miles_s3_errors_total{stage=\"writer_admission\"}"], 7)
        self.assertEqual(parsed["process_resident_memory_bytes"], 12345678)
        self.assertNotIn("malformed_line", parsed)

    def test_build_failed_window_payload_with_telemetry(self):
        client_res = {
            "status": "failed",
            "samples": [
                {"phase": "put", "request_id": "REQ-1", "status_code": 500}
            ]
        }
        node_journals = [
            {
                "alias": "cloud-a",
                "id": "i-12345",
                "lines": [
                    "{\"msg\": \"s3 server failure\", \"request_id\": \"REQ-1\", \"node_id\": \"cloud-a\", \"stage\": \"writer_admission\", \"failure_class\": \"clock_uncertainty_exceeded\"}"
                ]
            }
        ]
        node_status = {"cloud-a": "ok"}
        before_metrics = {"cloud-a": {"miles_ready": 1}}
        after_metrics = {"cloud-a": {"miles_ready": 0}}

        payload = build_failed_window_payload(
            name="test-trial",
            captured_at="2026-11-06T13:30:00Z",
            client_result=client_res,
            node_journals=node_journals,
            node_status=node_status,
            before_metrics=before_metrics,
            after_metrics=after_metrics,
        )

        self.assertEqual(payload["name"], "test-trial")
        self.assertEqual(len(payload["correlated_failures"]), 1)
        self.assertEqual(payload["correlated_failures"][0]["stage"], "writer_admission")
        self.assertEqual(payload["telemetry"]["before"], before_metrics)
        self.assertEqual(payload["telemetry"]["after"], after_metrics)

    def test_parse_chrony_tracking_csv_bounds(self):
        # 169.254.169.123 in hex is A9FEA97B, interval 16.0s -> max allowed age is 34.0s
        now_unix = 1730900000.0
        ref_time_healthy = now_unix - 10.0 # 10s age < 34s
        csv_healthy = f"A9FEA97B,169.254.169.123,3,{ref_time_healthy:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        parsed = parse_chrony_tracking(csv_healthy, now_unix=now_unix)
        self.assertEqual(parsed["status"], "ok")
        self.assertEqual(parsed["reference_id"], "A9FEA97B")
        self.assertEqual(parsed["stratum"], 3)
        self.assertEqual(parsed["interval_seconds"], 16.0)
        self.assertEqual(parsed["max_allowed_age_seconds"], 34.0)
        self.assertEqual(parsed["sample_age_seconds"], 10.0)
        self.assertFalse(parsed["uncertainty_exceeded"])

        # Stale sample: age = 45s > 34s
        ref_time_stale = now_unix - 45.0
        csv_stale = f"A9FEA97B,169.254.169.123,3,{ref_time_stale:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"
        parsed_stale = parse_chrony_tracking(csv_stale, now_unix=now_unix)
        self.assertEqual(parsed_stale["status"], "ok")
        self.assertTrue(parsed_stale["uncertainty_exceeded"])

        # Not synchronised leap status
        csv_unsynced = f"A9FEA97B,169.254.169.123,3,{ref_time_healthy:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Not synchronised\n"
        parsed_unsynced = parse_chrony_tracking(csv_unsynced, now_unix=now_unix)
        self.assertTrue(parsed_unsynced["uncertainty_exceeded"])

        # Invalid stratum (0 or 16)
        csv_bad_stratum = f"A9FEA97B,169.254.169.123,16,{ref_time_healthy:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"
        parsed_bad_stratum = parse_chrony_tracking(csv_bad_stratum, now_unix=now_unix)
        self.assertTrue(parsed_bad_stratum["uncertainty_exceeded"])

    def test_sanitize_benchmark_failure_detailed_classification(self):
        client_result = {
            "status": "failed",
            "samples": [
                # 1. Pre-header dial failure
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "http_status": 0,
                    "error": "dial tcp: connection refused",
                    "request_id": "",
                    "payload_bytes": 0,
                    "latency_ns": 1200000,
                },
                # 2. Post-header stream abort (200 with unexpected EOF)
                {
                    "phase": "verify-get",
                    "operation": "GetObject",
                    "http_status": 200,
                    "error": "unexpected EOF in chunk data",
                    "request_id": "REQ-STREAM-ABORT",
                    "payload_bytes": 4096,
                    "latency_ns": 15000000,
                },
                # 3. Gateway error (500 with request_id)
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "http_status": 500,
                    "error": "InternalError",
                    "request_id": "REQ-GATEWAY-500",
                    "payload_bytes": 0,
                    "latency_ns": 8000000,
                },
                # 4. Upstream error (503 without gateway request_id)
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "http_status": 503,
                    "error": "Service Unavailable",
                    "request_id": "",
                    "payload_bytes": 0,
                    "latency_ns": 30000000000,
                },
                # 5. Success sample (should NOT appear in failures)
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "http_status": 200,
                    "request_id": "REQ-SUCCESS",
                    "payload_bytes": 1048576,
                    "latency_ns": 4500000,
                }
            ]
        }

        sanitized = sanitize_benchmark_failure(client_result)
        failures = sanitized["failures"]
        self.assertEqual(len(failures), 4)

        self.assertEqual(failures[0]["failure_type"], "pre_header_error")
        self.assertEqual(failures[0]["duration_ms"], 1.2)

        self.assertEqual(failures[1]["failure_type"], "post_header_stream_abort")
        self.assertEqual(failures[1]["request_id"], "REQ-STREAM-ABORT")
        self.assertEqual(failures[1]["bytes_read"], 4096)

        self.assertEqual(failures[2]["failure_type"], "gateway_error")
        self.assertEqual(failures[2]["request_id"], "REQ-GATEWAY-500")

        self.assertEqual(failures[3]["failure_type"], "upstream_error")

    def test_build_node_telemetry_snapshot(self):
        metrics_raw = "miles_ready 1\nprocess_resident_memory_bytes 1000\n"
        chrony_raw = "A9FEA97B,169.254.169.123,3,100.0,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n"

        snap = build_node_telemetry_snapshot(
            alias="cloud-a",
            profile="cloud",
            metrics_raw=metrics_raw,
            chrony_raw=chrony_raw,
            readyz_status=200,
            captured_at="2026-11-06T15:00:00Z",
            now_unix=105.0
        )
        self.assertEqual(snap["alias"], "cloud-a")
        self.assertEqual(snap["profile"], "cloud")
        self.assertEqual(snap["status"], "ok")
        self.assertEqual(snap["readyz"], 200)
        self.assertEqual(snap["metrics"]["miles_ready"], 1)
        self.assertEqual(snap["clock"]["reference_id"], "A9FEA97B")
        self.assertFalse(snap["clock"]["uncertainty_exceeded"])

        # Error snapshot
        snap_err = build_node_telemetry_snapshot(
            alias="cloud-b",
            profile="cloud",
            error="Connection timed out",
            captured_at="2026-11-06T15:00:00Z"
        )
        self.assertEqual(snap_err["status"], "error")
        self.assertIn("Connection timed out", snap_err["error"])

    def test_build_failed_window_payload_structured_provenance(self):
        client_res = {
            "status": "failed",
            "samples": [
                {"phase": "put", "request_id": "REQ-A", "status_code": 500},
                {"phase": "put", "request_id": "REQ-B", "status_code": 503}
            ]
        }
        node_journals = [
            {
                "alias": "cloud-a",
                "lines": [
                    "{\"msg\": \"s3 server failure\", \"request_id\": \"REQ-A\", \"node_id\": \"cloud-a\", \"stage\": \"writer_admission\", \"failure_class\": \"clock_uncertainty_exceeded\"}"
                ]
            }
        ]
        # cloud-b had connection error
        node_status = {"cloud-a": "ok", "cloud-b": "error"}
        before_telemetry = {
            "captured_at": "2026-11-06T13:20:00Z",
            "nodes": [{"alias": "cloud-a", "status": "ok"}, {"alias": "cloud-b", "status": "ok"}]
        }
        after_telemetry = {
            "captured_at": "2026-11-06T13:25:00Z",
            "nodes": [{"alias": "cloud-a", "status": "ok"}, {"alias": "cloud-b", "status": "error"}]
        }

        payload = build_failed_window_payload(
            name="cloud-c1-t1",
            captured_at="2026-11-06T13:25:00Z",
            client_result=client_res,
            node_journals=node_journals,
            node_status=node_status,
            before_metrics=before_telemetry,
            after_metrics=after_telemetry,
            release_hash="da87fab12c8bc26bcee46f78eb24f0062a985880",
            utc_window={"before_at": "2026-11-06T13:20:00Z", "after_at": "2026-11-06T13:25:00Z"},
            schema_version=1
        )

        self.assertEqual(payload["schema_version"], 1)
        self.assertEqual(payload["release_hash"], "da87fab12c8bc26bcee46f78eb24f0062a985880")
        self.assertEqual(payload["utc_window"]["before_at"], "2026-11-06T13:20:00Z")
        self.assertEqual(payload["utc_window"]["after_at"], "2026-11-06T13:25:00Z")
        self.assertEqual(payload["telemetry"]["before"], before_telemetry)
        self.assertEqual(payload["telemetry"]["after"], after_telemetry)

        # REQ-A matched gateway journal on cloud-a
        cor = payload["correlated_failures"]
        self.assertEqual(cor[0]["request_id"], "REQ-A")
        self.assertEqual(cor[0]["source"], "gateway")
        self.assertEqual(cor[0]["failure_class"], "clock_uncertainty_exceeded")

        # REQ-B was unjoined, BUT cloud-b had an error collecting journal -> MUST be uncollected_evidence!
        self.assertEqual(cor[1]["request_id"], "REQ-B")
        self.assertEqual(cor[1]["source"], "uncollected_evidence")


if __name__ == "__main__":
    unittest.main()
