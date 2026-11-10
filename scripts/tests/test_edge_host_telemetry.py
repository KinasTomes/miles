"""
Unit and regression tests for Edge Host Diagnostic Sampler & Ring Buffer (scripts/edge_host_telemetry.py).
Includes regression tests for:
- [P1] Preflight validation for chronyc binary and tracking parser; fail-closed on missing dependencies.
- [P1] Empty window failure propagation: summarize_window returns UNVERIFIED with clock_evidence_verified=False.
- [P2] Ring buffer hard-cap bounding invariant: oversized records (> max_bytes) are dropped; file size strictly bounded.
- [P2] Local monotonic clock timing and read-only chrony command allowlist enforcement.
"""

import datetime
import json
import os
from pathlib import Path
import tempfile
import time
import unittest

from scripts.edge_host_telemetry import (
    ALLOWLISTED_CHRONY_COMMANDS,
    BoundedRingBuffer,
    ChronyProbe,
    HostTelemetryRecorder,
    OSPressureProbe,
    preflight_check,
)


class EdgeHostTelemetryTests(unittest.TestCase):

    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.tmppath = Path(self.tmpdir.name)

    def tearDown(self):
        self.tmpdir.cleanup()

    def test_chrony_probe_allowlist_enforcement(self):
        """Verifies that unauthorized chrony commands raise ValueError (read-only guard)."""
        probe = ChronyProbe()

        # Disallowed mutative or sensitive commands
        for cmd in ["makestep", "settime", "burst", "activity", "reselect"]:
            with self.assertRaises(ValueError):
                probe.execute_chrony_subcommand(cmd)

        # Allowlisted read-only commands
        for cmd in ALLOWLISTED_CHRONY_COMMANDS:
            def dummy_runner(c, t):
                return 0, "mock_output", ""

            p = ChronyProbe(runner=dummy_runner)
            res = p.execute_chrony_subcommand(cmd)
            self.assertEqual(res["returncode"], 0)
            self.assertEqual(res["subcommand"], cmd)

    def test_chrony_probe_monotonic_duration_and_timeout(self):
        """Verifies high-resolution duration measurement and timeout detection."""
        def fast_runner(cmd, timeout):
            time.sleep(0.01)
            t_ref = time.time() - 2.0
            return 0, f"A9FEA97B,169.254.169.123,3,{t_ref:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n", ""

        probe = ChronyProbe(command_timeout=1.0, runner=fast_runner)
        res = probe.execute_chrony_subcommand("tracking")
        self.assertEqual(res["returncode"], 0)
        self.assertFalse(res["timed_out"])
        self.assertGreaterEqual(res["duration_seconds"], 0.005)
        self.assertLess(res["duration_seconds"], 0.5)
        self.assertIn("host_observed_utc", res)
        self.assertIn("host_observed_unix", res)

        def timeout_runner(cmd, timeout):
            return -1, "", "Command timed out"

        probe_timeout = ChronyProbe(command_timeout=1.0, runner=timeout_runner)
        res_timeout = probe_timeout.execute_chrony_subcommand("tracking")
        self.assertEqual(res_timeout["returncode"], -1)
        self.assertTrue(res_timeout["timed_out"])

    def test_preflight_check(self):
        """[P1 Regression] Preflight check verifies tracking parser and chronyc availability."""
        # 1. With mock probe runner, binary is not required -> PASS
        mock_probe = ChronyProbe(runner=lambda c, t: (0, "", ""))
        ok, errors = preflight_check(chrony_probe=mock_probe, require_chrony=True)
        self.assertTrue(ok)
        self.assertEqual(len(errors), 0)

        # 2. When chronyc is required without mock runner on systems without chronyc
        import shutil
        original_which = shutil.which
        try:
            shutil.which = lambda cmd: None
            default_probe = ChronyProbe()
            ok_fail, errors_fail = preflight_check(chrony_probe=default_probe, require_chrony=True)
            self.assertFalse(ok_fail)
            self.assertTrue(any("chronyc binary not found" in e for e in errors_fail))
        finally:
            shutil.which = original_which

    def test_os_pressure_probe_cpu_stat_deltas_and_steal(self):
        """Verifies /proc/stat tick delta calculation for hypervisor steal and iowait."""
        proc_dir = self.tmppath / "proc"
        proc_dir.mkdir(parents=True, exist_ok=True)
        stat_file = proc_dir / "stat"

        stat_file.write_text("cpu  1000 0 500 8000 200 0 50 100 0 0\n")
        probe = OSPressureProbe(proc_root=proc_dir)
        s1 = probe.read_stat()
        self.assertIsNone(s1["deltas"])

        stat_file.write_text("cpu  1050 0 520 8080 200 0 50 150 0 0\n")
        s2 = probe.read_stat()
        self.assertIsNotNone(s2["deltas"])
        self.assertEqual(s2["deltas"]["steal"], 50)
        self.assertEqual(s2["deltas"]["user"], 50)
        self.assertEqual(s2["total_delta_ticks"], 200)
        self.assertAlmostEqual(s2["steal_pct"], 25.0, places=1)
        self.assertAlmostEqual(s2["user_pct"], 25.0, places=1)

    def test_os_pressure_probe_psi_and_loadavg(self):
        """Verifies PSI and loadavg parsing with graceful fallback for missing files."""
        proc_dir = self.tmppath / "proc"
        proc_dir.mkdir(parents=True, exist_ok=True)

        probe = OSPressureProbe(proc_root=proc_dir)
        psi_cpu = probe.read_psi("cpu")
        self.assertFalse(psi_cpu["enabled"])

        psi_dir = proc_dir / "pressure"
        psi_dir.mkdir(parents=True, exist_ok=True)
        (psi_dir / "cpu").write_text("some avg10=1.25 avg60=0.85 avg300=0.40 total=1254300\n")
        (psi_dir / "io").write_text("some avg10=2.50 avg60=1.10 avg300=0.50 total=2500000\nfull avg10=0.50 avg60=0.20 avg300=0.10 total=500000\n")

        psi_cpu_ok = probe.read_psi("cpu")
        self.assertTrue(psi_cpu_ok["enabled"])
        self.assertEqual(psi_cpu_ok["some"]["avg10"], 1.25)
        self.assertEqual(psi_cpu_ok["some"]["total"], 1254300)

        psi_io_ok = probe.read_psi("io")
        self.assertTrue(psi_io_ok["enabled"])
        self.assertEqual(psi_io_ok["some"]["avg10"], 2.50)
        self.assertEqual(psi_io_ok["full"]["avg10"], 0.50)

        (proc_dir / "loadavg").write_text("0.45 0.30 0.15 2/180 12345\n")
        load = probe.read_loadavg()
        self.assertEqual(load["load_1m"], 0.45)
        self.assertEqual(load["runnable_entities"], 2)
        self.assertEqual(load["total_entities"], 180)

    def test_bounded_ring_buffer_permissions_and_size_capping(self):
        """Verifies file permission 0600 and bounded size trimming when exceeding max_bytes."""
        buf_file = self.tmppath / "telemetry-ring.jsonl"
        max_bytes = 1024 * 1024  # 1 MB
        ring = BoundedRingBuffer(buf_file, max_bytes=max_bytes)

        # File permissions check (0600)
        stat_mode = buf_file.stat().st_mode & 0o777
        self.assertEqual(stat_mode, 0o600)

        # Write enough lines to exceed max_bytes
        large_payload = "X" * 1000
        for i in range(1500):
            ring.write_record({"seq": i, "payload": large_payload, "timestamp_utc": datetime.datetime.now(datetime.timezone.utc).isoformat()})

        # File size must strictly be <= max_bytes
        final_size = buf_file.stat().st_size
        self.assertLessEqual(final_size, max_bytes)
        self.assertGreater(ring.dropped_records_count, 0)
        self.assertGreater(ring.trimmed_bytes_total, 0)

        records = ring.read_records()
        self.assertGreater(len(records), 0)
        seqs = [r["seq"] for r in records]
        self.assertEqual(seqs, sorted(seqs))

    def test_bounded_ring_buffer_hard_cap_oversized_record(self):
        """[P2 Regression] Records larger than max_bytes are dropped immediately, preserving bounds."""
        buf_file = self.tmppath / "telemetry-oversized.jsonl"
        max_bytes = 1024 * 1024  # 1 MB
        ring = BoundedRingBuffer(buf_file, max_bytes=max_bytes)

        # Attempt to write single record exceeding max_bytes (1.5 MB)
        oversized_payload = "Z" * (1500 * 1024)
        ring.write_record({"payload": oversized_payload})

        # Must be dropped without writing to file
        self.assertEqual(buf_file.stat().st_size, 0)
        self.assertEqual(ring.dropped_records_count, 1)
        self.assertGreaterEqual(ring.trimmed_bytes_total, 1500 * 1024)

    def test_host_telemetry_recorder_lifecycle_and_summary(self):
        """Verifies coordinated recording loop, sampling cadences, and window summary."""
        output_file = self.tmppath / "daemon-test.jsonl"

        def mock_chrony_runner(cmd, timeout):
            t_ref = time.time() - 2.0
            return 0, f"A9FEA97B,169.254.169.123,3,{t_ref:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n", ""

        proc_dir = self.tmppath / "proc"
        proc_dir.mkdir(parents=True, exist_ok=True)
        (proc_dir / "stat").write_text("cpu  1000 0 500 8000 200 0 50 100 0 0\n")
        (proc_dir / "loadavg").write_text("0.10 0.10 0.10 1/100 100\n")

        recorder = HostTelemetryRecorder(
            output_file=output_file,
            os_interval=0.2,
            chrony_interval=0.4,
            chrony_timeout=1.0,
            chrony_runner=mock_chrony_runner,
            proc_root=proc_dir,
        )

        recorder.start_background()
        time.sleep(1.2)
        recorder.stop()

        self.assertGreater(recorder.samples_collected, 0)
        records = recorder.ring_buffer.read_records()
        self.assertGreaterEqual(len(records), 3)

        summary = recorder.summarize_window()
        self.assertEqual(summary["record_count"], len(records))
        self.assertEqual(summary["status"], "OK")
        self.assertTrue(summary["clock_evidence_verified"])
        self.assertGreaterEqual(summary["chrony_sample_count"], 1)
        self.assertIsNotNone(summary["chrony_timing"]["avg_duration_s"])

    def test_empty_window_summary_fail_closed(self):
        """[P1 Regression] Empty window returns UNVERIFIED with clock_evidence_verified=False."""
        output_file = self.tmppath / "empty.jsonl"
        recorder = HostTelemetryRecorder(output_file=output_file)

        summary = recorder.summarize_window()
        self.assertEqual(summary["record_count"], 0)
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertFalse(summary["clock_evidence_verified"])
        self.assertEqual(summary["reason"], "no_records_in_window")


if __name__ == "__main__":
    unittest.main()
