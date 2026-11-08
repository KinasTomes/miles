import datetime
import json
import unittest

from scripts.runner_diagnostics import (
    CHRONY_PAYLOAD_BEGIN,
    CHRONY_PAYLOAD_END,
    build_failed_window_payload,
    build_node_telemetry_snapshot,
    collect_node_journals_safe,
    collect_node_telemetry_safe,
    correlate_failures,
    execute_with_runcommand_fallback,
    parse_chrony_response_payload,
    parse_chrony_tracking,
    parse_gateway_journal_lines,
    parse_prometheus_metrics,
    redact_sensitive_strings,
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
            {"request_id": "UNMATCHED-500", "http_status": 500},
            {"request_id": "UNPROVEN-503", "http_status": 503},
        ]
        gateway_records = [
            {"request_id": "REQ-79-STREAM", "node_id": "cloud-a", "stage": "object_get_stream", "failure_class": "clock_uncertainty_exceeded"},
            {"request_id": "REQ-81-GATE", "node_id": "cloud-a", "stage": "object_get", "failure_class": "clock_uncertainty_exceeded"},
        ]
        alb_evidence = [
            {"request_id": "ALB-TIMEOUT-503", "elb_status_code": 503, "target_status_code": "-"}
        ]
        node_status = {"cloud-a": "ok", "cloud-b": "ok", "edge-a": "ok"}

        correlated = correlate_failures(client_failures, gateway_records, node_status=node_status, alb_evidence=alb_evidence)
        self.assertEqual(len(correlated), 5)

        # Matched gateway errors
        self.assertEqual(correlated[0]["source"], "gateway")
        self.assertEqual(correlated[0]["stage"], "object_get_stream")
        self.assertEqual(correlated[0]["failure_class"], "clock_uncertainty_exceeded")
        self.assertEqual(correlated[0]["diagnostic_status"], "correlated_gateway_log")

        self.assertEqual(correlated[1]["source"], "gateway")
        self.assertEqual(correlated[1]["stage"], "object_get")
        self.assertEqual(correlated[1]["failure_class"], "clock_uncertainty_exceeded")
        self.assertEqual(correlated[1]["diagnostic_status"], "correlated_gateway_log")

        # Corroborated upstream ALB error with evidence
        self.assertEqual(correlated[2]["source"], "upstream_alb")
        self.assertEqual(correlated[2]["diagnostic_status"], "corroborated_alb_evidence")

        # Unmatched 500 MUST NEVER be attributed to upstream ALB
        self.assertEqual(correlated[3]["source"], "origin_unverified")
        self.assertEqual(correlated[3]["diagnostic_status"], "gateway_log_missing")

        # Unmatched 503 without ALB proof MUST NOT be falsely attributed to upstream
        self.assertEqual(correlated[4]["source"], "origin_unverified")
        self.assertEqual(correlated[4]["diagnostic_status"], "gateway_log_missing")

    def test_correlate_failures_unmatched_500_never_attributed_to_upstream(self):
        # Reproduce exact reviewer test case:
        correlated = correlate_failures(
            [{"request_id": "req-not-logged", "http_status": 500}],
            [],
            {"cloud-a": "ok", "cloud-b": "ok"}
        )
        self.assertEqual(len(correlated), 1)
        self.assertEqual(correlated[0]["source"], "origin_unverified")
        self.assertEqual(correlated[0]["diagnostic_status"], "gateway_log_missing")

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

    def test_sanitize_benchmark_failure_fail_fast_and_indeterminate(self):
        mock_result = {
            "status": "failed",
            "error": "operation_failed",
            "fail_fast": True,
            "stop_trigger": "clock_uncertainty_exceeded",
            "needs_reconciliation": True,
            "samples": [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-FAIL-FAST-INDET",
                    "http_status": 0,
                    "error": "deadline_exceeded",
                    "indeterminate": True,
                    "reconciliation": "needs_reconciliation",
                    "secret_token": "PRIVATE_VAL",
                    "duration_ms": 500.0,
                },
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-INFLIGHT-SUCCESS",
                    "http_status": 200,
                    "payload_bytes": 1024,
                }
            ]
        }
        sanitized = sanitize_benchmark_failure(mock_result)
        self.assertEqual(sanitized["status"], "failed")
        self.assertTrue(sanitized["fail_fast"])
        self.assertEqual(sanitized["stop_trigger"], "clock_uncertainty_exceeded")
        self.assertTrue(sanitized["needs_reconciliation"])
        self.assertEqual(len(sanitized["failures"]), 1)
        f0 = sanitized["failures"][0]
        self.assertEqual(f0["request_id"], "REQ-FAIL-FAST-INDET")
        self.assertTrue(f0.get("indeterminate"))
        self.assertEqual(f0.get("reconciliation"), "needs_reconciliation")
        self.assertNotIn("secret_token", f0)

    def test_untrusted_reconciliation_string_does_not_leak_secrets(self):
        """
        Regression test for P1/P2: verifies that untrusted reconciliation string
        containing signed tokens or query params is never echoed into sanitized evidence.
        """
        leaky_recon = "needs_reconciliation?token=SECRET_TEST_KEY_12345&X-Amz-Signature=TEST_SIGNATURE_9876"
        client_res = {
            "status": "failed",
            "error": "interrupted",
            "fail_fast": True,
            "stop_trigger": "interrupted",
            "needs_reconciliation": True,
            "samples": [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-LEAKY-RECON",
                    "http_status": 0,
                    "error": "deadline_exceeded",
                    "indeterminate": True,
                    "reconciliation": leaky_recon,
                }
            ]
        }
        sanitized = sanitize_benchmark_failure(client_res)
        dumped = json.dumps(sanitized)
        self.assertNotIn("SECRET_TEST_KEY_12345", dumped)
        self.assertNotIn("TEST_SIGNATURE_9876", dumped)
        self.assertEqual(sanitized["failures"][0]["reconciliation"], "unknown_reconciliation_state")
        self.assertEqual(sanitized["stop_trigger"], "interrupted")

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

        # High frequency drift: freq = 600.0 PPM (> 500 PPM bound)
        csv_high_drift = f"A9FEA97B,169.254.169.123,3,{ref_time_healthy:.4f},0.0001,0.0001,0.0002,600.0,0.01,0.05,0.001,0.001,16.0,Normal\n"
        parsed_high_drift = parse_chrony_tracking(csv_high_drift, now_unix=now_unix)
        self.assertTrue(parsed_high_drift["uncertainty_exceeded"])
        self.assertEqual(parsed_high_drift["rejection_reason"], "drift_ppm_exceeded_500")

        # Non-finite value (NaN at skew field parts[9])
        csv_nan_skew = f"A9FEA97B,169.254.169.123,3,{ref_time_healthy:.4f},0.0001,0.0001,0.0002,0.1,0.01,NaN,0.001,0.001,16.0,Normal\n"
        parsed_nan_skew = parse_chrony_tracking(csv_nan_skew, now_unix=now_unix)
        self.assertTrue(parsed_nan_skew["uncertainty_exceeded"])
        self.assertEqual(parsed_nan_skew["rejection_reason"], "non_finite_or_unparseable_metrics")

        # Non-finite value (+Inf at offset field parts[4])
        csv_inf_offset = f"A9FEA97B,169.254.169.123,3,{ref_time_healthy:.4f},+Inf,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"
        parsed_inf_offset = parse_chrony_tracking(csv_inf_offset, now_unix=now_unix)
        self.assertTrue(parsed_inf_offset["uncertainty_exceeded"])

        # Wide horizon uncertainty: errorSeconds + driftPPM*1e-6*2700 > 2.0s
        # e.g. offset = 1.8s, root_dispersion = 0.5s -> errorSeconds = 2.31s > 2s
        csv_wide_horizon = f"A9FEA97B,169.254.169.123,3,{ref_time_healthy:.4f},1.8,0.0001,0.0002,10.0,0.01,0.05,0.001,0.5,16.0,Normal\n"
        parsed_wide = parse_chrony_tracking(csv_wide_horizon, now_unix=now_unix)
        self.assertTrue(parsed_wide["uncertainty_exceeded"])
        self.assertEqual(parsed_wide["rejection_reason"], "horizon_uncertainty_exceeded_2s")

        # Missing now_unix fails closed safely
        parsed_csv_no_now = parse_chrony_tracking(csv_healthy, now_unix=None)
        self.assertTrue(parsed_csv_no_now["uncertainty_exceeded"])
        self.assertEqual(parsed_csv_no_now["rejection_reason"], "missing_current_timestamp_for_age_verification")

    def test_parse_chrony_tracking_text_bounds(self):
        # Sample healthy text tracking output
        healthy_text = """
Reference ID    : A9FEA97B (169.254.169.123)
Stratum         : 3
Ref time (UTC)  : Fri Nov 06 14:32:00 2026
System time     : 0.000100000 seconds slow of NTP time
Last offset     : +0.000100000 seconds
RMS offset      : 0.000200000 seconds
Frequency       : 0.100 ppm fast
Residual freq   : +0.010 ppm
Skew            : 0.050 ppm
Root delay      : 0.001000000 seconds
Root dispersion : 0.001000000 seconds
Update interval : 16.0 seconds
Leap status     : Normal
"""
        import datetime
        dt = datetime.datetime(2026, 11, 6, 14, 32, 10, tzinfo=datetime.timezone.utc)
        now_unix = dt.timestamp()

        # 1. Complete healthy text
        parsed = parse_chrony_tracking(healthy_text, now_unix=now_unix)
        self.assertEqual(parsed["status"], "ok")
        self.assertEqual(parsed["reference_id"], "A9FEA97B")
        self.assertEqual(parsed["stratum"], 3)
        self.assertEqual(parsed["interval_seconds"], 16.0)
        self.assertEqual(parsed["sample_age_seconds"], 10.0)
        self.assertFalse(parsed["uncertainty_exceeded"])

        # 2. Stale sample in text format (now is 50s later, max allowed age is 34s)
        stale_now = now_unix + 40.0
        parsed_stale = parse_chrony_tracking(healthy_text, now_unix=stale_now)
        self.assertTrue(parsed_stale["uncertainty_exceeded"])
        self.assertEqual(parsed_stale["rejection_reason"], "sample_age_exceeded")

        # 3. Incomplete text format (only Reference ID, Stratum, Leap status)
        incomplete_text = """
Reference ID    : A9FEA97B (169.254.169.123)
Stratum         : 3
Leap status     : Normal
"""
        parsed_incomplete = parse_chrony_tracking(incomplete_text, now_unix=now_unix)
        self.assertEqual(parsed_incomplete["status"], "unverified_text_format")
        self.assertTrue(parsed_incomplete["uncertainty_exceeded"])
        self.assertEqual(parsed_incomplete["rejection_reason"], "unverified_text_format_missing_drift_or_age")

        # 4. High drift in text format (600 ppm > 500 bound)
        high_drift_text = healthy_text.replace("0.100 ppm fast", "600.000 ppm fast")
        parsed_high_drift = parse_chrony_tracking(high_drift_text, now_unix=now_unix)
        self.assertTrue(parsed_high_drift["uncertainty_exceeded"])
        self.assertEqual(parsed_high_drift["rejection_reason"], "drift_ppm_exceeded_500")

        # 5. Missing current timestamp (now_unix is None) fails closed safely
        parsed_no_now = parse_chrony_tracking(healthy_text, now_unix=None)
        self.assertTrue(parsed_no_now["uncertainty_exceeded"])
        self.assertEqual(parsed_no_now["rejection_reason"], "missing_current_timestamp_for_age_verification")

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

    def test_parse_chrony_tracking_non_finite_and_invalid_timestamps(self):
        # Verify fail-closed behavior for NaN, Inf, non-numeric and negative values
        now_unix = 1730900000.0
        ref_time = now_unix - 5.0
        csv_healthy = f"A9FEA97B,169.254.169.123,3,{ref_time:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        # 1. NaN now_unix MUST fail-closed
        res_nan_now = parse_chrony_tracking(csv_healthy, now_unix=float("nan"))
        self.assertTrue(res_nan_now["uncertainty_exceeded"])
        self.assertEqual(res_nan_now["rejection_reason"], "non_finite_current_timestamp")

        # 2. Inf now_unix MUST fail-closed
        res_inf_now = parse_chrony_tracking(csv_healthy, now_unix=float("inf"))
        self.assertTrue(res_inf_now["uncertainty_exceeded"])
        self.assertEqual(res_inf_now["rejection_reason"], "non_finite_current_timestamp")

        # 3. String now_unix MUST fail-closed
        res_str_now = parse_chrony_tracking(csv_healthy, now_unix="invalid_timestamp")
        self.assertTrue(res_str_now["uncertainty_exceeded"])
        self.assertEqual(res_str_now["rejection_reason"], "non_finite_current_timestamp")

        # 4. NaN elapsed_seconds MUST fail-closed
        res_nan_elapsed = parse_chrony_tracking(csv_healthy, now_unix=now_unix, elapsed_seconds=float("nan"))
        self.assertTrue(res_nan_elapsed["uncertainty_exceeded"])
        self.assertEqual(res_nan_elapsed["rejection_reason"], "invalid_or_non_finite_elapsed_seconds")

        # 5. Negative elapsed_seconds MUST fail-closed
        res_neg_elapsed = parse_chrony_tracking(csv_healthy, now_unix=now_unix, elapsed_seconds=-1.0)
        self.assertTrue(res_neg_elapsed["uncertainty_exceeded"])
        self.assertEqual(res_neg_elapsed["rejection_reason"], "invalid_or_non_finite_elapsed_seconds")

    def test_collect_node_telemetry_safe_and_partial_failure(self):
        nodes = [
            {"alias": "cloud-a", "profile": "cloud"},
            {"alias": "cloud-b", "profile": "cloud"}
        ]

        def mock_fetcher(node):
            if node["alias"] == "cloud-a":
                return {
                    "metrics_raw": "miles_ready 1\n",
                    "chrony_raw": "A9FEA97B,169.254.169.123,3,100.0,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n",
                    "readyz_status": 200
                }
            raise RuntimeError("SSM command timed out")

        cluster_snap = collect_node_telemetry_safe(
            nodes=nodes,
            fetcher_fn=mock_fetcher,
            captured_at="2026-11-06T13:20:00Z",
            now_unix=105.0
        )

        self.assertEqual(cluster_snap["status"], "partial")
        self.assertEqual(cluster_snap["summary"]["total_nodes"], 2)
        self.assertEqual(cluster_snap["summary"]["healthy_nodes"], 1)
        self.assertEqual(cluster_snap["summary"]["failed_nodes"], 1)

        self.assertEqual(cluster_snap["nodes"]["cloud-a"]["status"], "ok")
        self.assertEqual(cluster_snap["nodes"]["cloud-a"]["metrics"]["miles_ready"], 1)
        self.assertEqual(cluster_snap["nodes"]["cloud-b"]["status"], "error")
        self.assertIn("SSM command timed out", cluster_snap["nodes"]["cloud-b"]["error"])

    def test_sequential_multi_node_probe_timestamps_prevent_negative_sample_age(self):
        """
        Regression test: verifies that sequential telemetry probing across multiple nodes
        evaluates clock sample age against each probe's own finish timestamp, completely
        preventing false negative sample ages (e.g. -20s, -56s, -61s) caused by sequential delays.
        Verifies fallback path derives now_unix from probe_finished_at when now_unix is omitted.
        """
        nodes = [
            {"alias": "cloud-a", "profile": "cloud"},
            {"alias": "cloud-b", "profile": "cloud"},
            {"alias": "edge-a", "profile": "edge"},
        ]

        # Simulate sequential execution where each node probe occurs at simulated intervals:
        # cloud-a probe runs at T=1730900015.0, ref_time = 1730900010.0 (age = 5.0s)
        # cloud-b probe runs at T=1730900045.0, ref_time = 1730900040.0 (age = 5.0s)
        # edge-a  probe runs at T=1730900105.0, ref_time = 1730900100.0 (age = 5.0s)
        simulated_times = {
            "cloud-a": (1730900010.0, 1730900015.0, 1.2),
            "cloud-b": (1730900040.0, 1730900045.0, 1.5),
            "edge-a": (1730900100.0, 1730900105.0, 2.0),
        }

        def mock_sequential_fetcher(node):
            alias = node["alias"]
            ref_t, finish_t, duration = simulated_times[alias]
            start_t = finish_t - duration
            csv_str = f"A9FEA97B,169.254.169.123,3,{ref_t:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"
            import datetime
            dt_start = datetime.datetime.fromtimestamp(start_t, datetime.timezone.utc).isoformat()
            dt_finish = datetime.datetime.fromtimestamp(finish_t, datetime.timezone.utc).isoformat()
            return {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": csv_str,
                "readyz_status": None,
                "probe_started_at": dt_start,
                "probe_finished_at": dt_finish,
                "probe_duration_seconds": duration,
                "chrony_elapsed_seconds": 0.05,
                # Note: now_unix is intentionally omitted to verify the fallback
                # correctly derives now_unix from probe_finished_at!
            }

        snap = collect_node_telemetry_safe(
            nodes=nodes,
            fetcher_fn=mock_sequential_fetcher,
            captured_at="2026-11-06T13:00:00Z",
            # now_unix intentionally None
        )

        self.assertEqual(snap["status"], "ok")
        self.assertIn("collection_started_at", snap)
        self.assertIn("collection_finished_at", snap)

        for alias in ["cloud-a", "cloud-b", "edge-a"]:
            node_snap = snap["nodes"][alias]
            self.assertEqual(node_snap["status"], "ok")
            self.assertIsNotNone(node_snap["probe_started_at"])
            self.assertIsNotNone(node_snap["probe_finished_at"])
            self.assertIsNotNone(node_snap["probe_duration_seconds"])
            clock = node_snap["clock"]
            self.assertEqual(clock["status"], "ok")
            self.assertFalse(clock["uncertainty_exceeded"])
            self.assertEqual(clock.get("time_provenance"), "probe_finished")
            # Sample age MUST be positive (~5.0s), NOT negative!
            self.assertGreaterEqual(clock["sample_age_seconds"], 0.0)
            self.assertAlmostEqual(clock["sample_age_seconds"], 5.0, delta=0.5)

    def test_chrony_slow_probe_increases_horizon_uncertainty(self):
        """
        Regression test: verifies that slow probe execution properly factors into
        uncertainty bounds:
        1. When host chronyc execution exceeds 1.0s, it triggers chrony_command_duration_exceeded_1s.
        2. When host chronyc is valid (< 1.0s) but root dispersion / drift causes horizon uncertainty
           to exceed 2.0s, it triggers horizon_uncertainty_exceeded_2s.
        """
        now_unix = 1730900000.0
        ref_time = now_unix - 5.0
        csv_healthy = f"A9FEA97B,169.254.169.123,3,{ref_time:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        # Fast probe: elapsed = 0.05s -> OK
        fast_res = parse_chrony_tracking(csv_healthy, now_unix=now_unix, elapsed_seconds=0.05)
        self.assertEqual(fast_res["status"], "ok")
        self.assertFalse(fast_res["uncertainty_exceeded"])
        self.assertEqual(fast_res["runtime_equivalence"], "verified")

        # Slow probe on host >= 1.0s -> chrony_command_duration_exceeded_1s
        slow_cmd_res = parse_chrony_tracking(csv_healthy, now_unix=now_unix, elapsed_seconds=1.25)
        self.assertEqual(slow_cmd_res["status"], "ok")
        self.assertTrue(slow_cmd_res["uncertainty_exceeded"])
        self.assertEqual(slow_cmd_res["rejection_reason"], "chrony_command_duration_exceeded_1s")

        # Probe elapsed < 1.0s (0.5s) with high root dispersion (1.6s) -> horizon_uncertainty > 2.0s
        csv_high_dispersion = f"A9FEA97B,169.254.169.123,3,{ref_time:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,1.6,16.0,Normal\n"
        slow_res = parse_chrony_tracking(csv_high_dispersion, now_unix=now_unix, elapsed_seconds=0.5)
        self.assertEqual(slow_res["status"], "ok")
        self.assertTrue(slow_res["uncertainty_exceeded"])
        self.assertEqual(slow_res["rejection_reason"], "horizon_uncertainty_exceeded_2s")

    def test_chrony_future_timestamp_fails_closed(self):
        """
        Regression test: verifies that if ref_time is in the future by > 2.0s (clock step backwards
        or invalid observation), parse_chrony_tracking strictly fails closed with sample_age_exceeded.
        """
        now_unix = 1730900000.0
        # ref_time is 10.0s in the future relative to now_unix -> age = -10.0s < -2.0s
        ref_time_future = now_unix + 10.0
        csv_future = f"A9FEA97B,169.254.169.123,3,{ref_time_future:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        future_res = parse_chrony_tracking(csv_future, now_unix=now_unix)
        self.assertEqual(future_res["status"], "ok")
        self.assertTrue(future_res["uncertainty_exceeded"])
        self.assertEqual(future_res["rejection_reason"], "sample_age_exceeded")
        self.assertLess(future_res["sample_age_seconds"], -2.0)

    def test_chrony_stale_and_recovery_sequence(self):
        """
        Regression test: verifies that a node transitioning from stale (age > 34s) to fresh (age = 5s)
        properly reflects self-recovery across consecutive telemetry captures.
        """
        node = [{"alias": "edge-a", "profile": "edge"}]
        now_unix = 1730900000.0

        # Probe 1: Stale (age = 45s > 34s max allowed)
        stale_ref = now_unix - 45.0
        csv_stale = f"A9FEA97B,169.254.169.123,3,{stale_ref:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"
        snap1 = collect_node_telemetry_safe(
            nodes=node,
            fetcher_fn=lambda n: {"metrics_raw": "miles_ready 1\n", "chrony_raw": csv_stale, "now_unix": now_unix},
        )
        self.assertEqual(snap1["status"], "partial")
        self.assertTrue(snap1["nodes"]["edge-a"]["clock"]["uncertainty_exceeded"])
        self.assertEqual(snap1["nodes"]["edge-a"]["clock"]["rejection_reason"], "sample_age_exceeded")

        # Probe 2: Recovered (age = 5s <= 34s)
        now_unix_2 = now_unix + 20.0
        fresh_ref = now_unix_2 - 5.0
        csv_fresh = f"A9FEA97B,169.254.169.123,3,{fresh_ref:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"
        snap2 = collect_node_telemetry_safe(
            nodes=node,
            fetcher_fn=lambda n: {"metrics_raw": "miles_ready 1\n", "chrony_raw": csv_fresh, "now_unix": now_unix_2},
        )
        self.assertEqual(snap2["status"], "ok")
        self.assertFalse(snap2["nodes"]["edge-a"]["clock"]["uncertainty_exceeded"])
        self.assertEqual(snap2["nodes"]["edge-a"]["clock"]["sample_age_seconds"], 5.0)

    def test_parse_chrony_response_payload_structure_and_error_handling(self):
        """
        Verifies that parse_chrony_response_payload parses structured host envelopes,
        safely handles missing envelopes, and tolerates corrupted JSON payloads.
        """
        # 1. Valid envelope with host timings
        host_unix = 1730900000.0
        host_iso = datetime.datetime.fromtimestamp(host_unix, datetime.timezone.utc).isoformat()
        payload_dict = {
            "version": 1,
            "host_observed_at": host_iso,
            "host_observed_unix": host_unix,
            "host_chrony_elapsed_seconds": 0.0035,
            "returncode": 0,
            "stdout": "A9FEA97B,169.254.169.123,3,1730899995.0,0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n",
            "stderr": "",
        }
        wrapped = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(payload_dict)}\n{CHRONY_PAYLOAD_END}\n"
        res = parse_chrony_response_payload(wrapped)
        self.assertEqual(res["chrony_raw"], payload_dict["stdout"])
        self.assertEqual(res["host_observed_at"], host_iso)
        self.assertEqual(res["host_observed_unix"], host_unix)
        self.assertEqual(res["host_chrony_elapsed_seconds"], 0.0035)
        self.assertTrue(res["runtime_elapsed_verified"])
        self.assertEqual(res["runtime_equivalence"], "verified")
        self.assertTrue(res["envelope_valid"])
        self.assertIsNone(res["envelope_error"])
        self.assertEqual(res["returncode"], 0)

        # 2. Raw uninstrumented string without envelope
        raw_csv = payload_dict["stdout"]
        res_raw = parse_chrony_response_payload(raw_csv)
        self.assertEqual(res_raw["chrony_raw"], raw_csv)
        self.assertIsNone(res_raw["host_chrony_elapsed_seconds"])
        self.assertFalse(res_raw["runtime_elapsed_verified"])
        self.assertEqual(res_raw["runtime_equivalence"], "unverified")
        self.assertFalse(res_raw["envelope_valid"])
        self.assertEqual(res_raw["envelope_error"], "no_envelope_present")

        # 3. Corrupt JSON inside envelope fails safe to unverified
        corrupt = f"{CHRONY_PAYLOAD_BEGIN}\n{{invalid-json}}\n{CHRONY_PAYLOAD_END}\n"
        res_corrupt = parse_chrony_response_payload(corrupt)
        self.assertFalse(res_corrupt["runtime_elapsed_verified"])
        self.assertEqual(res_corrupt["runtime_equivalence"], "unverified")
        self.assertFalse(res_corrupt["envelope_valid"])
        self.assertEqual(res_corrupt["envelope_error"], "corrupted_payload_json")

        # 4. Non-finite or negative host elapsed
        bad_elapsed_dict = dict(payload_dict)
        bad_elapsed_dict["host_chrony_elapsed_seconds"] = -1.0
        bad_wrapped = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(bad_elapsed_dict)}\n{CHRONY_PAYLOAD_END}\n"
        res_bad = parse_chrony_response_payload(bad_wrapped)
        self.assertIsNone(res_bad["host_chrony_elapsed_seconds"])
        self.assertFalse(res_bad["runtime_elapsed_verified"])
        self.assertFalse(res_bad["envelope_valid"])
        self.assertEqual(res_bad["envelope_error"], "invalid_or_non_finite_host_elapsed")

        # 5. Non-zero returncode fails closed with empty raw output
        bad_rc_dict = dict(payload_dict)
        bad_rc_dict["returncode"] = 2
        bad_rc_wrapped = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(bad_rc_dict)}\n{CHRONY_PAYLOAD_END}\n"
        res_bad_rc = parse_chrony_response_payload(bad_rc_wrapped)
        self.assertFalse(res_bad_rc["envelope_valid"])
        self.assertEqual(res_bad_rc["envelope_error"], "chronyc_process_failed_rc_2")
        self.assertFalse(res_bad_rc["runtime_elapsed_verified"])
        self.assertEqual(res_bad_rc["runtime_equivalence"], "unverified")
        self.assertEqual(res_bad_rc["chrony_raw"], "")

        # 6. Unsupported schema version
        bad_ver_dict = dict(payload_dict)
        bad_ver_dict["version"] = 2
        bad_ver_wrapped = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(bad_ver_dict)}\n{CHRONY_PAYLOAD_END}\n"
        res_bad_ver = parse_chrony_response_payload(bad_ver_wrapped)
        self.assertFalse(res_bad_ver["envelope_valid"])
        self.assertEqual(res_bad_ver["envelope_error"], "unsupported_schema_version_2")

        # 7. Inconsistent paired UTC timestamps
        bad_skew_dict = dict(payload_dict)
        bad_skew_dict["host_observed_unix"] = host_unix + 50.0  # 50s discrepancy with host_observed_at
        bad_skew_wrapped = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(bad_skew_dict)}\n{CHRONY_PAYLOAD_END}\n"
        res_bad_skew = parse_chrony_response_payload(bad_skew_wrapped)
        self.assertFalse(res_bad_skew["envelope_valid"])
        self.assertEqual(res_bad_skew["envelope_error"], "inconsistent_paired_host_utc_timestamps")

        # 8. Empty stdout
        bad_out_dict = dict(payload_dict)
        bad_out_dict["stdout"] = "   \n"
        bad_out_wrapped = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(bad_out_dict)}\n{CHRONY_PAYLOAD_END}\n"
        res_bad_out = parse_chrony_response_payload(bad_out_wrapped)
        self.assertFalse(res_bad_out["envelope_valid"])
        self.assertEqual(res_bad_out["envelope_error"], "empty_chronyc_stdout")

    def test_host_observed_timestamp_provenance_and_payload_envelope(self):
        """
        Verifies that when a node returns host_observed_at / host_observed_unix,
        telemetry anchors clock sample age to the remote target host's own UTC observation time,
        marking time_provenance as 'host_observed'.
        """
        host_utc = 1730900000.0
        ref_time = host_utc - 4.5
        host_iso = datetime.datetime.fromtimestamp(host_utc, datetime.timezone.utc).isoformat()
        probe_start_iso = datetime.datetime.fromtimestamp(host_utc + 1.0, datetime.timezone.utc).isoformat()
        probe_finish_iso = datetime.datetime.fromtimestamp(host_utc + 3.0, datetime.timezone.utc).isoformat()
        payload_dict = {
            "version": 1,
            "host_observed_at": host_iso,
            "host_observed_unix": host_utc,
            "host_chrony_elapsed_seconds": 0.002,
            "returncode": 0,
            "stdout": f"A9FEA97B,169.254.169.123,3,{ref_time:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n",
        }
        wrapped = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(payload_dict)}\n{CHRONY_PAYLOAD_END}\n"

        nodes = [{"alias": "cloud-a", "profile": "cloud"}]
        snap = collect_node_telemetry_safe(
            nodes=nodes,
            fetcher_fn=lambda n: {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": wrapped,
                "probe_started_at": probe_start_iso,
                "probe_finished_at": probe_finish_iso,
                "probe_duration_seconds": 2.0,
                "transport_latency_seconds": 1.8,
            },
        )

        self.assertEqual(snap["status"], "ok")
        node_snap = snap["nodes"]["cloud-a"]
        clock = node_snap["clock"]
        self.assertEqual(clock["status"], "ok")
        self.assertFalse(clock["uncertainty_exceeded"])
        self.assertEqual(clock["time_provenance"], "host_observed")
        self.assertEqual(clock["runtime_equivalence"], "verified")
        self.assertEqual(clock["host_chrony_elapsed_seconds"], 0.002)
        self.assertEqual(node_snap["transport_latency_seconds"], 1.8)
        self.assertAlmostEqual(clock["sample_age_seconds"], 4.5, delta=0.1)

    def test_adversarial_slow_transport_latency_with_fast_remote_chronyc(self):
        """
        Adversarial test (P1): SSM transport latency is 2.5s, but remote host chronyc took
        only 0.003s. Telemetry MUST NOT conflate transport latency with clock uncertainty
        or trigger horizon_uncertainty_exceeded_2s.
        """
        host_now = 1730900000.0
        ref_time = host_now - 4.0
        csv_str = f"A9FEA97B,169.254.169.123,3,{ref_time:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        nodes = [{"alias": "cloud-a", "profile": "cloud"}]

        # Outer probe duration is 2.5s due to SSM Session Manager startup/network lag
        def slow_transport_fetcher(node):
            return {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": csv_str,
                "probe_started_at": "2026-11-08T07:00:00+00:00",
                "probe_finished_at": "2026-11-08T07:00:02.500+00:00",
                "probe_duration_seconds": 2.5,
                "transport_latency_seconds": 2.5,
                "host_chrony_elapsed_seconds": 0.003,
                "host_observed_unix": host_now,
                "runtime_elapsed_verified": True,
            }

        snap = collect_node_telemetry_safe(
            nodes=nodes,
            fetcher_fn=slow_transport_fetcher,
        )

        self.assertEqual(snap["status"], "ok")
        node_snap = snap["nodes"]["cloud-a"]
        clock = node_snap["clock"]
        self.assertEqual(clock["status"], "ok")
        self.assertFalse(clock["uncertainty_exceeded"])
        self.assertEqual(clock["runtime_equivalence"], "verified")
        self.assertEqual(clock["host_chrony_elapsed_seconds"], 0.003)
        self.assertEqual(node_snap["transport_latency_seconds"], 2.5)
        # Horizon uncertainty must be small (~0.015s), NOT > 2.0s!
        self.assertLess(clock["horizon_uncertainty_seconds"], 0.5)

    def test_truly_slow_remote_chronyc_on_host_fails_closed(self):
        """
        Regression test (P1): When host-local chronyc execution is genuinely slow
        (>= 1.0s bound matching Go runtime coordination.Chrony.Bounds), telemetry
        strictly fails closed with chrony_command_duration_exceeded_1s.
        """
        host_now = 1730900000.0
        ref_time = host_now - 4.0
        csv_str = f"A9FEA97B,169.254.169.123,3,{ref_time:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        nodes = [{"alias": "edge-a", "profile": "edge"}]

        def slow_host_chronyc_fetcher(node):
            return {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": csv_str,
                "probe_started_at": "2026-11-08T07:00:00+00:00",
                "probe_finished_at": "2026-11-08T07:00:01.500+00:00",
                "probe_duration_seconds": 1.5,
                "transport_latency_seconds": 0.2,
                "host_chrony_elapsed_seconds": 1.25, # Truly slow local chronyc on host!
                "host_observed_unix": host_now,
                "runtime_elapsed_verified": True,
            }

        snap = collect_node_telemetry_safe(
            nodes=nodes,
            fetcher_fn=slow_host_chronyc_fetcher,
        )

        self.assertEqual(snap["status"], "partial")
        node_snap = snap["nodes"]["edge-a"]
        clock = node_snap["clock"]
        self.assertEqual(clock["status"], "ok")
        self.assertTrue(clock["uncertainty_exceeded"])
        self.assertEqual(clock["rejection_reason"], "chrony_command_duration_exceeded_1s")
        self.assertEqual(clock["runtime_equivalence"], "verified")

    def test_missing_host_timing_envelope_marks_runtime_equivalence_unverified_without_false_alarm(self):
        """
        Regression test (P1): When host timing envelope is unavailable (raw CSV) and transport
        latency is high (2.5s), telemetry marks runtime_equivalence='unverified' without falsely
        asserting horizon_uncertainty_exceeded_2s.
        """
        now_unix = 1730900000.0
        ref_time = now_unix - 4.0
        raw_csv = f"A9FEA97B,169.254.169.123,3,{ref_time:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        nodes = [{"alias": "cloud-a", "profile": "cloud"}]

        def uninstrumented_fetcher(node):
            return {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": raw_csv, # raw CSV without host timing
                "probe_started_at": "2026-11-08T07:00:00+00:00",
                "probe_finished_at": "2026-11-08T07:00:02.500+00:00",
                "probe_duration_seconds": 2.5,
                "transport_latency_seconds": 2.5,
                "now_unix": now_unix,
            }

        snap = collect_node_telemetry_safe(
            nodes=nodes,
            fetcher_fn=uninstrumented_fetcher,
        )

        self.assertEqual(snap["status"], "ok")
        node_snap = snap["nodes"]["cloud-a"]
        clock = node_snap["clock"]
        self.assertEqual(clock["status"], "ok")
        self.assertFalse(clock["uncertainty_exceeded"])
        self.assertEqual(clock["runtime_equivalence"], "unverified")
        self.assertFalse(clock["runtime_elapsed_verified"])
        self.assertEqual(node_snap["transport_latency_seconds"], 2.5)
        # Uncertainty is NOT exceeded!
        self.assertLess(clock["horizon_uncertainty_seconds"], 1.0)

    def test_envelope_nonzero_returncode_marked_unverified_and_rejected(self):
        """
        Regression test (P1): When remote chronyc returns non-zero returncode (e.g. rc=2),
        envelope parser rejects stdout, marks envelope_valid=False, and downstream node
        telemetry snapshot strictly fails closed:
        - node status is 'partial' or 'error' (NEVER 'ok')
        - clock uncertainty_exceeded is True
        - rejection_reason reflects chronyc failure
        - runtime_equivalence is 'unverified'
        """
        host_unix = 1730900000.0
        host_iso = datetime.datetime.fromtimestamp(host_unix, datetime.timezone.utc).isoformat()
        payload = {
            "version": 1,
            "host_observed_at": host_iso,
            "host_observed_unix": host_unix,
            "host_chrony_elapsed_seconds": 0.005,
            "returncode": 2, # Process failed!
            "stdout": "A9FEA97B,169.254.169.123,3,1730899995.0,0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n",
            "stderr": "506 Cannot talk to daemon",
        }
        wrapped = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(payload)}\n{CHRONY_PAYLOAD_END}\n"

        nodes = [{"alias": "edge-a", "profile": "edge"}]
        snap = collect_node_telemetry_safe(
            nodes=nodes,
            fetcher_fn=lambda n: {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": wrapped,
                "probe_started_at": host_iso,
                "probe_finished_at": host_iso,
            },
        )

        self.assertNotEqual(snap["status"], "ok")
        self.assertEqual(snap["status"], "partial")
        node_snap = snap["nodes"]["edge-a"]
        self.assertEqual(node_snap["status"], "partial")
        clock = node_snap["clock"]
        self.assertTrue(clock["uncertainty_exceeded"])
        self.assertEqual(clock["runtime_equivalence"], "unverified")
        self.assertFalse(clock["runtime_elapsed_verified"])
        self.assertIn("chronyc", clock["rejection_reason"])
        self.assertIn("2", clock["rejection_reason"])

    def test_envelope_schema_and_timestamp_validation_failures_fail_closed(self):
        """
        Regression test (P1): When envelope contains unsupported version or inconsistent
        paired UTC timestamps, downstream snapshot fails closed with unverified clock.
        """
        host_unix = 1730900000.0
        host_iso = datetime.datetime.fromtimestamp(host_unix, datetime.timezone.utc).isoformat()

        # Inconsistent paired timestamps (skew > 1.0s)
        skewed_payload = {
            "version": 1,
            "host_observed_at": host_iso,
            "host_observed_unix": host_unix + 30.0,
            "host_chrony_elapsed_seconds": 0.003,
            "returncode": 0,
            "stdout": f"A9FEA97B,169.254.169.123,3,{host_unix - 4.0:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n",
        }
        wrapped_skewed = f"{CHRONY_PAYLOAD_BEGIN}\n{json.dumps(skewed_payload)}\n{CHRONY_PAYLOAD_END}\n"

        snap = collect_node_telemetry_safe(
            nodes=[{"alias": "cloud-a", "profile": "cloud"}],
            fetcher_fn=lambda n: {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": wrapped_skewed,
            },
        )
        self.assertEqual(snap["status"], "partial")
        clock = snap["nodes"]["cloud-a"]["clock"]
        self.assertTrue(clock["uncertainty_exceeded"])
        self.assertEqual(clock["rejection_reason"], "inconsistent_paired_host_utc_timestamps")
        self.assertEqual(clock["runtime_equivalence"], "unverified")

    def test_missing_timestamp_provenance_marked_unverified_without_unbound_local_error(self):
        """
        Regression test (P2): When node response has NO host timestamps, NO now_unix,
        and NO probe_finished_at, collect_node_telemetry_safe MUST NOT raise UnboundLocalError.
        Must fail closed with explicit time_provenance='unverified_missing_timestamp'
        and rejection_reason='missing_current_timestamp_for_age_verification'.
        """
        raw_csv = "A9FEA97B,169.254.169.123,3,1730899995.0,0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        # Fetcher returns no timestamps whatsoever
        def stripped_fetcher(node):
            return {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": raw_csv,
            }

        snap = collect_node_telemetry_safe(
            nodes=[{"alias": "edge-a", "profile": "edge"}],
            fetcher_fn=stripped_fetcher,
        )

        self.assertEqual(snap["status"], "partial")
        node_snap = snap["nodes"]["edge-a"]
        clock = node_snap["clock"]
        self.assertTrue(clock["uncertainty_exceeded"])
        self.assertEqual(clock["rejection_reason"], "missing_current_timestamp_for_age_verification")
        self.assertEqual(clock["time_provenance"], "unverified_missing_timestamp")
        self.assertIsNone(clock["sample_age_seconds"])

    def test_unparseable_timestamp_string_handled_gracefully(self):
        """
        Regression test (P2): When probe_finished_at or host timestamp contains an unparseable
        string (e.g. 'not-an-iso-string'), collector does not raise an unhandled exception,
        marks time_provenance='unverified_unparseable_timestamp', and fails closed.
        """
        raw_csv = "A9FEA97B,169.254.169.123,3,1730899995.0,0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n"

        snap = collect_node_telemetry_safe(
            nodes=[{"alias": "cloud-a", "profile": "cloud"}],
            fetcher_fn=lambda n: {
                "metrics_raw": "miles_ready 1\n",
                "chrony_raw": raw_csv,
                "probe_finished_at": "definitely-not-an-iso-timestamp-string",
            },
        )

        self.assertEqual(snap["status"], "partial")
        node_snap = snap["nodes"]["cloud-a"]
        clock = node_snap["clock"]
        self.assertTrue(clock["uncertainty_exceeded"])
        self.assertEqual(clock["rejection_reason"], "missing_current_timestamp_for_age_verification")
        self.assertEqual(clock["time_provenance"], "unverified_unparseable_timestamp")

    def test_collect_node_journals_safe(self):
        nodes = [
            {"alias": "cloud-a", "id": "i-aaa"},
            {"alias": "cloud-b", "id": "i-bbb"}
        ]

        def mock_journal_fetcher(node):
            if node["alias"] == "cloud-a":
                return {"lines": ["line 1", "line 2"]}
            raise ConnectionResetError("SSM Session stream disconnected")

        node_journals, node_status = collect_node_journals_safe(nodes, mock_journal_fetcher)
        self.assertEqual(len(node_journals), 2)
        self.assertEqual(node_status["cloud-a"], "ok")
        self.assertEqual(node_status["cloud-b"], "error")
        self.assertEqual(node_journals[0]["count"], 2)
        self.assertEqual(node_journals[1]["count"], 0)
        self.assertEqual(node_journals[1]["error_type"], "ConnectionResetError")

    def test_two_gateway_fixture_controlled_fault_correlation(self):
        """
        Phase A (A3) regression:
        In a 2-gateway fixture, exercises:
        - 1 HTTP 500 (matched to gateway A)
        - 1 HTTP 503 (matched to gateway B)
        - 1 post-header stream abort (matched to gateway A)
        - 1 upstream ALB failure (no gateway record)
        Verifies client request IDs map to exactly 1 diagnostic record or upstream_no_gateway_request.
        Verifies before/after telemetry snapshot preservation.
        Verifies zero secret leaks (tokens, payload bodies, signed URLs, keys).
        """
        secret_token = "TOP_SECRET_SESSION_TOKEN_XYZ_123"
        signed_url = "https://bucket.s3.amazonaws.com/obj?X-Amz-Signature=supersecret456"
        secret_key = "user/private/financial-statement.pdf"

        client_result = {
            "status": "failed",
            "samples": [
                # 1. HTTP 500 on cloud-a (writer admission rejection)
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-500-A",
                    "status_code": 500,
                    "error": "InternalError",
                    "payload_bytes": 8388608,
                    "secret_header": secret_token,
                    "signed_url": signed_url,
                    "target_key": secret_key,
                    "latency_ns": 25000000,
                },
                # 2. HTTP 503 on cloud-b (storage backend unavailable)
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-503-B",
                    "status_code": 503,
                    "error": "ServiceUnavailable",
                    "payload_bytes": 8389273,
                    "secret_header": secret_token,
                    "latency_ns": 8800000000,
                },
                # 3. Post-header stream abort on cloud-a
                {
                    "phase": "verify-get",
                    "operation": "GetObject",
                    "request_id": "REQ-STREAM-ABORT-A",
                    "status_code": 200,
                    "error": "unexpected EOF in data stream",
                    "payload_bytes": 4194304,
                    "secret_header": secret_token,
                    "latency_ns": 1200000000,
                },
                # 4. Upstream ALB 503 (never reached any gateway)
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-ALB-TIMEOUT-503",
                    "status_code": 503,
                    "error": "Service Unavailable",
                    "payload_bytes": 0,
                    "latency_ns": 30000000000,
                }
            ]
        }

        node_journals = [
            {
                "alias": "cloud-a",
                "id": "i-03bea44ea0f048125",
                "lines": [
                    "{\"msg\": \"s3 server failure\", \"request_id\": \"REQ-500-A\", \"node_id\": \"cloud-a\", \"method\": \"PUT\", \"operation\": \"PutObject\", \"stage\": \"writer_admission\", \"failure_class\": \"clock_uncertainty_exceeded\"}",
                    "{\"msg\": \"s3 server failure\", \"request_id\": \"REQ-STREAM-ABORT-A\", \"node_id\": \"cloud-a\", \"method\": \"GET\", \"operation\": \"GetObject\", \"stage\": \"object_get_stream\", \"failure_class\": \"stream_abort\"}",
                ]
            },
            {
                "alias": "cloud-b",
                "id": "i-075e63f7bff407047",
                "lines": [
                    "{\"msg\": \"s3 service unavailable\", \"request_id\": \"REQ-503-B\", \"node_id\": \"cloud-b\", \"method\": \"PUT\", \"operation\": \"PutObject\", \"stage\": \"prepare\", \"failure_class\": \"backend_unavailable\"}",
                ]
            }
        ]

        node_status = {"cloud-a": "ok", "cloud-b": "ok"}

        before_telemetry = {
            "captured_at": "2026-11-06T13:20:00Z",
            "status": "ok",
            "nodes": {
                "cloud-a": {"alias": "cloud-a", "status": "ok", "metrics": {"miles_ready": 1}},
                "cloud-b": {"alias": "cloud-b", "status": "ok", "metrics": {"miles_ready": 1}}
            }
        }
        after_telemetry = {
            "captured_at": "2026-11-06T13:23:00Z",
            "status": "ok",
            "nodes": {
                "cloud-a": {"alias": "cloud-a", "status": "ok", "metrics": {"miles_ready": 0}},
                "cloud-b": {"alias": "cloud-b", "status": "ok", "metrics": {"miles_ready": 1}}
            }
        }

        alb_evidence = [
            {"request_id": "REQ-ALB-TIMEOUT-503", "elb_status_code": 503, "target_status_code": "-"}
        ]

        payload = build_failed_window_payload(
            name="cloud-c1-t1-fixture-test",
            captured_at="2026-11-06T13:23:00Z",
            client_result=client_result,
            node_journals=node_journals,
            node_status=node_status,
            before_metrics=before_telemetry,
            after_metrics=after_telemetry,
            release_hash="da87fab12c8bc26bcee46f78eb24f0062a985880",
            utc_window={"before_at": "2026-11-06T13:20:00Z", "after_at": "2026-11-06T13:23:00Z"},
            alb_evidence=alb_evidence,
            schema_version=1
        )

        # 1. Telemetry before/after snapshots preserved
        self.assertEqual(payload["telemetry"]["before"], before_telemetry)
        self.assertEqual(payload["telemetry"]["after"], after_telemetry)

        # 2. Exactly 4 correlated failure records matching each client failure
        correlated = payload["correlated_failures"]
        self.assertEqual(len(correlated), 4)

        # Req 1: Gateway 500 on cloud-a
        self.assertEqual(correlated[0]["request_id"], "REQ-500-A")
        self.assertEqual(correlated[0]["source"], "gateway")
        self.assertEqual(correlated[0]["node_id"], "cloud-a")
        self.assertEqual(correlated[0]["stage"], "writer_admission")
        self.assertEqual(correlated[0]["failure_class"], "clock_uncertainty_exceeded")

        # Req 2: Gateway 503 on cloud-b
        self.assertEqual(correlated[1]["request_id"], "REQ-503-B")
        self.assertEqual(correlated[1]["source"], "gateway")
        self.assertEqual(correlated[1]["node_id"], "cloud-b")
        self.assertEqual(correlated[1]["stage"], "prepare")
        self.assertEqual(correlated[1]["failure_class"], "backend_unavailable")

        # Req 3: Post-header stream abort on cloud-a
        self.assertEqual(correlated[2]["request_id"], "REQ-STREAM-ABORT-A")
        self.assertEqual(correlated[2]["source"], "gateway")
        self.assertEqual(correlated[2]["node_id"], "cloud-a")
        self.assertEqual(correlated[2]["stage"], "object_get_stream")
        self.assertEqual(correlated[2]["failure_class"], "stream_abort")

        # Req 4: Upstream ALB 503 (corroborated by native ALB evidence)
        self.assertEqual(correlated[3]["request_id"], "REQ-ALB-TIMEOUT-503")
        self.assertEqual(correlated[3]["source"], "upstream_alb")
        self.assertEqual(correlated[3]["diagnostic_status"], "corroborated_alb_evidence")

        # 3. Security audit: zero leak of secret tokens, signed URLs, payload bodies, or target keys
        payload_json = json.dumps(payload)
        self.assertNotIn(secret_token, payload_json)
        self.assertNotIn(signed_url, payload_json)
        self.assertNotIn(secret_key, payload_json)
        self.assertNotIn("supersecret456", payload_json)

    def test_sanitize_benchmark_failure_adversarial_secrets_in_errors_and_journals(self):
        """
        Adversarial test injecting signed URLs, authorization tokens, AWS access keys,
        and concrete EC2 instance IDs into free-form errors and journal lines.
        Asserts the serialized entire payload contains zero sensitive leaks.
        """
        signed_url_leak = "https://s3.ap-southeast-1.amazonaws.com/test-bucket/secret-key.bin?X-Amz-Signature=SENSITIVE_SIGNATURE_HEX12345&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20261107%2Fap-southeast-1%2Fs3%2Faws4_request"
        bearer_leak = "Bearer secret_bearer_token_xyz987"
        aws_key_leak = "AKIAIOSFODNN7EXAMPLE"
        instance_id_leak = "i-03bea44ea0f048125"
        password_leak = "secret_password=TopSecretPassword456!"

        client_result = {
            "status": "failed",
            "error": f"workload failed with signed URL: {signed_url_leak} and {password_leak}",
            "samples": [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-LEAK-1",
                    "status_code": 500,
                    "error": f"Dial failed to {signed_url_leak} with auth {bearer_leak}",
                    "payload_bytes": 0,
                    "duration_ms": 15.0,
                }
            ]
        }

        node_journals = [
            {
                "alias": "cloud-a",
                "id": instance_id_leak,
                "lines": [
                    f'{{"msg": "s3 server failure", "request_id": "REQ-LEAK-1", "node_id": "cloud-a", "stage": "writer_admission", "failure_class": "clock_uncertainty_exceeded", "url": "{signed_url_leak}", "auth": "{bearer_leak}"}}',
                    f"raw unparsed line with {instance_id_leak} and {aws_key_leak} and {password_leak}",
                ]
            }
        ]

        node_status = {"cloud-a": "ok"}

        payload = build_failed_window_payload(
            name="adversarial-leak-test",
            captured_at="2026-11-07T12:00:00Z",
            client_result=client_result,
            node_journals=node_journals,
            node_status=node_status,
        )

        payload_json = json.dumps(payload)

        # Verify absolute zero leak across entire serialized payload
        self.assertNotIn("SENSITIVE_SIGNATURE_HEX12345", payload_json)
        self.assertNotIn("secret_bearer_token_xyz987", payload_json)
        self.assertNotIn(aws_key_leak, payload_json)
        self.assertNotIn(instance_id_leak, payload_json)
        self.assertNotIn("TopSecretPassword456!", payload_json)

        # Verify error was classified into allowlisted error code
        failures = payload["sanitized_summary"]["failures"]
        self.assertEqual(len(failures), 1)
        self.assertIn(failures[0]["error_code"], ["dial_error", "clock_uncertainty_exceeded", "internal_error", "unclassified_error"])
        self.assertIn("error_code", payload["sanitized_summary"])

    def test_object_path_and_opaque_auth_zero_leakage(self):
        """
        Verifies that object keys/paths in signed URLs and opaque Authorization tokens
        are completely eliminated from the sanitized public artifact.
        """
        raw_auth = "Authorization = aRealTokenSecretWithNoBearer"
        redacted_auth = redact_sensitive_strings(raw_auth)
        self.assertNotIn("aRealTokenSecretWithNoBearer", redacted_auth)
        self.assertIn("Authorization: [REDACTED]", redacted_auth)

        leak_url = "https://example.invalid/private/financial-statement.pdf?X-Amz-Signature=NOTREAL"
        client_res = {
            "status": "failed",
            "error": f"workload failed fetching {leak_url}",
            "samples": [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-LEAK-OBJ",
                    "status_code": 500,
                    "error": f"error getting {leak_url} with {raw_auth}",
                }
            ]
        }

        sanitized = sanitize_benchmark_failure(client_res)
        dumped = json.dumps(sanitized)
        self.assertNotIn("/private/financial-statement.pdf", dumped)
        self.assertNotIn("financial-statement", dumped)
        self.assertNotIn("aRealTokenSecretWithNoBearer", dumped)
        self.assertNotIn("NOTREAL", dumped)

    def test_correlate_failures_alb_target_503_not_upstream(self):
        """
        Verifies that when ALB access logs record target_status_code=503 (target responded),
        the error is classified as origin_unverified (missing gateway log), NOT upstream_alb.
        """
        correlated = correlate_failures(
            client_failures=[{"request_id": "R", "http_status": 503}],
            gateway_records=[],
            node_status={"cloud-a": "ok"},
            alb_evidence=[{"request_id": "R", "elb_status_code": 503, "target_status_code": 503, "upstream": False}]
        )
        self.assertEqual(len(correlated), 1)
        self.assertEqual(correlated[0]["request_id"], "R")
        self.assertEqual(correlated[0]["source"], "origin_unverified")
        self.assertEqual(correlated[0]["diagnostic_status"], "gateway_log_missing")


if __name__ == "__main__":
    unittest.main()
