"""
Safe runner diagnostics, journal parsing, and failure correlation utilities.

Contains no AWS credentials, private ARNs, or secret material. Usable by
both local unit/integration tests and operator test runners.
"""
import json
import re


def parse_gateway_journal_lines(stdout_text):
    """
    Parses journalctl output matching structured JSON slog lines or legacy text slog.
    Returns a normalized list of failure records, scrubbing any sensitive details.
    """
    if not stdout_text:
        return []

    out = []
    text_pattern = re.compile(
        r'(?:ERROR|WARN)\s+(?P<msg>s3\s+server\s+failure|s3\s+service\s+unavailable)\s+'
        r'.*?request_id=(?P<request_id>[A-Za-z0-9_-]+)'
        r'.*?node_id=(?P<node_id>[^\s]*)'
        r'.*?stage=(?P<stage>[^\s]+)'
        r'.*?failure_class=(?P<failure_class>[^\s]+)'
    )

    for raw_line in stdout_text.splitlines():
        line = raw_line.strip()
        if not line:
            continue

        # 1. Structured JSON line
        if line.startswith("{") and line.endswith("}"):
            try:
                d = json.loads(line)
                msg = d.get("msg")
                if msg in ("s3 server failure", "s3 service unavailable"):
                    out.append({
                        "msg": msg,
                        "request_id": d.get("request_id", ""),
                        "node_id": d.get("node_id", ""),
                        "method": d.get("method", ""),
                        "operation": d.get("operation", ""),
                        "stage": d.get("stage", ""),
                        "failure_class": d.get("failure_class", ""),
                    })
                    continue
            except Exception:
                pass

        # 2. Text line fallback
        m = text_pattern.search(line)
        if m:
            out.append({
                "msg": m.group("msg"),
                "request_id": m.group("request_id"),
                "node_id": m.group("node_id"),
                "stage": m.group("stage"),
                "failure_class": m.group("failure_class"),
            })

    return out


def execute_with_runcommand_fallback(session_runner, runcommand_runner):
    """
    Executes a command via Session Manager first, cleanly falling back to
    RunCommand if Session Manager encounters stream or pty errors.
    """
    try:
        return session_runner()
    except Exception as e:
        err_msg = str(e)
        if any(marker in err_msg for marker in ("Standard_Stream", "session", "pty", "channel closed")) or isinstance(e, RuntimeError):
            return runcommand_runner()
        raise


def correlate_failures(client_failures, gateway_records, node_status=None):
    """
    Joins observed client errors with collected gateway diagnostic records.

    client_failures: list of dicts with at least 'request_id' and 'http_status'.
    gateway_records: dict or list of gateway log dicts with 'request_id'.
    node_status: optional dict mapping node_alias to collection status ('ok', 'error', 'empty').

    Returns a list of classified error reports. If gateway logs are missing or
    incomplete, unjoined errors are marked 'uncollected_evidence' rather than
    falsely attributing them to upstream ALB without proof.
    """
    if isinstance(gateway_records, list):
        gw_map = {}
        for r in gateway_records:
            rid = r.get("request_id")
            if rid and rid not in gw_map:
                gw_map[rid] = r
    else:
        gw_map = gateway_records or {}

    collection_complete = True
    if node_status:
        for alias, status in node_status.items():
            if status not in ("ok", "empty"):
                collection_complete = False
                break

    correlated = []
    for cf in client_failures:
        rid = cf.get("request_id")
        status = cf.get("http_status")
        if rid in gw_map:
            rec = gw_map[rid]
            correlated.append({
                "request_id": rid,
                "http_status": status,
                "source": "gateway",
                "node_id": rec.get("node_id", ""),
                "stage": rec.get("stage", ""),
                "failure_class": rec.get("failure_class", ""),
                "msg": rec.get("msg", ""),
            })
        else:
            if collection_complete:
                correlated.append({
                    "request_id": rid,
                    "http_status": status,
                    "source": "upstream_no_gateway_request",
                })
            else:
                correlated.append({
                    "request_id": rid,
                    "http_status": status,
                    "source": "uncollected_evidence",
                    "note": "cannot attribute to upstream because node journal collection was incomplete",
                })

    return correlated


def sanitize_benchmark_failure(client_result):
    """
    Extracts sanitized failure details from warmbench client result without
    leaking payload bodies, authorization headers, or sensitive keys.
    Distinguishes pre-header, post-header stream abort, upstream and gateway errors.
    """
    if not client_result:
        return {"status": "no_data", "failures": []}

    failures = []

    def classify_failure(http_code, request_id, err_str, bytes_val):
        err_lower = (err_str or "").lower()
        if not http_code or http_code == 0 or "dial" in err_lower or "connection refused" in err_lower:
            return "pre_header_error"
        if http_code == 200 and (err_str or "eof" in err_lower or "short read" in err_lower or "stream abort" in err_lower):
            return "post_header_stream_abort"
        if request_id and http_code >= 400:
            return "gateway_error"
        if http_code in (502, 503, 504) and not request_id:
            return "upstream_error"
        if http_code >= 400:
            return "gateway_error"
        return "unknown_failure"

    # 1. Top-level samples list (warmbench primary format)
    for sample in client_result.get("samples", []):
        err = sample.get("error")
        http_code = sample.get("status_code") or sample.get("http_status")
        bytes_val = sample.get("payload_bytes", sample.get("bytes_read", 0))
        duration = sample.get("duration_ms")
        if duration is None and "latency_ns" in sample:
            duration = round(sample["latency_ns"] / 1e6, 2)
        elif duration is None:
            duration = 0

        if err or (http_code and http_code >= 400):
            rid = sample.get("request_id", "")
            ftype = classify_failure(http_code, rid, err, bytes_val)
            record = {
                "phase": sample.get("phase", "unknown"),
                "operation": sample.get("operation", ""),
                "request_id": rid,
                "http_status": http_code,
                "bytes_read": bytes_val,
                "duration_ms": duration,
                "failure_type": ftype,
            }
            if err:
                record["error"] = str(err)[:200]
            failures.append(record)

    # 2. Phase-nested samples (fallback / mock test format)
    phases = client_result.get("phases", {})
    if isinstance(phases, dict):
        for phase_name, phase_data in phases.items():
            if isinstance(phase_data, dict):
                for sample in phase_data.get("samples", []):
                    err = sample.get("error")
                    http_code = sample.get("status_code") or sample.get("http_status")
                    bytes_val = sample.get("payload_bytes", sample.get("bytes_read", 0))
                    duration = sample.get("duration_ms")
                    if duration is None and "latency_ns" in sample:
                        duration = round(sample["latency_ns"] / 1e6, 2)
                    elif duration is None:
                        duration = 0

                    if err or (http_code and http_code >= 400):
                        rid = sample.get("request_id", "")
                        ftype = classify_failure(http_code, rid, err, bytes_val)
                        record = {
                            "phase": phase_name,
                            "operation": sample.get("operation", ""),
                            "request_id": rid,
                            "http_status": http_code,
                            "bytes_read": bytes_val,
                            "duration_ms": duration,
                            "failure_type": ftype,
                        }
                        if err:
                            record["error"] = str(err)[:200]
                        failures.append(record)

    return {
        "status": client_result.get("status", "unknown"),
        "error": client_result.get("error"),
        "failures": failures,
    }


def parse_chrony_tracking(raw_text, now_unix=None, elapsed_seconds=0.0):
    """
    Parses chronyc tracking output in CSV format (-c) or key-value text format.
    Extracts bounded clock signals including stratum, offsets, interval, sample age,
    frequency drift, and validates clock uncertainty bounds matching coordination.Chrony invariant:
    - Finite numeric checks (rejects NaN and Inf)
    - Stratum in [1, 15] integer
    - Leap status == "Normal"
    - Age in [-2, min(1024, 2*interval+2)]
    - interval > 0, delay >= 0, dispersion >= 0, skew >= 0, abs(offset) <= 2
    - driftPPM = abs(freq) + abs(residual) + skew + 1 <= 500
    - errorSeconds + driftPPM * 1e-6 * runtimeHorizon <= 2 (horizon = 45 minutes)
    """
    import math

    if not raw_text:
        return {"status": "not_collected"}

    lines = [line.strip() for line in raw_text.splitlines() if line.strip()]
    if not lines:
        return {"status": "empty"}

    runtime_horizon_seconds = 45.0 * 60.0  # 45 minutes matching coordination.runtimeHorizon

    # Try CSV format first (e.g. chronyc -n -c tracking)
    for line in lines:
        parts = [p.strip() for p in line.split(",")]
        if len(parts) >= 14:
            ref_id = parts[0]
            ref_ip = parts[1]
            leap_status = parts[13]

            # Parse and validate all 11 numeric fields
            numbers = []
            has_non_finite = False
            for p in parts[2:13]:
                try:
                    val = float(p)
                    if not math.isfinite(val):
                        has_non_finite = True
                    numbers.append(val)
                except ValueError:
                    has_non_finite = True
                    numbers.append(float("nan"))

            if has_non_finite or len(numbers) != 11:
                return {
                    "status": "ok",
                    "reference_id": ref_id,
                    "reference_ip": ref_ip,
                    "leap_status": leap_status,
                    "uncertainty_exceeded": True,
                    "rejection_reason": "non_finite_or_unparseable_metrics",
                }

            stratum = numbers[0]
            ref_time = numbers[1]
            offset = numbers[2]
            # numbers[3] is last offset
            rms_offset = numbers[4]
            freq_ppm = numbers[5]
            residual_ppm = numbers[6]
            skew_ppm = numbers[7]
            root_delay = numbers[8]
            root_dispersion = numbers[9]
            interval = numbers[10]

            age = (now_unix - ref_time) if now_unix is not None else None
            max_allowed_age = min(1024.0, 2.0 * interval + 2.0) if interval > 0 else 0.0

            # Coordination invariants:
            # 1. Stratum integer between 1 and 15
            # 2. Leap status Normal
            # 3. Interval positive, non-negative delay, dispersion, skew
            # 4. Offset within 2 seconds
            # 5. Age bound
            # 6. Drift bound <= 500 PPM
            # 7. Horizon uncertainty bound <= 2.0 seconds
            drift_ppm = abs(freq_ppm) + abs(residual_ppm) + skew_ppm + 1.0
            error_seconds = abs(offset) + root_dispersion + root_delay / 2.0 + max(0.0, elapsed_seconds) + 0.01
            horizon_uncertainty = error_seconds + drift_ppm * 1e-6 * runtime_horizon_seconds

            uncertainty_exceeded = False
            rejection_reason = None

            if stratum < 1 or stratum > 15 or not stratum.is_integer():
                uncertainty_exceeded = True
                rejection_reason = "invalid_stratum"
            elif leap_status != "Normal":
                uncertainty_exceeded = True
                rejection_reason = "unsynchronized_leap_status"
            elif interval <= 0 or root_delay < 0 or root_dispersion < 0 or skew_ppm < 0:
                uncertainty_exceeded = True
                rejection_reason = "negative_or_invalid_interval_metrics"
            elif abs(offset) > 2.0:
                uncertainty_exceeded = True
                rejection_reason = "offset_exceeded_2s"
            elif age is not None and (age < -2.0 or age > max_allowed_age):
                uncertainty_exceeded = True
                rejection_reason = "sample_age_exceeded"
            elif drift_ppm > 500.0:
                uncertainty_exceeded = True
                rejection_reason = "drift_ppm_exceeded_500"
            elif horizon_uncertainty > 2.0:
                uncertainty_exceeded = True
                rejection_reason = "horizon_uncertainty_exceeded_2s"

            return {
                "status": "ok",
                "reference_id": ref_id,
                "reference_ip": ref_ip,
                "stratum": int(stratum),
                "ref_time": ref_time,
                "offset_seconds": offset,
                "rms_offset_seconds": rms_offset,
                "freq_ppm": freq_ppm,
                "residual_ppm": residual_ppm,
                "skew_ppm": skew_ppm,
                "drift_ppm": round(drift_ppm, 3),
                "root_delay": root_delay,
                "root_dispersion": root_dispersion,
                "interval_seconds": interval,
                "leap_status": leap_status,
                "sample_age_seconds": round(age, 4) if age is not None else None,
                "max_allowed_age_seconds": round(max_allowed_age, 2),
                "horizon_uncertainty_seconds": round(horizon_uncertainty, 4),
                "uncertainty_exceeded": uncertainty_exceeded,
                "rejection_reason": rejection_reason,
            }

    # Fallback to key-value text format (e.g. chronyc tracking)
    data = {}
    for line in lines:
        if ":" in line:
            k, v = line.split(":", 1)
            data[k.strip()] = v.strip()

    if "Reference ID" in data and "Stratum" in data:
        try:
            stratum = int(data.get("Stratum", "0"))
            leap_status = data.get("Leap status", "Unknown")
            return {
                "status": "ok",
                "reference_id": data.get("Reference ID", ""),
                "stratum": stratum,
                "leap_status": leap_status,
                "uncertainty_exceeded": (stratum < 1 or stratum > 15 or leap_status != "Normal"),
            }
        except ValueError:
            pass

    return {"status": "unrecognized_format"}


def build_node_telemetry_snapshot(
    alias,
    profile,
    metrics_raw=None,
    chrony_raw=None,
    readyz_status=None,
    error=None,
    captured_at=None,
    now_unix=None
):
    """
    Constructs a safe, non-sensitive telemetry snapshot for an individual gateway or edge node.
    """
    if error:
        return {
            "alias": alias,
            "profile": profile,
            "status": "error",
            "error": str(error),
            "captured_at": captured_at,
        }

    metrics = parse_prometheus_metrics(metrics_raw) if metrics_raw else {}
    clock_sig = parse_chrony_tracking(chrony_raw, now_unix=now_unix) if chrony_raw else {"status": "not_collected"}

    status = "ok" if (metrics or clock_sig.get("status") == "ok") else "empty"

    return {
        "alias": alias,
        "profile": profile,
        "status": status,
        "captured_at": captured_at,
        "readyz": readyz_status,
        "metrics": metrics,
        "clock": clock_sig,
    }


def parse_prometheus_metrics(raw_metrics_text, max_metrics=50):
    """
    Parses a Prometheus exposition text block into a dictionary of metric names
    to numeric values. Bounds total metrics parsed to prevent unbounded growth.
    """
    if not raw_metrics_text:
        return {}

    metrics = {}
    for raw_line in raw_metrics_text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue

        parts = line.split()
        if len(parts) != 2:
            continue

        metric_token, val_str = parts[0], parts[1]
        try:
            val = float(val_str)
            if val.is_integer():
                val = int(val)
        except ValueError:
            continue

        metrics[metric_token] = val
        if len(metrics) >= max_metrics:
            break

    return metrics


def build_failed_window_payload(
    name,
    captured_at,
    client_result,
    node_journals,
    node_status,
    before_metrics=None,
    after_metrics=None,
    release_hash=None,
    utc_window=None,
    schema_version=1
):
    """
    Constructs the structured failed-window diagnostic artifact, including
    sanitized benchmark failures, per-node log summaries, correlated failures,
    release provenance, UTC window, and before/after telemetry snapshots.
    """
    sanitized = sanitize_benchmark_failure(client_result)

    all_gw_records = []
    for nj in node_journals:
        lines_text = "\n".join(nj.get("lines", []))
        parsed = parse_gateway_journal_lines(lines_text)
        all_gw_records.extend(parsed)

    correlated = correlate_failures(
        sanitized.get("failures", []),
        all_gw_records,
        node_status=node_status
    )

    effective_utc_window = utc_window
    if not effective_utc_window:
        before_at = None
        if isinstance(before_metrics, dict):
            before_at = before_metrics.get("captured_at")
        effective_utc_window = {
            "before_at": before_at,
            "after_at": captured_at
        }

    return {
        "name": name,
        "schema_version": schema_version,
        "release_hash": release_hash or "unspecified",
        "captured_at": captured_at,
        "utc_window": effective_utc_window,
        "sanitized_summary": sanitized,
        "node_journals": node_journals,
        "node_status": node_status,
        "correlated_failures": correlated,
        "telemetry": {
            "before": before_metrics if before_metrics else {"status": "not_collected"},
            "after": after_metrics if after_metrics else {"status": "not_collected"}
        }
    }
