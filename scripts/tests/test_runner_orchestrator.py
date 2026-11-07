"""
Unit and integration tests for scripts/runner_orchestrator.py.

Tests the real runner orchestration call site with mocked SSM/SSH runners:
- Fallback for Session Manager 'Standard_Stream not found' to RunCommand
- Partial node failure handling and status reporting
- Persistence of public and restricted private failure artifacts before exit
- Rejection of false upstream attribution for unmatched 500 errors
- Zero secret leakage across entire serialized payloads
"""
import json
import os
import tempfile
import unittest
from pathlib import Path

from scripts.runner_orchestrator import (
    capture_and_assemble_failed_window,
    collect_cluster_journals,
    collect_cluster_telemetry,
)


class RunnerOrchestratorTests(unittest.TestCase):

    def test_ssm_standard_stream_fallback(self):
        """
        Verifies that when SSM Session Manager fails with 'Standard_Stream not found',
        the orchestrator cleanly falls back to RunCommand without aborting collection.
        """
        session_called = []
        runcommand_called = []

        def mock_session_runner(node_id, prog):
            session_called.append((node_id, prog))
            raise RuntimeError("Standard_Stream not found in session channel")

        def mock_runcommand_runner(node_id, prog):
            runcommand_called.append((node_id, prog))
            if "startup.json" in prog:
                return "miles_ready 1\nprocess_resident_memory_bytes 2048\n"
            if "chronyc" in prog:
                return "A9FEA97B,169.254.169.123,3,100.0,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n"
            if "journalctl" in prog:
                return json.dumps({
                    "count": 1,
                    "lines": [
                        '{"msg": "s3 server failure", "request_id": "REQ-SSM-1", "node_id": "cloud-a", "stage": "writer_admission", "failure_class": "clock_uncertainty_exceeded"}'
                    ]
                })
            return "{}"

        nodes = [{"alias": "cloud-a", "id": "i-03bea44ea0f048125", "profile": "cloud"}]

        # 1. Test telemetry collection falls back
        snap = collect_cluster_telemetry(
            nodes=nodes,
            ssm_runner=mock_session_runner,
            ssm_runcommand_runner=mock_runcommand_runner,
            captured_at="2026-11-07T12:00:00Z",
            now_unix=105.0,
        )
        self.assertEqual(snap["status"], "ok")
        self.assertIn("cloud-a", snap["nodes"])
        self.assertEqual(snap["nodes"]["cloud-a"]["status"], "ok")
        self.assertEqual(snap["nodes"]["cloud-a"]["metrics"]["miles_ready"], 1)
        self.assertTrue(len(session_called) > 0)
        self.assertTrue(len(runcommand_called) > 0)

        # 2. Test journal collection falls back
        node_journals, node_status = collect_cluster_journals(
            nodes=nodes,
            since_timestamp=1730900000,
            ssm_runner=mock_session_runner,
            ssm_runcommand_runner=mock_runcommand_runner,
        )
        self.assertEqual(node_status["cloud-a"], "ok")
        self.assertEqual(node_journals[0]["count"], 1)

    def test_partial_node_failure_handling(self):
        """
        Verifies that failure on one node (e.g. timeout on cloud-b) does not lose
        telemetry or journals collected from healthy nodes.
        """
        def mock_ssm(node_id, prog):
            if "cloud-b" in node_id or node_id == "i-fail-b":
                raise TimeoutError("SSM command timed out after 75s")
            if "startup.json" in prog:
                return "miles_ready 1\n"
            if "chronyc" in prog:
                return "A9FEA97B,169.254.169.123,3,100.0,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n"
            return json.dumps({"count": 0, "lines": []})

        nodes = [
            {"alias": "cloud-a", "id": "i-ok-a", "profile": "cloud"},
            {"alias": "cloud-b", "id": "i-fail-b", "profile": "cloud"},
        ]

        telemetry = collect_cluster_telemetry(
            nodes=nodes,
            ssm_runner=mock_ssm,
            captured_at="2026-11-07T12:00:00Z",
            now_unix=105.0,
        )
        self.assertEqual(telemetry["status"], "partial")
        self.assertEqual(telemetry["nodes"]["cloud-a"]["status"], "ok")
        self.assertEqual(telemetry["nodes"]["cloud-b"]["status"], "error")
        self.assertIn("timed out", telemetry["nodes"]["cloud-b"]["error"])

    def test_capture_and_assemble_failed_window_persists_evidence(self):
        """
        Verifies that capture_and_assemble_failed_window creates the public sanitized
        artifact and restricted private raw trace before returning.
        """
        with tempfile.TemporaryDirectory() as tmp_dir:
            out_path = Path(tmp_dir)

            client_result = {
                "status": "failed",
                "error": "benchmark execution failed with HTTP 500",
                "samples": [
                    {
                        "phase": "put",
                        "operation": "PutObject",
                        "request_id": "REQ-FAIL-100",
                        "status_code": 500,
                        "error": "InternalError",
                        "payload_bytes": 1048576,
                        "duration_ms": 32.5,
                    }
                ]
            }

            def mock_ssm(node_id, prog):
                if "startup.json" in prog:
                    return "miles_ready 0\n"
                if "chronyc" in prog:
                    return "A9FEA97B,169.254.169.123,3,100.0,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n"
                if "journalctl" in prog:
                    return json.dumps({
                        "count": 1,
                        "lines": [
                            '{"msg": "s3 server failure", "request_id": "REQ-FAIL-100", "node_id": "cloud-a", "stage": "writer_admission", "failure_class": "clock_uncertainty_exceeded"}'
                        ]
                    })
                return "{}"

            nodes = [{"alias": "cloud-a", "id": "i-03bea44ea0f048125", "profile": "cloud"}]

            public_payload, artifact_path = capture_and_assemble_failed_window(
                name="cloud-c1-t1-test",
                nodes=nodes,
                client_result=client_result,
                since_timestamp=1730900000,
                ssm_runner=mock_ssm,
                out_dir=out_path,
                release_hash="da87fab12c8bc26bcee46f78eb24f0062a985880",
                save_private_trace=True,
                private_dir=out_path,
            )

            # 1. Public artifact created and matches schema
            self.assertTrue(artifact_path.exists())
            self.assertEqual(artifact_path.name, "cloud-c1-t1-test-failed-window.json")
            data = json.loads(artifact_path.read_text())
            self.assertEqual(data["name"], "cloud-c1-t1-test")
            self.assertEqual(data["release_hash"], "da87fab12c8bc26bcee46f78eb24f0062a985880")
            self.assertEqual(len(data["correlated_failures"]), 1)
            self.assertEqual(data["correlated_failures"][0]["source"], "gateway")
            self.assertEqual(data["correlated_failures"][0]["stage"], "writer_admission")

            # 2. Private raw trace created with 0o600 permissions
            private_file = out_path / "cloud-c1-t1-test-private-raw-trace.json"
            self.assertTrue(private_file.exists())
            mode = private_file.stat().st_mode & 0o777
            self.assertEqual(mode, 0o600)

    def test_unmatched_500_marked_origin_unverified_and_evidenced_503_upstream(self):
        """
        Verifies that an unmatched 500 is never attributed to upstream ALB,
        while a 503 with native ALB evidence is correctly attributed to upstream_alb.
        """
        client_result = {
            "status": "failed",
            "samples": [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-500-NOLOG",
                    "status_code": 500,
                    "error": "InternalError",
                },
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-503-ALB",
                    "status_code": 503,
                    "error": "Service Unavailable",
                }
            ]
        }

        # No gateway records collected
        def mock_ssm(node_id, prog):
            if "journalctl" in prog:
                return json.dumps({"count": 0, "lines": []})
            return ""

        alb_evidence = [
            {"request_id": "REQ-503-ALB", "elb_status_code": 503, "target_status_code": "-"}
        ]

        nodes = [{"alias": "cloud-a", "id": "i-03bea44ea0f048125", "profile": "cloud"}]

        payload, _ = capture_and_assemble_failed_window(
            name="attribution-test",
            nodes=nodes,
            client_result=client_result,
            ssm_runner=mock_ssm,
            alb_evidence=alb_evidence,
        )

        correlated = payload["correlated_failures"]
        self.assertEqual(len(correlated), 2)

        # REQ-500-NOLOG: MUST be origin_unverified with gateway_log_missing
        self.assertEqual(correlated[0]["request_id"], "REQ-500-NOLOG")
        self.assertEqual(correlated[0]["source"], "origin_unverified")
        self.assertEqual(correlated[0]["diagnostic_status"], "gateway_log_missing")

        # REQ-503-ALB: Corroborated upstream ALB error
        self.assertEqual(correlated[1]["request_id"], "REQ-503-ALB")
        self.assertEqual(correlated[1]["source"], "upstream_alb")
        self.assertEqual(correlated[1]["diagnostic_status"], "corroborated_alb_evidence")

    def test_zero_leakage_of_secrets_and_identifiers(self):
        """
        Adversarial test verifying zero leakage of signed URLs, tokens, keys,
        or raw EC2 instance IDs across the serialized orchestrator output.
        """
        secret_sig = "SENSITIVE_SIGNATURE_ABCD_9999"
        signed_url = f"https://example.test/obj.bin?X-Amz-Signature={secret_sig}&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE"
        bearer_tok = "Bearer my_secret_token_val_456"
        inst_id = "i-03bea44ea0f048125"

        client_result = {
            "status": "failed",
            "error": f"Workload crashed accessing {signed_url}",
            "samples": [
                {
                    "phase": "put",
                    "operation": "PutObject",
                    "request_id": "REQ-ADVERSARIAL",
                    "status_code": 500,
                    "error": f"Failure: {signed_url} with {bearer_tok}",
                }
            ]
        }

        def mock_ssm(node_id, prog):
            if "journalctl" in prog:
                return json.dumps({
                    "count": 1,
                    "lines": [
                        f"raw system log on {inst_id} with Authorization: {bearer_tok} and secret_key=TopSecret"
                    ]
                })
            return ""

        nodes = [{"alias": "cloud-a", "id": inst_id, "profile": "cloud"}]

        payload, _ = capture_and_assemble_failed_window(
            name="zero-leak-test",
            nodes=nodes,
            client_result=client_result,
            ssm_runner=mock_ssm,
        )

        dumped = json.dumps(payload)
        self.assertNotIn(secret_sig, dumped)
        self.assertNotIn("my_secret_token_val_456", dumped)
        self.assertNotIn("AKIAIOSFODNN7EXAMPLE", dumped)
        self.assertNotIn(inst_id, dumped)
        self.assertNotIn("TopSecret", dumped)


if __name__ == "__main__":
    unittest.main()
