"""
Unit and integration tests for Edge Host Diagnostic Sampler & Ring Buffer (scripts/edge_host_telemetry.py).
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
        # Fast runner
        def fast_runner(cmd, timeout):
            time.sleep(0.01)
            return 0, "A9FEA97B,169.254.169.123,4,1794112023.0,0.000001,0.000001,0.000001,0.1,0.001,0.001,16.2,Normal", ""

        probe = ChronyProbe(command_timeout=1.0, runner=fast_runner)
        res = probe.execute_chrony_subcommand("tracking")
        self.assertEqual(res["returncode"], 0)
        self.assertFalse(res["timed_out"])
        self.assertGreaterEqual(res["duration_seconds"], 0.005)
        self.assertLess(res["duration_seconds"], 0.5)
        self.assertIn("host_observed_utc", res)
        self.assertIn("host_observed_unix", res)

        # Timed out runner (> 1.0s)
        def timeout_runner(cmd, timeout):
            return -1, "", "Command timed out"

        probe_timeout = ChronyProbe(command_timeout=1.0, runner=timeout_runner)
        res_timeout = probe_timeout.execute_chrony_subcommand("tracking")
        self.assertEqual(res_timeout["returncode"], -1)
        self.assertTrue(res_timeout["timed_out"])

    def test_os_pressure_probe_cpu_stat_deltas_and_steal(self):
        """Verifies /proc/stat tick delta calculation for hypervisor steal and iowait."""
        proc_dir = self.tmppath / "proc"
        proc_dir.mkdir(parents=True, exist_ok=True)
        stat_file = proc_dir / "stat"

        # Sample 1: initial ticks
        stat_file.write_text("cpu  1000 0 500 8000 200 0 50 100 0 0\n")
        probe = OSPressureProbe(proc_root=proc_dir)
        s1 = probe.read_stat()
        self.assertIsNone(s1["deltas"])  # First sample establishes baseline

        # Sample 2: ticks incremented (50 steal ticks out of 200 total delta -> 25% steal)
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

        # Missing PSI returns enabled=False without crashing
        probe = OSPressureProbe(proc_root=proc_dir)
        psi_cpu = probe.read_psi("cpu")
        self.assertFalse(psi_cpu["enabled"])

        # Populate PSI
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

        # Loadavg
        (proc_dir / "loadavg").write_text("0.45 0.30 0.15 2/180 12345\n")
        load = probe.read_loadavg()
        self.assertEqual(load["load_1m"], 0.45)
        self.assertEqual(load["runnable_entities"], 2)
        self.assertEqual(load["total_entities"], 180)

    def test_bounded_ring_buffer_permissions_and_size_capping(self):
        """Verifies file permission 0600 and bounded size trimming when exceeding max_bytes."""
        buf_file = self.tmppath / "telemetry-ring.jsonl"
        # Minimum bounded size: 1 MB
        max_bytes = 1024 * 1024
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

        # Read back records
        records = ring.read_records()
        self.assertGreater(len(records), 0)
        # Sequence numbers in file must be sequential
        seqs = [r["seq"] for r in records]
        self.assertEqual(seqs, sorted(seqs))

    def test_host_telemetry_recorder_lifecycle_and_summary(self):
        """Verifies coordinated recording loop, sampling cadences, and window summary."""
        output_file = self.tmppath / "daemon-test.jsonl"

        def mock_chrony_runner(cmd, timeout):
            # Returns valid CSV tracking
            return 0, "A9FEA97B,169.254.169.123,4,1794112023.0,0.000001,0.000001,0.000001,0.1,0.001,0.001,16.2,Normal", ""

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

        # Run background recorder for 1.2s (~6 OS samples, ~3 chrony samples)
        recorder.start_background()
        time.sleep(1.2)
        recorder.stop()

        records = recorder.ring_buffer.read_records()
        self.assertGreaterEqual(len(records), 3)

        chrony_samples = [r for r in records if r.get("chrony") is not None]
        self.assertGreaterEqual(len(chrony_samples), 1)

        summary = recorder.summarize_window()
        self.assertEqual(summary["record_count"], len(records))
        self.assertGreaterEqual(summary["chrony_sample_count"], 1)
        self.assertIsNotNone(summary["chrony_timing"]["avg_duration_s"])
        self.assertEqual(summary["chrony_timing"]["timeouts"], 0)
        self.assertEqual(summary["chrony_timing"]["slow_commands_over_1s"], 0)


if __name__ == "__main__":
    unittest.main()
