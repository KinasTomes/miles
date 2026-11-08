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

from unittest.mock import MagicMock, patch

from scripts.runner_orchestrator import (
    capture_and_assemble_failed_window,
    collect_cluster_journals,
    collect_cluster_telemetry,
    create_aws_cli_runcommand_runner,
    execute_workload_and_capture_on_failure,
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

    def test_ssm_fail_fast_when_no_runcommand_runner(self):
        """
        Verifies that if Session Manager fails and no ssm_runcommand_runner is passed,
        the orchestrator fails fast with a clear error and does not re-invoke ssm_runner.
        """
        call_count = 0

        def failing_ssm(node_id, prog):
            nonlocal call_count
            call_count += 1
            raise RuntimeError("Standard_Stream not found in session channel")

        nodes = [{"alias": "cloud-a", "id": "i-03bea44ea0f048125", "profile": "cloud"}]

        telemetry = collect_cluster_telemetry(
            nodes=nodes,
            ssm_runner=failing_ssm,
            ssm_runcommand_runner=None,
        )
        self.assertEqual(telemetry["status"], "error")
        self.assertEqual(telemetry["nodes"]["cloud-a"]["status"], "error")
        self.assertIn("no genuine ssm_runcommand_runner fallback", telemetry["nodes"]["cloud-a"]["error"])
        # Should only have called failing_ssm once, not looped back
        self.assertEqual(call_count, 1)

    def test_execute_workload_and_capture_on_failure_with_exception(self):
        """
        Verifies that when workload_fn raises an exception (e.g. TimeoutError or ConnectionResetError),
        the orchestrator writes the failed-window artifact to out_dir before re-raising the exception.
        """
        with tempfile.TemporaryDirectory() as tmp_dir:
            out_path = Path(tmp_dir)

            def failing_workload():
                raise ConnectionResetError("SSH connection dropped by remote peer")

            def mock_ssm(node_id, prog):
                if "startup.json" in prog:
                    return "miles_ready 1\n"
                if "chronyc" in prog:
                    return "A9FEA97B,169.254.169.123,3,100.0,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n"
                if "journalctl" in prog:
                    return json.dumps({"count": 0, "lines": []})
                return "{}"

            nodes = [{"alias": "cloud-a", "id": "i-03bea44ea0f048125", "profile": "cloud"}]

            with self.assertRaises(ConnectionResetError):
                execute_workload_and_capture_on_failure(
                    workload_fn=failing_workload,
                    name="ssh-fail-test",
                    nodes=nodes,
                    ssm_runner=mock_ssm,
                    out_dir=out_path,
                )

            artifact = out_path / "ssh-fail-test-failed-window.json"
            self.assertTrue(artifact.exists(), "Failed window artifact must be written on workload exception")
            data = json.loads(artifact.read_text())
            self.assertEqual(data["name"], "ssh-fail-test")

    def test_execute_workload_and_capture_on_failure_nonzero_rc(self):
        """
        Verifies that when workload_fn returns a completed process object with non-zero returncode,
        it captures failure diagnostics and raises a RuntimeError.
        """
        with tempfile.TemporaryDirectory() as tmp_dir:
            out_path = Path(tmp_dir)

            class MockProcessResult:
                returncode = 2
                stdout = "workload execution error"

            def mock_ssm(node_id, prog):
                if "startup.json" in prog:
                    return "miles_ready 1\n"
                if "chronyc" in prog:
                    return "A9FEA97B,169.254.169.123,3,100.0,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n"
                return json.dumps({"count": 0, "lines": []})

            nodes = [{"alias": "cloud-a", "id": "i-03bea44ea0f048125", "profile": "cloud"}]

            with self.assertRaises(RuntimeError) as ctx:
                execute_workload_and_capture_on_failure(
                    workload_fn=lambda: MockProcessResult(),
                    name="rc-nonzero-test",
                    nodes=nodes,
                    ssm_runner=mock_ssm,
                    out_dir=out_path,
                )
            self.assertIn("non-zero return code: 2", str(ctx.exception))
            artifact = out_path / "rc-nonzero-test-failed-window.json"
            self.assertTrue(artifact.exists())

    def test_create_aws_cli_runcommand_runner(self):
        """
        Verifies that create_aws_cli_runcommand_runner sends AWS-RunShellScript command
        and polls get-command-invocation until success, extracting the delimited output.
        """
        import base64
        import re

        captured_script = []

        def fake_subprocess_run(cmd, **kwargs):
            if "send-command" in cmd:
                params_idx = cmd.index("--parameters")
                params_json = json.loads(cmd[params_idx + 1])
                sh_cmd = params_json["commands"][0]
                captured_script.append(sh_cmd)
                res = MagicMock()
                res.returncode = 0
                res.stdout = json.dumps({"Command": {"CommandId": "cmd-test-12345"}})
                return res
            elif "get-command-invocation" in cmd:
                sh_cmd = captured_script[-1]
                m_b64 = re.search(r"base64\.b64decode\('([^']+)'\)", sh_cmd)
                decoded = base64.b64decode(m_b64.group(1)).decode() if m_b64 else ""
                m_begin = re.search(r"__MILES_RC_BEGIN_[0-9a-f]+__", decoded)
                m_end = re.search(r"__MILES_RC_END_[0-9a-f]+__", decoded)
                begin = m_begin.group(0) if m_begin else ""
                end = m_end.group(0) if m_end else ""
                res = MagicMock()
                res.returncode = 0
                res.stdout = json.dumps({
                    "Status": "Success",
                    "StandardOutputContent": f"{begin}\nmiles_ready 1\n{end} 0\n",
                    "StandardErrorContent": "",
                })
                return res
            raise ValueError(f"Unexpected command: {cmd}")

        with patch("subprocess.run", side_effect=fake_subprocess_run) as mock_run, patch("time.sleep"):
            runner = create_aws_cli_runcommand_runner(region="ap-southeast-1", timeout=10)
            output = runner("i-testnode", "print('hello')")
            self.assertEqual(output, "miles_ready 1")

            self.assertEqual(mock_run.call_count, 2)
            first_call_args = mock_run.call_args_list[0][0][0]
            self.assertIn("send-command", first_call_args)
            self.assertIn("AWS-RunShellScript", first_call_args)
            self.assertIn("i-testnode", first_call_args)

            second_call_args = mock_run.call_args_list[1][0][0]
            self.assertIn("get-command-invocation", second_call_args)
            self.assertIn("cmd-test-12345", second_call_args)

    def test_create_aws_cli_runcommand_runner_truncated_sentinels_raises_error(self):
        """
        Verifies that if get-command-invocation returns Status=Success but output is
        truncated or missing begin/end sentinels, create_aws_cli_runcommand_runner raises
        an explicit RuntimeError instead of silently returning truncated content.
        """
        mock_send_res = MagicMock()
        mock_send_res.returncode = 0
        mock_send_res.stdout = json.dumps({"Command": {"CommandId": "cmd-trunc-12345"}})

        mock_inv_res = MagicMock()
        mock_inv_res.returncode = 0
        mock_inv_res.stdout = json.dumps({
            "Status": "Success",
            "StandardOutputContent": "partial but truncated output; no sentinel",
            "StandardErrorContent": "",
        })

        with patch("subprocess.run") as mock_run, patch("time.sleep"):
            mock_run.side_effect = [mock_send_res, mock_inv_res]
            runner = create_aws_cli_runcommand_runner(region="ap-southeast-1", timeout=10)
            with self.assertRaises(RuntimeError) as ctx:
                runner("i-testnode", "print('hello')")
            self.assertIn("missing begin/end sentinels", str(ctx.exception))

    def test_execute_workload_and_capture_on_failure_persistence_failure_surfaced(self):
        """
        Verifies that if evidence capture cannot write to out_dir (e.g. out_dir is an existing file),
        the persistence failure is surfaced via a compound RuntimeError while preserving the cause.
        """
        with tempfile.TemporaryDirectory() as tmp_dir:
            file_blocking_dir = Path(tmp_dir) / "occupied-file"
            file_blocking_dir.write_text("i am a file not a directory")

            def failing_workload():
                raise TimeoutError("SSH workload timed out after 1000s")

            nodes = [{"alias": "cloud-a", "id": "i-03bea44ea0f048125", "profile": "cloud"}]

            with self.assertRaises(RuntimeError) as ctx:
                execute_workload_and_capture_on_failure(
                    workload_fn=failing_workload,
                    name="persist-fail-test",
                    nodes=nodes,
                    out_dir=file_blocking_dir,
                )

            err_msg = str(ctx.exception)
            self.assertIn("Workload failed with TimeoutError", err_msg)
            self.assertIn("failure evidence persistence failed", err_msg)
            self.assertIsInstance(ctx.exception.__cause__, TimeoutError)

    def test_chrony_probe_failure_marks_partial_without_fake_readiness(self):
        """
        Verifies that when metrics are collected but Chrony probe fails with an exception,
        the node and cluster status are marked 'partial' (NOT 'ok'), clock status is 'error',
        and readyz is None (NOT synthesized 200).
        """
        def mock_ssm(node_id, prog):
            if "startup.json" in prog:
                return "miles_ready 1\n"
            if "chronyc" in prog:
                raise RuntimeError("chronyc timed out waiting for tracking")
            return "{}"

        nodes = [{"alias": "cloud-a", "id": "i-03bea44ea0f048125", "profile": "cloud"}]

        snap = collect_cluster_telemetry(
            nodes=nodes,
            ssm_runner=mock_ssm,
            ssm_runcommand_runner=mock_ssm,
            captured_at="2026-11-07T12:00:00Z",
            now_unix=105.0,
        )

        # Cluster and node must be partial, NOT ok!
        self.assertEqual(snap["status"], "partial")
        node_snap = snap["nodes"]["cloud-a"]
        self.assertEqual(node_snap["status"], "partial")
        self.assertEqual(node_snap["clock"]["status"], "error")
        self.assertIn("chronyc timed out", node_snap["clock"]["error"])
        # readyz must NOT be fake 200
        self.assertIsNone(node_snap["readyz"])
        self.assertEqual(snap["summary"]["healthy_nodes"], 0)
        self.assertEqual(snap["summary"]["partial_nodes"], 1)

    def test_collect_cluster_telemetry_captures_per_node_probe_timings_and_chrony_elapsed(self):
        """
        Verifies that collect_cluster_telemetry records probe_started_at, probe_finished_at,
        probe_duration_seconds, and chrony_elapsed_seconds across both Cloud (SSM) and Edge (SSH) nodes,
        and ensures clock sample age is evaluated accurately against the probe finish time.
        """
        import time

        def mock_ssm(node_id, prog):
            now_t = time.time()
            if "startup.json" in prog:
                return "miles_ready 1\n"
            if "chronyc" in prog:
                ref_t = now_t - 4.0
                return f"A9FEA97B,169.254.169.123,3,{ref_t:.4f},0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n"
            return "{}"

        class MockSSHResult:
            def __init__(self, stdout):
                self.stdout = stdout.encode() if isinstance(stdout, str) else stdout

        def mock_ssh(cmd):
            now_t = time.time()
            if "metrics" in " ".join(cmd):
                return MockSSHResult("miles_ready 1\n")
            if "chronyc" in " ".join(cmd):
                ref_t = now_t - 6.0
                return MockSSHResult(f"A9FEA97B,169.254.169.123,3,{ref_t:.4f},0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n")
            return MockSSHResult("")

        nodes = [
            {"alias": "cloud-a", "id": "i-cloud-a", "profile": "cloud"},
            {"alias": "edge-a", "id": "edge-node", "profile": "edge"},
        ]

        snap = collect_cluster_telemetry(
            nodes=nodes,
            ssm_runner=mock_ssm,
            ssm_runcommand_runner=mock_ssm,
            ssh_runner=mock_ssh,
        )

        self.assertEqual(snap["status"], "ok")
        self.assertIn("collection_started_at", snap)
        self.assertIn("collection_finished_at", snap)
        self.assertEqual(len(snap["nodes"]), 2)

        for alias in ["cloud-a", "edge-a"]:
            node_snap = snap["nodes"][alias]
            self.assertEqual(node_snap["status"], "ok")
            self.assertIsNotNone(node_snap["probe_started_at"])
            self.assertIsNotNone(node_snap["probe_finished_at"])
            self.assertIsNotNone(node_snap["probe_duration_seconds"])
            self.assertGreaterEqual(node_snap["probe_duration_seconds"], 0.0)

            clock = node_snap["clock"]
            self.assertEqual(clock["status"], "ok")
            self.assertFalse(clock["uncertainty_exceeded"])
            # Sample age must be positive (freshly sampled)
            self.assertGreater(clock["sample_age_seconds"], 0.0)
            self.assertLess(clock["sample_age_seconds"], 16.0)


if __name__ == "__main__":
    unittest.main()
