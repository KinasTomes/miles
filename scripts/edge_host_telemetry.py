"""
Edge host diagnostic sampler and ring buffer telemetry recorder.

Deployed on Edge hosts (e.g. edge-pilot) during benchmark workloads to provide
high-resolution, contemporaneous evidence distinguishing between:
- Hypothesis A: NTP poll starvation / sample age exceeding 2*interval+2
- Hypothesis B: Local chronyc command execution latency exceeding 1.0s context timeout
- Hypervisor CPU credit exhaustion (CPU steal ticks / PSI pressure)
- Local disk I/O stall (iowait ticks / PSI I/O pressure)

Strict Guardrails:
- Zero sensitive data leakage: records numerical metrics and chrony status only.
- Read-only to chrony: only runs allowlisted read commands (tracking, sources, sourcestats).
- Local execution duration measurement: nanosecond monotonic timers capture true host duration.
- Bounded private ring buffer: strictly bounded to <= max_bytes in /run/miles with 0600 permissions.
- Hard cap invariant: oversized records (> max_bytes) are safely dropped; file size strictly bounded.
- Preflight validation with active daemon probe: fails closed if chronyc daemon is down or unparseable.
- Fail-closed empty-window and chrony failure propagation: OS-only records never qualify as clock evidence.
"""

import argparse
import datetime
import json
import math
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from typing import Any, Callable, Dict, List, Optional, Tuple

# Ensure scripts directory and repo root are in sys.path regardless of execution CWD
_this_dir = Path(__file__).resolve().parent
_repo_root = _this_dir.parent
for _p in [str(_repo_root), str(_this_dir)]:
    if _p not in sys.path:
        sys.path.insert(0, _p)

try:
    from scripts.runner_diagnostics import parse_chrony_tracking
except ImportError:
    try:
        from runner_diagnostics import parse_chrony_tracking
    except ImportError:
        # Fallback for standalone execution on remote hosts
        parse_chrony_tracking = None  # type: ignore


ALLOWLISTED_CHRONY_COMMANDS = frozenset({"tracking", "sources", "sourcestats", "serverstats"})


def _iso_utc_now() -> str:
    """Returns current UTC timestamp in ISO 8601 format with microsecond precision."""
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


def _parse_iso_timestamp(ts: Optional[str]) -> Optional[datetime.datetime]:
    if not ts:
        return None
    try:
        if ts.endswith("Z"):
            ts = ts[:-1] + "+00:00"
        return datetime.datetime.fromisoformat(ts)
    except Exception:
        return None


def preflight_check(
    chrony_probe: Optional["ChronyProbe"] = None,
    require_chrony: bool = True,
) -> Tuple[bool, List[str]]:
    """
    Verifies that required dependencies and tools are present and functional before sampling:
    - parse_chrony_tracking parser is loaded.
    - chronyc executable is available on PATH (or mock runner provided).
    - Active chronyc probe passes (daemon is running and returns valid, bounded tracking data).
    """
    errors: List[str] = []

    if parse_chrony_tracking is None:
        errors.append("parse_chrony_tracking parser function is unavailable (import failed)")

    if require_chrony:
        probe = chrony_probe or ChronyProbe()
        if probe._runner == probe._default_runner:
            if not shutil.which("chronyc"):
                errors.append("chronyc binary not found on system PATH")
                return (False, errors)

        # Active probe to verify daemon readiness and tracking output
        sample = probe.sample_full(sample_sources=False)
        t_res = sample.get("tracking", {})
        rc = t_res.get("returncode", -1)
        if rc != 0:
            err_msg = t_res.get("stderr", "").strip() or f"exit code {rc}"
            errors.append(f"chrony tracking probe failed: {err_msg}")
        elif t_res.get("timed_out", False):
            errors.append(f"chrony tracking probe timed out after {probe.command_timeout}s")
        else:
            parsed = sample.get("parsed_tracking")
            if not parsed or not isinstance(parsed, dict) or parsed.get("status") != "ok":
                reason = parsed.get("rejection_reason") if isinstance(parsed, dict) else "unparseable_output"
                errors.append(f"chrony tracking output rejected during preflight: {reason}")
            elif parsed.get("uncertainty_exceeded", False):
                reason = parsed.get("rejection_reason", "uncertainty_exceeded")
                errors.append(f"chrony clock uncertainty exceeded during preflight: {reason}")

    return (len(errors) == 0, errors)


class ChronyProbe:
    """
    Executes and parses host chronyc commands with high-precision monotonic timing.
    Strictly read-only: rejects any command not in the allowlist.
    """

    def __init__(
        self,
        command_timeout: float = 1.0,
        runner: Optional[Callable[[List[str], float], Tuple[int, str, str]]] = None,
    ):
        self.command_timeout = command_timeout
        self._runner = runner or self._default_runner

    @staticmethod
    def _default_runner(cmd: List[str], timeout: float) -> Tuple[int, str, str]:
        t0 = time.perf_counter()
        try:
            res = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
            return res.returncode, res.stdout, res.stderr
        except subprocess.TimeoutExpired as exc:
            stdout = exc.stdout.decode() if isinstance(exc.stdout, bytes) else (exc.stdout or "")
            stderr = exc.stderr.decode() if isinstance(exc.stderr, bytes) else (exc.stderr or "")
            return -1, stdout, stderr

    def execute_chrony_subcommand(self, subcommand: str) -> Dict[str, Any]:
        """
        Executes a single chronyc subcommand, returning execution timings and stdout.
        """
        if subcommand not in ALLOWLISTED_CHRONY_COMMANDS:
            raise ValueError(f"Unauthorized chrony command: {subcommand!r}. Allowed: {sorted(ALLOWLISTED_CHRONY_COMMANDS)}")

        cmd = ["chronyc", "-n", "-c", subcommand]
        t_start_utc = _iso_utc_now()
        t_start_unix = time.time()
        t_start_mono = time.perf_counter()

        returncode, stdout, stderr = self._runner(cmd, self.command_timeout)

        t_finish_mono = time.perf_counter()
        t_finish_utc = _iso_utc_now()
        duration_s = round(t_finish_mono - t_start_mono, 6)

        timed_out = returncode == -1 or duration_s >= self.command_timeout

        return {
            "subcommand": subcommand,
            "cmd": cmd,
            "host_observed_utc": t_start_utc,
            "host_observed_finish_utc": t_finish_utc,
            "host_observed_unix": t_start_unix,
            "duration_seconds": duration_s,
            "timed_out": timed_out,
            "returncode": returncode,
            "stdout": stdout,
            "stderr": stderr,
        }

    def sample_full(self, sample_sources: bool = True) -> Dict[str, Any]:
        """
        Performs a full chrony sample: tracking, and optionally sources and sourcestats.
        """
        t_res = self.execute_chrony_subcommand("tracking")
        res: Dict[str, Any] = {
            "tracking": t_res,
            "command_duration_seconds": t_res["duration_seconds"],
            "command_timed_out": t_res["timed_out"],
            "returncode": t_res["returncode"],
            "parsed_tracking": None,
        }

        # Parse tracking metrics if available
        if t_res["returncode"] == 0 and t_res["stdout"]:
            if parse_chrony_tracking is not None:
                parsed = parse_chrony_tracking(
                    t_res["stdout"],
                    now_unix=t_res["host_observed_unix"],
                    elapsed_seconds=t_res["duration_seconds"],
                    runtime_elapsed_verified=True,
                )
                res["parsed_tracking"] = parsed

        if sample_sources:
            try:
                res["sources"] = self.execute_chrony_subcommand("sources")
            except Exception as e:
                res["sources"] = {"error": str(e)}
            try:
                res["sourcestats"] = self.execute_chrony_subcommand("sourcestats")
            except Exception as e:
                res["sourcestats"] = {"error": str(e)}

        return res


class OSPressureProbe:
    """
    Samples Linux kernel scheduler and resource pressure metrics from /proc.
    Calculates CPU steal ticks & deltas (hypervisor credit exhaustion) and
    PSI (Pressure Stall Information) for CPU and I/O.
    """

    def __init__(self, proc_root: Path = Path("/proc")):
        self.proc_root = Path(proc_root)
        self._prev_stat_ticks: Optional[Dict[str, int]] = None

    def read_stat(self) -> Dict[str, Any]:
        """Reads /proc/stat CPU tick counters and calculates deltas."""
        stat_file = self.proc_root / "stat"
        if not stat_file.exists():
            return {"error": "proc_stat_unavailable"}

        ticks: Dict[str, int] = {}
        try:
            content = stat_file.read_text(encoding="utf-8")
            for line in content.splitlines():
                if line.startswith("cpu "):
                    parts = line.split()
                    keys = ["user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal", "guest", "guest_nice"]
                    for k, v in zip(keys, parts[1:]):
                        ticks[k] = int(v)
                    break
        except Exception as e:
            return {"error": f"read_error: {e}"}

        res: Dict[str, Any] = {"ticks": ticks, "deltas": None}

        if self._prev_stat_ticks is not None and ticks:
            deltas = {k: max(0, ticks.get(k, 0) - self._prev_stat_ticks.get(k, 0)) for k in ticks}
            total_delta = sum(deltas.values())
            res["deltas"] = deltas
            res["total_delta_ticks"] = total_delta
            if total_delta > 0:
                res["steal_pct"] = round(100.0 * deltas.get("steal", 0) / total_delta, 2)
                res["iowait_pct"] = round(100.0 * deltas.get("iowait", 0) / total_delta, 2)
                res["user_pct"] = round(100.0 * deltas.get("user", 0) / total_delta, 2)
                res["system_pct"] = round(100.0 * deltas.get("system", 0) / total_delta, 2)
                res["idle_pct"] = round(100.0 * deltas.get("idle", 0) / total_delta, 2)
            else:
                res["steal_pct"] = 0.0
                res["iowait_pct"] = 0.0

        self._prev_stat_ticks = ticks
        return res

    def read_loadavg(self) -> Dict[str, Any]:
        """Reads /proc/loadavg."""
        load_file = self.proc_root / "loadavg"
        if not load_file.exists():
            return {"error": "proc_loadavg_unavailable"}
        try:
            parts = load_file.read_text(encoding="utf-8").strip().split()
            runnable_parts = parts[3].split("/") if len(parts) >= 4 else ["0", "0"]
            return {
                "load_1m": float(parts[0]),
                "load_5m": float(parts[1]),
                "load_15m": float(parts[2]),
                "runnable_entities": int(runnable_parts[0]),
                "total_entities": int(runnable_parts[1]),
            }
        except Exception as e:
            return {"error": f"read_error: {e}"}

    def read_psi(self, resource: str) -> Dict[str, Any]:
        """Reads Pressure Stall Information (/proc/pressure/{cpu,io,memory})."""
        psi_file = self.proc_root / "pressure" / resource
        if not psi_file.exists():
            return {"enabled": False}
        try:
            content = psi_file.read_text(encoding="utf-8").strip()
            data: Dict[str, Any] = {"enabled": True}
            for line in content.splitlines():
                parts = line.split()
                if not parts:
                    continue
                kind = parts[0]  # "some" or "full"
                metrics: Dict[str, Any] = {}
                for item in parts[1:]:
                    if "=" in item:
                        k, v = item.split("=", 1)
                        metrics[k] = float(v) if "." in v else int(v)
                data[kind] = metrics
            return data
        except Exception as e:
            return {"enabled": False, "error": str(e)}

    def read_meminfo(self) -> Dict[str, Any]:
        """Reads key memory fields from /proc/meminfo."""
        mem_file = self.proc_root / "meminfo"
        if not mem_file.exists():
            return {"error": "proc_meminfo_unavailable"}
        try:
            res: Dict[str, int] = {}
            for line in mem_file.read_text(encoding="utf-8").splitlines():
                parts = line.split(":")
                if len(parts) == 2:
                    k = parts[0].strip()
                    if k in ("MemTotal", "MemFree", "MemAvailable", "Buffers", "Cached", "Dirty", "Writeback"):
                        val_str = parts[1].strip().split()[0]
                        res[k] = int(val_str)
            return res
        except Exception as e:
            return {"error": str(e)}

    def sample_all(self) -> Dict[str, Any]:
        """Collects a complete OS scheduler and pressure snapshot."""
        return {
            "stat": self.read_stat(),
            "loadavg": self.read_loadavg(),
            "psi_cpu": self.read_psi("cpu"),
            "psi_io": self.read_psi("io"),
            "meminfo": self.read_meminfo(),
        }


class BoundedRingBuffer:
    """
    Thread-safe, append-only JSONL ring buffer file with strict size bounding
    and restricted private file permissions (0600).
    When file exceeds max_bytes, trims the oldest lines to maintain bounds.
    Maintains the invariant: file.stat().st_size <= max_bytes at all times.
    """

    def __init__(self, file_path: Path, max_bytes: int = 10 * 1024 * 1024):
        self.file_path = Path(file_path)
        self.max_bytes = max(1024 * 1024, max_bytes)  # Minimum 1MB
        self._lock = threading.Lock()
        self.dropped_records_count = 0
        self.trimmed_bytes_total = 0
        self._ensure_file_exists()

    def _ensure_file_exists(self) -> None:
        self.file_path.parent.mkdir(parents=True, exist_ok=True)
        if not self.file_path.exists():
            # Create file with 0600 permissions
            fd = os.open(str(self.file_path), os.O_CREAT | os.O_WRONLY | os.O_APPEND, 0o600)
            os.close(fd)
        else:
            try:
                os.chmod(str(self.file_path), 0o600)
            except Exception:
                pass

    def write_record(self, record: Dict[str, Any]) -> None:
        """Serializes and appends a record to the bounded file."""
        line = json.dumps(record, separators=(",", ":")) + "\n"
        encoded = line.encode("utf-8")

        # Invariant guard: if a single record exceeds max_bytes, drop it immediately
        if len(encoded) > self.max_bytes:
            with self._lock:
                self.dropped_records_count += 1
                self.trimmed_bytes_total += len(encoded)
            return

        with self._lock:
            current_size = self.file_path.stat().st_size if self.file_path.exists() else 0
            if current_size + len(encoded) > self.max_bytes:
                self._trim_to_fit(len(encoded))

            # Re-check size after trimming; if still insufficient, drop record
            current_size = self.file_path.stat().st_size if self.file_path.exists() else 0
            if current_size + len(encoded) > self.max_bytes:
                self.dropped_records_count += 1
                self.trimmed_bytes_total += len(encoded)
                return

            with open(self.file_path, "a", encoding="utf-8") as f:
                f.write(line)
                f.flush()

    def _trim_to_fit(self, needed_bytes: int) -> None:
        """
        Trims oldest lines until remaining_size + needed_bytes <= max_bytes,
        freeing at least 30% headroom.
        """
        if not self.file_path.exists():
            return
        try:
            with open(self.file_path, "r", encoding="utf-8") as f:
                lines = f.readlines()
            if not lines:
                return

            line_bytes = [len(l.encode("utf-8")) for l in lines]
            total_size = sum(line_bytes)

            # Target size: leave 30% buffer below max_bytes or enough room for needed_bytes
            target_size = min(int(self.max_bytes * 0.7), self.max_bytes - needed_bytes)
            if target_size < 0:
                target_size = 0

            dropped_count = 0
            dropped_bytes = 0
            current_size = total_size

            while dropped_count < len(lines) and current_size > target_size:
                dropped_bytes += line_bytes[dropped_count]
                current_size -= line_bytes[dropped_count]
                dropped_count += 1

            # Ensure at least 1 line is dropped if capacity is breached
            if dropped_count == 0 and len(lines) > 0 and (total_size + needed_bytes > self.max_bytes):
                dropped_count = 1
                dropped_bytes = line_bytes[0]

            keep_lines = lines[dropped_count:]
            fd, tmp_file_path = tempfile.mkstemp(
                prefix=f".{self.file_path.stem}_",
                suffix=".tmp",
                dir=str(self.file_path.parent),
            )
            tmp_path = Path(tmp_file_path)
            try:
                os.fchmod(fd, 0o600)
                with os.fdopen(fd, "w", encoding="utf-8") as f:
                    f.writelines(keep_lines)
                    f.flush()
                    os.fsync(f.fileno())
                os.replace(str(tmp_path), str(self.file_path))
                os.chmod(str(self.file_path), 0o600)
            finally:
                if tmp_path.exists() or os.path.islink(str(tmp_path)):
                    try:
                        tmp_path.unlink()
                    except Exception:
                        pass

            self.dropped_records_count += dropped_count
            self.trimmed_bytes_total += dropped_bytes
        except Exception:
            pass

    def read_records(
        self,
        since_utc: Optional[str] = None,
        until_utc: Optional[str] = None,
    ) -> List[Dict[str, Any]]:
        """Reads records from the ring buffer, optionally filtered by UTC timestamps."""
        if not self.file_path.exists():
            return []

        since_dt = _parse_iso_timestamp(since_utc)
        until_dt = _parse_iso_timestamp(until_utc)

        records = []
        with self._lock:
            with open(self.file_path, "r", encoding="utf-8") as f:
                for line in f:
                    line = line.strip()
                    if not line:
                        continue
                    try:
                        rec = json.loads(line)
                        ts_str = rec.get("timestamp_utc")
                        if since_dt or until_dt:
                            rec_dt = _parse_iso_timestamp(ts_str)
                            if rec_dt:
                                if since_dt and rec_dt < since_dt:
                                    continue
                                if until_dt and rec_dt > until_dt:
                                    continue
                        records.append(rec)
                    except Exception:
                        continue
        return records


class HostTelemetryRecorder:
    """
    Orchestrates high-frequency diagnostic sampling on an Edge host.
    Combines ChronyProbe, OSPressureProbe, and BoundedRingBuffer into a coordinated recorder.
    Tracks records_written vs valid_chrony_samples to ensure OS-only samples never masquerade
    as clock evidence.
    """

    def __init__(
        self,
        output_file: Path,
        os_interval: float = 1.0,
        chrony_interval: float = 2.0,
        chrony_timeout: float = 1.0,
        max_bytes: int = 10 * 1024 * 1024,
        chrony_probe: Optional[ChronyProbe] = None,
        chrony_runner: Optional[Callable[[List[str], float], Tuple[int, str, str]]] = None,
        proc_root: Path = Path("/proc"),
    ):
        self.output_file = Path(output_file)
        self.os_interval = max(0.2, os_interval)
        self.chrony_interval = max(0.5, chrony_interval)
        self.ring_buffer = BoundedRingBuffer(self.output_file, max_bytes=max_bytes)
        self.chrony_probe = chrony_probe or ChronyProbe(command_timeout=chrony_timeout, runner=chrony_runner)
        self.os_probe = OSPressureProbe(proc_root=proc_root)

        self._running = False
        self._thread: Optional[threading.Thread] = None
        self._sequence = 0
        self._last_chrony_sample_mono = 0.0

        # Separate bookkeeping: raw records written vs valid verified chrony samples
        self.records_written = 0
        self.valid_chrony_samples = 0
        self.consecutive_chrony_errors = 0
        self.max_consecutive_chrony_errors = 0

        # Cumulative durability counters (survive ring buffer compaction/eviction)
        self.cumulative_chrony_attempts = 0
        self.cumulative_chrony_failures = 0
        self.cumulative_chrony_timeouts = 0
        self.cumulative_uncertainty_violations = 0

    def sample_now(self) -> Dict[str, Any]:
        """Takes an immediate coordinated sample and writes to the ring buffer."""
        self._sequence += 1
        now_utc = _iso_utc_now()
        now_unix = time.time()
        mono_now = time.perf_counter()

        # OS Pressure sample
        os_data = self.os_probe.sample_all()

        # Check if chrony should be sampled
        chrony_data = None
        if (mono_now - self._last_chrony_sample_mono) >= (self.chrony_interval * 0.95) or self._last_chrony_sample_mono == 0.0:
            chrony_data = self.chrony_probe.sample_full()
            self._last_chrony_sample_mono = mono_now
            self.cumulative_chrony_attempts += 1

            # Track validity of chrony sample
            t_res = chrony_data.get("tracking", {})
            pt = chrony_data.get("parsed_tracking")
            cmd_failed = (t_res.get("returncode") != 0)
            timed_out = t_res.get("timed_out", False)
            if cmd_failed:
                self.cumulative_chrony_failures += 1
            if timed_out:
                self.cumulative_chrony_timeouts += 1

            uncertainty_violated = (
                not isinstance(pt, dict)
                or pt.get("status") != "ok"
                or pt.get("uncertainty_exceeded", False)
            )
            if uncertainty_violated:
                self.cumulative_uncertainty_violations += 1

            sample_ok = not cmd_failed and not timed_out and not uncertainty_violated
            if sample_ok:
                self.valid_chrony_samples += 1
                self.consecutive_chrony_errors = 0
            else:
                self.consecutive_chrony_errors += 1
                if self.consecutive_chrony_errors > self.max_consecutive_chrony_errors:
                    self.max_consecutive_chrony_errors = self.consecutive_chrony_errors

        record: Dict[str, Any] = {
            "sequence": self._sequence,
            "timestamp_utc": now_utc,
            "timestamp_unix": now_unix,
            "os_pressure": os_data,
            "chrony": chrony_data,
            "provenance": {
                "sequence": self._sequence,
                "cumulative_chrony_failures": self.cumulative_chrony_failures,
                "cumulative_chrony_timeouts": self.cumulative_chrony_timeouts,
                "cumulative_uncertainty_violations": self.cumulative_uncertainty_violations,
                "dropped_records_prior": self.ring_buffer.dropped_records_count,
            },
        }

        self.ring_buffer.write_record(record)
        self.records_written += 1
        return record

    def run_loop(self, duration_seconds: Optional[float] = None) -> None:
        """Runs the sampling loop in the foreground until stopped or duration expires."""
        self._running = True
        t0 = time.perf_counter()

        # Prime initial os_probe delta
        self.os_probe.sample_all()

        while self._running:
            if duration_seconds is not None and (time.perf_counter() - t0) >= duration_seconds:
                break
            try:
                self.sample_now()
            except Exception:
                # Never crash the diagnostic loop on transient sampling errors
                pass

            time.sleep(self.os_interval)

        self._running = False

    def start_background(self) -> None:
        """Starts the sampling loop in a daemon background thread."""
        if self._running:
            return
        self._running = True
        self._thread = threading.Thread(target=self.run_loop, daemon=True)
        self._thread.start()

    def stop(self) -> None:
        """Signals the loop to stop and waits for completion."""
        self._running = False
        if self._thread and self._thread.is_alive():
            self._thread.join(timeout=3.0)

    def summarize_window(
        self,
        since_utc: Optional[str] = None,
        until_utc: Optional[str] = None,
    ) -> Dict[str, Any]:
        """
        Analyzes records in the ring buffer for a given window, producing key
        epistemological indicators for RCA hypothesis evaluation.
        Fails closed on missing or unverified chrony clock evidence.
        """
        records = self.ring_buffer.read_records(since_utc, until_utc)
        if not records:
            return {
                "record_count": 0,
                "status": "UNVERIFIED",
                "clock_evidence_verified": False,
                "reason": "no_records_in_window",
            }

        chrony_records = [r for r in records if r.get("chrony") is not None]
        durations = [r["chrony"]["command_duration_seconds"] for r in chrony_records if "command_duration_seconds" in r["chrony"]]
        timeouts = [r for r in chrony_records if r["chrony"].get("command_timed_out", False)]
        slow_chrony = [d for d in durations if d >= 1.0]

        # Tracking sample ages and drift
        sample_ages = []
        drifts = []
        offsets = []
        uncertainty_violations = 0
        failed_commands = 0

        for r in chrony_records:
            t_res = r["chrony"].get("tracking", {})
            if t_res.get("returncode") != 0 or t_res.get("timed_out", False):
                failed_commands += 1

            pt = r["chrony"].get("parsed_tracking")
            if isinstance(pt, dict) and pt.get("status") == "ok":
                if pt.get("uncertainty_exceeded", False):
                    uncertainty_violations += 1
                sa = pt.get("sample_age_seconds")
                if sa is not None:
                    sample_ages.append(sa)
                d = pt.get("drift_ppm")
                if d is not None:
                    drifts.append(d)
                off = pt.get("offset_seconds")
                if off is not None:
                    offsets.append(off)
            else:
                uncertainty_violations += 1

        # OS steal and iowait
        steal_percentages = []
        iowait_percentages = []
        for r in records:
            stat = r.get("os_pressure", {}).get("stat", {})
            if "steal_pct" in stat:
                steal_percentages.append(stat["steal_pct"])
            if "iowait_pct" in stat:
                iowait_percentages.append(stat["iowait_pct"])

        # Sequence continuity and compaction loss checks
        first_seq = records[0].get("sequence") or records[0].get("provenance", {}).get("sequence")
        last_seq = records[-1].get("sequence") or records[-1].get("provenance", {}).get("sequence")
        expected_records_in_span = (last_seq - first_seq + 1) if (first_seq is not None and last_seq is not None) else len(records)
        internal_sequence_gap = max(0, expected_records_in_span - len(records))

        # Check provenance from records themselves (survives offline restart/reading from file)
        last_prov = records[-1].get("provenance", {})
        prov_chrony_failures = last_prov.get("cumulative_chrony_failures", 0)
        prov_chrony_timeouts = last_prov.get("cumulative_chrony_timeouts", 0)
        prov_uncertainty_violations = last_prov.get("cumulative_uncertainty_violations", 0)
        prov_dropped_records = last_prov.get("dropped_records_prior", 0)

        dropped_records_in_buffer = max(self.ring_buffer.dropped_records_count, prov_dropped_records)

        # Eviction / completeness check:
        # If evaluating entire recording window (no since_utc) and records were evicted,
        # or if there is an internal sequence gap, or if since_utc is earlier than first retained record:
        window_evicted = False
        if since_utc is None:
            if (first_seq is not None and first_seq > 1) or dropped_records_in_buffer > 0 or internal_sequence_gap > 0:
                window_evicted = True
        else:
            if internal_sequence_gap > 0:
                window_evicted = True
            elif records and since_utc < records[0]["timestamp_utc"]:
                window_evicted = True

        total_failed_commands = max(failed_commands, self.cumulative_chrony_failures, prov_chrony_failures)
        total_timeouts = max(len(timeouts), self.cumulative_chrony_timeouts, prov_chrony_timeouts)
        total_uncertainty_violations = max(uncertainty_violations, self.cumulative_uncertainty_violations, prov_uncertainty_violations)

        # Clock evidence is verified ONLY if we have valid chrony samples, zero timeouts,
        # zero command failures, valid non-stale sample ages, and unbroken window completeness.
        clock_evidence_verified = (
            len(chrony_records) > 0
            and total_failed_commands == 0
            and total_timeouts == 0
            and len(sample_ages) > 0
            and total_uncertainty_violations == 0
            and not window_evicted
        )

        reason = None
        if not clock_evidence_verified:
            reasons = []
            if len(chrony_records) == 0:
                reasons.append("no_chrony_samples")
            if total_failed_commands > 0:
                reasons.append(f"{total_failed_commands}_failed_chrony_commands")
            if total_timeouts > 0:
                reasons.append(f"{total_timeouts}_chrony_timeouts")
            if len(sample_ages) == 0:
                reasons.append("no_valid_tracking_parsed")
            if total_uncertainty_violations > 0:
                reasons.append(f"{total_uncertainty_violations}_uncertainty_violations")
            if window_evicted:
                reasons.append(f"window_completeness_broken_by_record_eviction({dropped_records_in_buffer}_dropped)")
            reason = "; ".join(reasons)

        return {
            "record_count": len(records),
            "status": "OK" if clock_evidence_verified else "UNVERIFIED",
            "clock_evidence_verified": clock_evidence_verified,
            "reason": reason,
            "chrony_sample_count": len(chrony_records),
            "valid_tracking_samples": len(sample_ages),
            "failed_chrony_commands": total_failed_commands,
            "window_start_utc": records[0]["timestamp_utc"],
            "window_end_utc": records[-1]["timestamp_utc"],
            "chrony_timing": {
                "count": len(durations),
                "min_duration_s": min(durations) if durations else None,
                "avg_duration_s": round(sum(durations) / len(durations), 4) if durations else None,
                "max_duration_s": max(durations) if durations else None,
                "slow_commands_over_1s": len(slow_chrony),
                "timeouts": total_timeouts,
            },
            "clock_tracking": {
                "max_sample_age_s": max(sample_ages) if sample_ages else None,
                "min_sample_age_s": min(sample_ages) if sample_ages else None,
                "max_drift_ppm": max(drifts) if drifts else None,
                "max_abs_offset_s": max(abs(x) for x in offsets) if offsets else None,
            },
            "os_pressure": {
                "max_steal_pct": max(steal_percentages) if steal_percentages else None,
                "avg_steal_pct": round(sum(steal_percentages) / len(steal_percentages), 2) if steal_percentages else None,
                "max_iowait_pct": max(iowait_percentages) if iowait_percentages else None,
                "avg_iowait_pct": round(sum(iowait_percentages) / len(iowait_percentages), 2) if iowait_percentages else None,
            },
            "dropped_records_in_buffer": dropped_records_in_buffer,
        }


def main():
    parser = argparse.ArgumentParser(description="Edge host diagnostic telemetry recorder.")
    subparsers = parser.add_subparsers(dest="subcommand", required=True)

    # run command
    run_parser = subparsers.add_parser("run", help="Run the telemetry recorder daemon.")
    run_parser.add_argument("--output-file", default="/run/miles/host-telemetry.jsonl", help="Ring buffer JSONL file path.")
    run_parser.add_argument("--os-interval", type=float, default=1.0, help="OS pressure sample interval in seconds.")
    run_parser.add_argument("--chrony-interval", type=float, default=2.0, help="Chrony sample interval in seconds.")
    run_parser.add_argument("--chrony-timeout", type=float, default=1.0, help="Chrony command timeout in seconds.")
    run_parser.add_argument("--max-bytes", type=int, default=10 * 1024 * 1024, help="Max ring buffer size in bytes (default 10MB).")
    run_parser.add_argument("--duration", type=float, default=None, help="Optional duration to run before auto-exit.")

    # probe command (one-shot)
    probe_parser = subparsers.add_parser("probe", help="Perform a single one-shot probe and output JSON to stdout.")

    # summarize command
    sum_parser = subparsers.add_parser("summarize", help="Summarize an existing ring buffer file.")
    sum_parser.add_argument("--input-file", required=True, help="Path to host-telemetry.jsonl file.")
    sum_parser.add_argument("--since", default=None, help="Start UTC timestamp filter.")
    sum_parser.add_argument("--until", default=None, help="End UTC timestamp filter.")

    args = parser.parse_args()

    if args.subcommand == "probe":
        cp = ChronyProbe()
        ok, errors = preflight_check(chrony_probe=cp, require_chrony=True)
        if not ok:
            for err in errors:
                print(f"Preflight error: {err}", file=sys.stderr)
            return 1

        op = OSPressureProbe()
        chrony_data = cp.sample_full()
        pt = chrony_data.get("parsed_tracking")
        if chrony_data.get("returncode") != 0 or not pt or pt.get("status") != "ok" or pt.get("uncertainty_exceeded"):
            print("Error: Chrony tracking failed or uncertainty exceeded during probe", file=sys.stderr)
            return 1

        os_data = op.sample_all()
        result = {
            "timestamp_utc": _iso_utc_now(),
            "timestamp_unix": time.time(),
            "chrony": chrony_data,
            "os_pressure": os_data,
        }
        print(json.dumps(result, indent=2))
        return 0

    if args.subcommand == "run":
        cp = ChronyProbe(command_timeout=args.chrony_timeout)
        ok, errors = preflight_check(chrony_probe=cp, require_chrony=True)
        if not ok:
            for err in errors:
                print(f"Preflight error: {err}", file=sys.stderr)
            return 1

        recorder = HostTelemetryRecorder(
            output_file=Path(args.output_file),
            os_interval=args.os_interval,
            chrony_interval=args.chrony_interval,
            chrony_timeout=args.chrony_timeout,
            max_bytes=args.max_bytes,
            chrony_probe=cp,
        )

        def sig_handler(sig, frame):
            recorder.stop()
            sys.exit(0)

        signal.signal(signal.SIGINT, sig_handler)
        signal.signal(signal.SIGTERM, sig_handler)

        recorder.run_loop(duration_seconds=args.duration)

        # Fail closed if 0 records written, 0 valid chrony samples, or clock evidence unverified
        if recorder.records_written == 0:
            print("Error: Telemetry run loop wrote 0 records", file=sys.stderr)
            return 1
        if recorder.valid_chrony_samples == 0:
            print("Error: Gate D0 telemetry failure: 0 valid chrony clock tracking samples collected", file=sys.stderr)
            return 1

        summary = recorder.summarize_window()
        if summary.get("status") != "OK" or not summary.get("clock_evidence_verified"):
            reason = summary.get("reason") or "clock uncertainty or timeouts in window"
            print(f"Error: Gate D0 clock evidence unverified: {reason}", file=sys.stderr)
            return 1

        return 0

    if args.subcommand == "summarize":
        p = Path(args.input_file)
        if not p.exists():
            print(f"File not found: {p}", file=sys.stderr)
            return 1
        recorder = HostTelemetryRecorder(output_file=p)
        summary = recorder.summarize_window(since_utc=args.since, until_utc=args.until)
        print(json.dumps(summary, indent=2))
        return 0 if summary.get("status") == "OK" and summary.get("clock_evidence_verified") else 1

    return 0


if __name__ == "__main__":
    sys.exit(main())
