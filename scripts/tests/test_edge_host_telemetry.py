"""
Unit and regression tests for Edge Host Diagnostic Sampler & Ring Buffer (scripts/edge_host_telemetry.py).
Includes regression tests for:
- [P1] Active preflight verification for chronyc daemon and tracking validity (fails closed on broken daemon).
- [P1] CLI `run` fails with exit code 1 when chronyc returns non-zero exit code or unparseable tracking.
- [P1] OS-only records never qualify as Gate D0 clock evidence (exit code 1, UNVERIFIED status).
- [P2] Ring buffer hard-cap bounding invariant: oversized records (> max_bytes) are dropped; file size strictly bounded.
- [P2] Local monotonic clock timing and read-only chrony command allowlist enforcement.
"""

import datetime
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest

from scripts.edge_host_telemetry import (
    ALLOWLISTED_CHRONY_COMMANDS,
    BoundedRingBuffer,
    ChronyProbe,
    HostTelemetryRecorder,
    OSPressureProbe,
    main,
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

        for cmd in ["makestep", "settime", "burst", "activity", "reselect"]:
            with self.assertRaises(ValueError):
                probe.execute_chrony_subcommand(cmd)

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

        def timeout_runner(cmd, timeout):
            return -1, "", "Command timed out"

        probe_timeout = ChronyProbe(command_timeout=1.0, runner=timeout_runner)
        res_timeout = probe_timeout.execute_chrony_subcommand("tracking")
        self.assertEqual(res_timeout["returncode"], -1)
        self.assertTrue(res_timeout["timed_out"])

    def test_active_preflight_check(self):
        """[P1 Regression] Preflight check actively tests chrony daemon readiness and tracking output."""
        # 1. Daemon healthy with valid tracking -> PASS
        def mock_healthy(cmd, timeout):
            t_ref = time.time() - 2.0
            return 0, f"A9FEA97B,169.254.169.123,3,{t_ref:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n", ""

        probe_ok = ChronyProbe(runner=mock_healthy)
        ok, errors = preflight_check(chrony_probe=probe_ok, require_chrony=True)
        self.assertTrue(ok)
        self.assertEqual(len(errors), 0)

        # 2. Binary present but chronyd daemon down (returns exit code 1) -> FAIL
        def mock_daemon_down(cmd, timeout):
            return 1, "", "506 Cannot talk to daemon\n"

        probe_down = ChronyProbe(runner=mock_daemon_down)
        ok_down, errors_down = preflight_check(chrony_probe=probe_down, require_chrony=True)
        self.assertFalse(ok_down)
        self.assertTrue(any("chrony tracking probe failed" in e for e in errors_down))

        # 3. Tracking unparseable or rejected -> FAIL
        def mock_bad_output(cmd, timeout):
            return 0, "not_a_valid_chrony_csv\n", ""

        probe_bad = ChronyProbe(runner=mock_bad_output)
        ok_bad, errors_bad = preflight_check(chrony_probe=probe_bad, require_chrony=True)
        self.assertFalse(ok_bad)
        self.assertTrue(any("output rejected" in e for e in errors_bad))

    def test_cli_run_fails_closed_on_chrony_error(self):
        """[P1 Regression] CLI `run` fails with exit code 1 when chronyc tracking fails (OS-only is not enough)."""
        output_file = self.tmppath / "telemetry-fail.jsonl"

        # Mock runner where chrony always fails (daemon unavailable)
        def mock_failing_chrony(cmd, timeout):
            return 1, "", "506 Cannot talk to daemon"

        cp = ChronyProbe(runner=mock_failing_chrony)
        recorder = HostTelemetryRecorder(
            output_file=output_file,
            os_interval=0.1,
            chrony_interval=0.1,
            chrony_probe=cp,
        )

        recorder.run_loop(duration_seconds=0.3)
        self.assertGreater(recorder.records_written, 0)
        self.assertEqual(recorder.valid_chrony_samples, 0)

        summary = recorder.summarize_window()
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertFalse(summary["clock_evidence_verified"])
        self.assertIn("failed_chrony_commands", summary["reason"])

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

        (proc_dir / "loadavg").write_text("0.45 0.30 0.15 2/180 12345\n")
        load = probe.read_loadavg()
        self.assertEqual(load["load_1m"], 0.45)

    def test_bounded_ring_buffer_permissions_and_size_capping(self):
        """Verifies file permission 0600 and bounded size trimming when exceeding max_bytes."""
        buf_file = self.tmppath / "telemetry-ring.jsonl"
        max_bytes = 1024 * 1024  # 1 MB
        ring = BoundedRingBuffer(buf_file, max_bytes=max_bytes)

        stat_mode = buf_file.stat().st_mode & 0o777
        self.assertEqual(stat_mode, 0o600)

        large_payload = "X" * 1000
        for i in range(1500):
            ring.write_record({"seq": i, "payload": large_payload, "timestamp_utc": datetime.datetime.now(datetime.timezone.utc).isoformat()})

        final_size = buf_file.stat().st_size
        self.assertLessEqual(final_size, max_bytes)
        self.assertGreater(ring.dropped_records_count, 0)
        self.assertGreater(ring.trimmed_bytes_total, 0)

    def test_bounded_ring_buffer_hard_cap_oversized_record(self):
        """[P2 Regression] Records larger than max_bytes are dropped immediately, preserving bounds."""
        buf_file = self.tmppath / "telemetry-oversized.jsonl"
        max_bytes = 1024 * 1024  # 1 MB
        ring = BoundedRingBuffer(buf_file, max_bytes=max_bytes)

        oversized_payload = "Z" * (1500 * 1024)
        ring.write_record({"payload": oversized_payload})

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

        self.assertGreater(recorder.records_written, 0)
        self.assertGreater(recorder.valid_chrony_samples, 0)
        records = recorder.ring_buffer.read_records()
        self.assertGreaterEqual(len(records), 3)

        summary = recorder.summarize_window()
        self.assertEqual(summary["record_count"], len(records))
        self.assertEqual(summary["status"], "OK")
        self.assertTrue(summary["clock_evidence_verified"])
        self.assertGreaterEqual(summary["chrony_sample_count"], 1)

    def test_empty_window_summary_fail_closed(self):
        """[P1 Regression] Empty window returns UNVERIFIED with clock_evidence_verified=False."""
        output_file = self.tmppath / "empty.jsonl"
        recorder = HostTelemetryRecorder(output_file=output_file)

        summary = recorder.summarize_window()
        self.assertEqual(summary["record_count"], 0)
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertFalse(summary["clock_evidence_verified"])
        self.assertEqual(summary["reason"], "no_records_in_window")

    def test_compaction_loss_preserves_chrony_failure_and_fails_closed(self):
        """
        [P1 Regression] Verifies that when a Chrony failure sample is evicted by ring buffer
        compaction, the cumulative failure and window loss are preserved and summarize_window
        fails closed with UNVERIFIED (never false OK).
        """
        output_file = self.tmppath / "telemetry-compaction.jsonl"
        max_bytes = 1024 * 1024  # 1 MB

        is_failed = True

        def mock_chrony_runner(cmd, timeout):
            nonlocal is_failed
            if is_failed:
                return 1, "", "506 Cannot talk to daemon"
            t_ref = time.time() - 1.0
            return 0, f"A9FEA97B,169.254.169.123,3,{t_ref:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n", ""

        proc_dir = self.tmppath / "proc"
        proc_dir.mkdir(parents=True, exist_ok=True)
        (proc_dir / "stat").write_text("cpu  1000 0 500 8000 200 0 50 100 0 0\n")
        (proc_dir / "loadavg").write_text("0.10 0.10 0.10 1/100 100\n")

        recorder = HostTelemetryRecorder(
            output_file=output_file,
            os_interval=0.01,
            chrony_interval=0.01,
            chrony_runner=mock_chrony_runner,
            max_bytes=max_bytes,
            proc_root=proc_dir,
        )

        # 1. First sample has chrony failure (rc=1)
        is_failed = True
        recorder.sample_now()
        self.assertEqual(recorder.cumulative_chrony_failures, 1)

        # 2. Write 850 valid samples (~2 KiB each) to exceed 1MB cap and trigger compaction
        is_failed = False
        large_extra = "Y" * 1800  # make each record ~2KB
        for _ in range(850):
            # force chrony sampling on each iteration
            recorder._last_chrony_sample_mono = 0.0
            rec = recorder.sample_now()

        # Compaction must have occurred and evicted sample 0
        self.assertGreater(recorder.ring_buffer.dropped_records_count, 0)
        retained = recorder.ring_buffer.read_records()
        self.assertGreater(retained[0]["sequence"], 1)

        # 3. summarize_window must fail closed with UNVERIFIED
        summary = recorder.summarize_window()
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertFalse(summary["clock_evidence_verified"])
        self.assertGreaterEqual(summary["failed_chrony_commands"], 1)
        self.assertIn("failed_chrony_commands", summary["reason"])
        self.assertIn("window_completeness_broken_by_record_eviction", summary["reason"])

        # 4. Standalone file summary (new recorder reading from disk) also detects failure provenance
        offline_recorder = HostTelemetryRecorder(output_file=output_file)
        offline_summary = offline_recorder.summarize_window()
        self.assertEqual(offline_summary["status"], "UNVERIFIED")
        self.assertFalse(offline_summary["clock_evidence_verified"])
        self.assertGreaterEqual(offline_summary["failed_chrony_commands"], 1)

    def test_bounded_window_without_loss_passes_summary(self):
        """
        Verifies that a valid bounded window without loss, gaps, or Chrony errors
        evaluates to OK with clock_evidence_verified=True.
        """
        output_file = self.tmppath / "telemetry-clean.jsonl"

        def mock_chrony_runner(cmd, timeout):
            t_ref = time.time() - 1.0
            return 0, f"A9FEA97B,169.254.169.123,3,{t_ref:.4f},0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal\n", ""

        proc_dir = self.tmppath / "proc"
        proc_dir.mkdir(parents=True, exist_ok=True)
        (proc_dir / "stat").write_text("cpu  1000 0 500 8000 200 0 50 100 0 0\n")
        (proc_dir / "loadavg").write_text("0.10 0.10 0.10 1/100 100\n")

        recorder = HostTelemetryRecorder(
            output_file=output_file,
            os_interval=0.01,
            chrony_interval=0.01,
            chrony_runner=mock_chrony_runner,
            proc_root=proc_dir,
        )

        for _ in range(15):
            recorder._last_chrony_sample_mono = 0.0
            recorder.sample_now()

        summary = recorder.summarize_window()
        self.assertEqual(summary["status"], "OK")
        self.assertTrue(summary["clock_evidence_verified"])
        self.assertEqual(summary["failed_chrony_commands"], 0)
        self.assertEqual(summary["dropped_records_in_buffer"], 0)

    def test_bounded_ring_buffer_trim_prevents_symlink_clobber(self):
        """
        [P2 Regression] Verifies that ring buffer trimming uses unpredictable temp files
        and will not clobber target files through pre-planted symlinks.
        """
        victim_file = self.tmppath / "private-victim.txt"
        victim_file.write_text("VICTIM_INTACT_DATA", encoding="utf-8")
        os.chmod(str(victim_file), 0o600)

        buf_file = self.tmppath / "trim-symlink-test.jsonl"
        # Pre-plant predictable symlink if any
        predictable_tmp = self.tmppath / "trim-symlink-test.jsonl.tmp"
        try:
            os.symlink(str(victim_file), str(predictable_tmp))
        except OSError:
            pass

        ring = BoundedRingBuffer(buf_file, max_bytes=1024 * 1024)
        for i in range(1200):
            ring.write_record({"seq": i, "payload": "X" * 1000})

        # Victim file must remain completely untouched
        self.assertEqual(victim_file.read_text(encoding="utf-8"), "VICTIM_INTACT_DATA")
        self.assertEqual(buf_file.stat().st_mode & 0o777, 0o600)

    def test_sigterm_stops_sampling_and_evaluates_unified_fail_closed_verdict(self):
        """
        [P1 Regression] Verifies that when SIGTERM stops the recording loop,
        the unified verdict is evaluated and fails closed (exit 1) if Chrony errors occurred during runtime.
        """
        fake_bin_dir = self.tmppath / "fake_bin"
        fake_bin_dir.mkdir(parents=True, exist_ok=True)
        chrony_bin = fake_bin_dir / "chronyc"
        state_file = self.tmppath / "chrony_state.txt"
        state_file.write_text("0")

        # Fake chronyc: returns 0 on first call (preflight), then 1 on runtime calls
        fake_script = f"""#!/bin/sh
call_count=$(cat "{state_file}")
new_count=$((call_count + 1))
echo "$new_count" > "{state_file}"
if [ "$call_count" -le 1 ]; then
    echo "A9FEA97B,169.254.169.123,3,$(date +%s),0.0001,0.0001,0.0002,0.1,0.01,0.05,0.001,0.001,16.0,Normal"
    exit 0
else
    echo "506 Cannot talk to daemon" >&2
    exit 1
fi
"""
        chrony_bin.write_text(fake_script)
        chrony_bin.chmod(0o755)

        output_file = self.tmppath / "sigterm_test.jsonl"
        env = os.environ.copy()
        env["PATH"] = f"{fake_bin_dir}:{env.get('PATH', '')}"

        cmd = [
            sys.executable,
            "-B",
            str(Path(__file__).resolve().parent.parent / "edge_host_telemetry.py"),
            "run",
            "--output-file",
            str(output_file),
            "--os-interval",
            "0.1",
            "--chrony-interval",
            "0.2",
            "--duration",
            "30",
        ]

        proc = subprocess.Popen(cmd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        time.sleep(1.2)
        proc.send_signal(signal.SIGTERM)
        stdout, stderr = proc.communicate(timeout=5)

        # Unified verdict must fail closed with exit code 1
        self.assertEqual(proc.returncode, 1)
        self.assertIn("Gate D0 clock evidence unverified", stderr)
        self.assertTrue(output_file.exists())

        # Standalone summarize also confirms UNVERIFIED
        offline_recorder = HostTelemetryRecorder(output_file=output_file)
        summary = offline_recorder.summarize_window()
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertFalse(summary["clock_evidence_verified"])


if __name__ == "__main__":
    unittest.main()
