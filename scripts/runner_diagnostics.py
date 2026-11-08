"""
Safe runner diagnostics, journal parsing, and failure correlation utilities.

Contains no AWS credentials, private ARNs, or secret material. Usable by
both local unit/integration tests and operator test runners.
"""
import datetime
import json
import math
import re
import time


SENSITIVE_PATTERNS = [
    # Full URLs & S3 URIs (removes all hosts, object paths, keys, and queries)
    (re.compile(r'(?i)https?://[^\s"\'<>]+'), r'[REDACTED_URL]'),
    (re.compile(r'(?i)s3://[^\s"\'<>]+'), r'[REDACTED_S3_URI]'),
    # Signed URL query components & auth params (if unattached to full scheme)
    (re.compile(r'(?i)(X-Amz-Signature=)[^&\s"\'<>]+'), r'\1[REDACTED]'),
    (re.compile(r'(?i)(X-Amz-Credential=)[^&\s"\'<>]+'), r'\1[REDACTED]'),
    (re.compile(r'(?i)(X-Amz-Security-Token=)[^&\s"\'<>]+'), r'\1[REDACTED]'),
    (re.compile(r'(?i)(X-Amz-Date=)[^&\s"\'<>]+'), r'\1[REDACTED]'),
    (re.compile(r'(?i)(\bSignature=)[^&\s"\'<>]+'), r'\1[REDACTED]'),
    (re.compile(r'(?i)(AWSAccessKeyId=)[^&\s"\'<>]+'), r'\1[REDACTED]'),
    (re.compile(r'(?i)(AWSSecretAccessKey=)[^&\s"\'<>]+'), r'\1[REDACTED]'),
    # Authorization header (matches Bearer, SigV4, Basic, or opaque tokens without scheme)
    (re.compile(r'(?i)\bAuthorization\s*[:=]\s*(?:(?:Bearer|AWS4-HMAC-SHA256|Basic)\s+)?[^\s,"\'<>]+'), r'Authorization: [REDACTED]'),
    (re.compile(r'(?i)\bBearer\s+[^\s,"\'<>]+'), r'Bearer [REDACTED]'),
    # Raw AWS Access Key IDs (AKIA, ASIA, AROA, AIPA)
    (re.compile(r'(?<![A-Z0-9])(?:AKIA|ASIA|AROA|AIPA)[A-Z0-9]{16}(?![A-Z0-9])'), r'[REDACTED_AWS_KEY]'),
    # Generic secret/password/token/key/credential key-values
    (re.compile(r'(?i)\b([a-z0-9_-]*(?:password|secret|token|key|credential|signature|auth))\s*[:=]\s*["\']?[^"\'\s,;]+["\']?'), r'\1=[REDACTED]'),
    # Concrete AWS account IDs (12 digits) in ARNs or plain text
    (re.compile(r'arn:aws[a-z0-9-]*:[a-z0-9-]+:[a-z0-9-]*:(\d{12}):'), r'arn:aws:ap-southeast-1:123456789012:'),
    # Concrete EC2 instance IDs (i- followed by 8 or 17 hex characters)
    (re.compile(r'\bi-[0-9a-f]{8,17}\b'), r'[REDACTED_INSTANCE_ID]'),
]


def redact_sensitive_strings(text):
    """
    Deterministically scrubs AWS signatures, signed URLs, credentials, secret tokens,
    authorization headers, and concrete instance IDs from text strings.
    """
    if not isinstance(text, str):
        return text
    for pattern, repl in SENSITIVE_PATTERNS:
        text = pattern.sub(repl, text)
    return text


def redact_sensitive_payload(obj):
    """
    Recursively scrubs sensitive strings across arbitrary dicts, lists, and strings.
    """
    if isinstance(obj, str):
        return redact_sensitive_strings(obj)
    if isinstance(obj, list):
        return [redact_sensitive_payload(x) for x in obj]
    if isinstance(obj, dict):
        return {k: redact_sensitive_payload(v) for k, v in obj.items()}
    return obj


ALLOWLISTED_ERROR_CODES = {
    "clock_uncertainty_exceeded",
    "backend_unavailable",
    "service_unavailable",
    "internal_error",
    "stream_abort",
    "connection_refused",
    "connection_timeout",
    "dial_error",
    "unexpected_eof",
    "short_read",
    "slow_down",
    "resource_busy",
    "context_canceled",
    "deadline_exceeded",
    "request_timeout",
    "bad_request",
    "unauthorized",
    "forbidden",
    "not_found",
    "conflict",
    "precondition_failed",
    "pre_header_error",
    "post_header_stream_abort",
    "gateway_error",
    "upstream_error",
    "workload_failure",
    "unclassified_error",
}


def classify_error_to_code(err_str, http_status=None):
    """
    Maps free-form error strings or HTTP status codes into strict allowlisted error codes.
    """
    if not err_str:
        if http_status == 500:
            return "internal_error"
        if http_status == 503:
            return "service_unavailable"
        if http_status == 504:
            return "request_timeout"
        return "none" if (http_status and http_status < 400) else "unclassified_error"

    s = str(err_str).lower()
    if "clock" in s or "uncertain" in s:
        return "clock_uncertainty_exceeded"
    if "stream" in s and ("abort" in s or "reset" in s):
        return "stream_abort"
    if "eof" in s or "unexpected eof" in s or "short read" in s:
        return "unexpected_eof"
    if "connection refused" in s:
        return "connection_refused"
    if "timeout" in s or "deadline" in s:
        return "connection_timeout"
    if "dial" in s:
        return "dial_error"
    if "backend" in s and "unavailable" in s:
        return "backend_unavailable"
    if "service unavailable" in s or "slowdown" in s:
        return "service_unavailable"
    if "internal" in s:
        return "internal_error"
    if "canceled" in s or "cancelled" in s:
        return "context_canceled"
    if http_status == 500:
        return "internal_error"
    if http_status == 503:
        return "service_unavailable"
    if http_status == 504:
        return "request_timeout"
    return "unclassified_error"



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


def correlate_failures(client_failures, gateway_records, node_status=None, alb_evidence=None):
    """
    Joins observed client errors with collected gateway diagnostic records.

    client_failures: list of dicts with at least 'request_id' and 'http_status'.
    gateway_records: dict or list of gateway log dicts with 'request_id'.
    node_status: optional dict mapping node_alias to collection status ('ok', 'error', 'empty').
    alb_evidence: optional dict or list of corroborated ALB access logs, target status records,
                  or fixture assertions confirming upstream error.

    Returns a list of classified error reports. If gateway logs are missing or
    incomplete, unjoined errors are marked 'origin_unverified' / 'gateway_log_missing'
    unless corroborated by native ALB evidence. Never attributes HTTP 500 to upstream.
    """
    if isinstance(gateway_records, list):
        gw_map = {}
        for r in gateway_records:
            rid = r.get("request_id")
            if rid and rid not in gw_map:
                gw_map[rid] = r
    else:
        gw_map = gateway_records or {}

    alb_map = {}
    if isinstance(alb_evidence, list):
        for a in alb_evidence:
            arid = a.get("request_id")
            if arid and arid not in alb_map:
                alb_map[arid] = a
    elif isinstance(alb_evidence, dict):
        alb_map = alb_evidence

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
                "diagnostic_status": "correlated_gateway_log",
            })
        else:
            alb_rec = alb_map.get(rid)
            is_pre_target_alb = False
            if alb_rec:
                t_status = str(alb_rec.get("target_status_code", "-")).strip()
                target_did_not_respond = t_status in ("-", "", "0", "None")
                upstream_flag = alb_rec.get("upstream")

                if upstream_flag is True:
                    is_pre_target_alb = True
                elif upstream_flag is False:
                    is_pre_target_alb = False
                elif target_did_not_respond and alb_rec.get("elb_status_code") in (502, 503, 504):
                    is_pre_target_alb = True

            if is_pre_target_alb:
                correlated.append({
                    "request_id": rid,
                    "http_status": status,
                    "source": "upstream_alb",
                    "diagnostic_status": "corroborated_alb_evidence",
                    "alb_status": alb_rec.get("elb_status_code", status),
                    "target_status": alb_rec.get("target_status_code", "-"),
                    "note": "corroborated by ALB access evidence; request terminated at ALB before target",
                })
            elif alb_rec and not is_pre_target_alb:
                # ALB log indicates target responded or was contacted, but gateway log is missing
                correlated.append({
                    "request_id": rid,
                    "http_status": status,
                    "source": "origin_unverified",
                    "diagnostic_status": "gateway_log_missing",
                    "alb_status": alb_rec.get("elb_status_code", status),
                    "target_status": alb_rec.get("target_status_code", "-"),
                    "note": "ALB log indicates target responded or was contacted, but gateway log is missing",
                })
            elif status == 500:
                # ALB does NOT generate HTTP 500 on its own for target errors
                correlated.append({
                    "request_id": rid,
                    "http_status": status,
                    "source": "origin_unverified",
                    "diagnostic_status": "gateway_log_missing",
                    "note": "unmatched HTTP 500 without gateway log record; ALB does not generate 500 on its own",
                })
            else:
                if collection_complete:
                    correlated.append({
                        "request_id": rid,
                        "http_status": status,
                        "source": "origin_unverified",
                        "diagnostic_status": "gateway_log_missing",
                        "note": "unmatched client failure without corroborating native ALB access log; origin unverified",
                    })
                else:
                    correlated.append({
                        "request_id": rid,
                        "http_status": status,
                        "source": "uncollected_evidence",
                        "diagnostic_status": "incomplete_journal_collection",
                        "note": "cannot attribute origin because node journal collection was incomplete",
                    })

    return correlated


SAFE_ERROR_NAMES = {
    "InternalError": "InternalError",
    "Service Unavailable": "Service Unavailable",
    "ServiceUnavailable": "ServiceUnavailable",
    "RequestTimeout": "RequestTimeout",
    "SlowDown": "SlowDown",
    "unexpected EOF in data stream": "unexpected EOF in data stream",
    "connection refused": "connection refused",
    "context deadline exceeded": "context deadline exceeded",
}


def sanitize_benchmark_failure(client_result):
    """
    Extracts sanitized failure details from warmbench client result without
    leaking payload bodies, authorization headers, signed URLs, or sensitive keys.
    Distinguishes pre-header, post-header stream abort, upstream and gateway errors.
    Deterministic redaction and allowlisted error codes are enforced.
    """
    if not client_result:
        return {"status": "no_data", "error": None, "error_code": None, "failures": []}

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

    def process_sample(sample, phase_default="unknown"):
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
            ecode = classify_error_to_code(err, http_code)
            record = {
                "phase": str(sample.get("phase", phase_default)),
                "operation": str(sample.get("operation", "")),
                "request_id": str(rid),
                "http_status": http_code,
                "bytes_read": bytes_val,
                "duration_ms": duration,
                "failure_type": ftype,
                "error_code": ecode,
            }
            if err:
                cleaned_err = str(err).strip()
                if cleaned_err in SAFE_ERROR_NAMES:
                    record["error"] = SAFE_ERROR_NAMES[cleaned_err]
                else:
                    record["error"] = ecode
            return record
        return None

    # 1. Top-level samples list (warmbench primary format)
    for sample in client_result.get("samples", []):
        rec = process_sample(sample)
        if rec:
            failures.append(rec)

    # 2. Phase-nested samples (fallback / mock test format)
    phases = client_result.get("phases", {})
    if isinstance(phases, dict):
        for phase_name, phase_data in phases.items():
            if isinstance(phase_data, dict):
                for sample in phase_data.get("samples", []):
                    rec = process_sample(sample, phase_default=phase_name)
                    if rec:
                        failures.append(rec)

    raw_top_err = client_result.get("error")
    top_err_code = classify_error_to_code(raw_top_err) if raw_top_err is not None else None
    top_err = top_err_code if raw_top_err is not None else None

    return {
        "status": str(client_result.get("status", "unknown")),
        "error": top_err,
        "error_code": top_err_code,
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

            now_is_finite = isinstance(now_unix, (int, float)) and math.isfinite(now_unix)
            elapsed_is_valid = isinstance(elapsed_seconds, (int, float)) and math.isfinite(elapsed_seconds) and elapsed_seconds >= 0

            age = (float(now_unix) - ref_time) if now_is_finite else None
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
            elapsed_val = max(0.0, float(elapsed_seconds)) if elapsed_is_valid else float("nan")
            error_seconds = abs(offset) + root_dispersion + root_delay / 2.0 + elapsed_val + 0.01
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
            elif now_unix is None:
                uncertainty_exceeded = True
                rejection_reason = "missing_current_timestamp_for_age_verification"
            elif not now_is_finite:
                uncertainty_exceeded = True
                rejection_reason = "non_finite_current_timestamp"
            elif not elapsed_is_valid:
                uncertainty_exceeded = True
                rejection_reason = "invalid_or_non_finite_elapsed_seconds"
            elif age is None or not math.isfinite(age) or age < -2.0 or age > max_allowed_age:
                uncertainty_exceeded = True
                rejection_reason = "sample_age_exceeded"
            elif not math.isfinite(drift_ppm) or drift_ppm > 500.0:
                uncertainty_exceeded = True
                rejection_reason = "drift_ppm_exceeded_500"
            elif not math.isfinite(horizon_uncertainty) or horizon_uncertainty > 2.0:
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
    import datetime

    def _extract_float(val_str):
        if not val_str:
            return None
        parts = val_str.split()
        if not parts:
            return None
        try:
            val = float(parts[0])
            return val if math.isfinite(val) else None
        except ValueError:
            return None

    def _parse_ref_time(val_str):
        if not val_str:
            return None
        # Format e.g. "Fri Nov 06 14:32:00 2026" or ISO
        for fmt in ("%a %b %d %H:%M:%S %Y", "%a %b  %d %H:%M:%S %Y", "%Y-%m-%dT%H:%M:%SZ", "%Y-%m-%d %H:%M:%S"):
            try:
                dt = datetime.datetime.strptime(val_str.strip(), fmt).replace(tzinfo=datetime.timezone.utc)
                return dt.timestamp()
            except ValueError:
                continue
        return None

    data = {}
    for line in lines:
        if ":" in line:
            k, v = line.split(":", 1)
            data[k.strip()] = v.strip()

    if "Reference ID" in data or "Stratum" in data:
        ref_id_str = data.get("Reference ID", "")
        ref_id = ref_id_str.split()[0] if ref_id_str else ""
        leap_status = data.get("Leap status", "Unknown")

        try:
            stratum = int(data.get("Stratum", "0"))
        except ValueError:
            stratum = 0

        ref_time = _parse_ref_time(data.get("Ref time (UTC)", ""))
        interval = _extract_float(data.get("Update interval"))
        freq_ppm = _extract_float(data.get("Frequency"))
        residual_ppm = _extract_float(data.get("Residual freq"))
        skew_ppm = _extract_float(data.get("Skew"))
        root_delay = _extract_float(data.get("Root delay"))
        root_dispersion = _extract_float(data.get("Root dispersion"))
        last_offset = _extract_float(data.get("Last offset"))
        system_offset = _extract_float(data.get("System time"))
        offset = last_offset if last_offset is not None else system_offset

        has_all_metrics = (
            ref_time is not None
            and interval is not None
            and freq_ppm is not None
            and residual_ppm is not None
            and skew_ppm is not None
            and root_delay is not None
            and root_dispersion is not None
            and offset is not None
        )

        if not has_all_metrics:
            # Incomplete text format cannot be verified against coordination invariants;
            # fail-closed to avoid false healthy classification.
            return {
                "status": "unverified_text_format",
                "reference_id": ref_id,
                "stratum": stratum,
                "leap_status": leap_status,
                "uncertainty_exceeded": True,
                "rejection_reason": "unverified_text_format_missing_drift_or_age",
            }

        now_is_finite = isinstance(now_unix, (int, float)) and math.isfinite(now_unix)
        elapsed_is_valid = isinstance(elapsed_seconds, (int, float)) and math.isfinite(elapsed_seconds) and elapsed_seconds >= 0

        age = (float(now_unix) - ref_time) if now_is_finite else None
        max_allowed_age = min(1024.0, 2.0 * interval + 2.0) if interval > 0 else 0.0

        drift_ppm = abs(freq_ppm) + abs(residual_ppm) + skew_ppm + 1.0
        elapsed_val = max(0.0, float(elapsed_seconds)) if elapsed_is_valid else float("nan")
        error_seconds = abs(offset) + root_dispersion + root_delay / 2.0 + elapsed_val + 0.01
        horizon_uncertainty = error_seconds + drift_ppm * 1e-6 * runtime_horizon_seconds

        uncertainty_exceeded = False
        rejection_reason = None

        if stratum < 1 or stratum > 15:
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
        elif now_unix is None:
            uncertainty_exceeded = True
            rejection_reason = "missing_current_timestamp_for_age_verification"
        elif not now_is_finite:
            uncertainty_exceeded = True
            rejection_reason = "non_finite_current_timestamp"
        elif not elapsed_is_valid:
            uncertainty_exceeded = True
            rejection_reason = "invalid_or_non_finite_elapsed_seconds"
        elif age is None or not math.isfinite(age) or age < -2.0 or age > max_allowed_age:
            uncertainty_exceeded = True
            rejection_reason = "sample_age_exceeded"
        elif not math.isfinite(drift_ppm) or drift_ppm > 500.0:
            uncertainty_exceeded = True
            rejection_reason = "drift_ppm_exceeded_500"
        elif not math.isfinite(horizon_uncertainty) or horizon_uncertainty > 2.0:
            uncertainty_exceeded = True
            rejection_reason = "horizon_uncertainty_exceeded_2s"

        return {
            "status": "ok",
            "reference_id": ref_id,
            "stratum": stratum,
            "ref_time": ref_time,
            "offset_seconds": offset,
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

    return {"status": "unrecognized_format"}


def build_node_telemetry_snapshot(
    alias,
    profile,
    metrics_raw=None,
    chrony_raw=None,
    chrony_error=None,
    readyz_status=None,
    error=None,
    captured_at=None,
    now_unix=None,
    elapsed_seconds=0.0,
    probe_started_at=None,
    probe_finished_at=None,
    probe_duration_seconds=None,
):
    """
    Constructs a safe, non-sensitive telemetry snapshot for an individual gateway or edge node.
    Records probe start/finish timestamps, probe duration, and ensures clock sample evaluation
    is anchored to the actual probe completion window.
    """
    effective_captured_at = captured_at or probe_finished_at or probe_started_at
    if error:
        return {
            "alias": alias,
            "profile": profile,
            "status": "error",
            "error": redact_sensitive_strings(str(error)),
            "captured_at": effective_captured_at,
            "probe_started_at": probe_started_at,
            "probe_finished_at": probe_finished_at,
            "probe_duration_seconds": probe_duration_seconds,
        }

    if now_unix is None:
        target_ts = probe_finished_at or effective_captured_at
        if target_ts:
            try:
                dt = datetime.datetime.fromisoformat(target_ts.replace("Z", "+00:00"))
                now_unix = dt.timestamp()
            except Exception:
                pass

    effective_elapsed = elapsed_seconds
    if effective_elapsed <= 0.0 and probe_duration_seconds is not None and probe_duration_seconds > 0.0:
        effective_elapsed = probe_duration_seconds

    metrics = parse_prometheus_metrics(metrics_raw) if metrics_raw else {}
    if chrony_error:
        clock_sig = {"status": "error", "error": redact_sensitive_strings(str(chrony_error))}
    elif chrony_raw:
        clock_sig = parse_chrony_tracking(chrony_raw, now_unix=now_unix, elapsed_seconds=effective_elapsed)
    else:
        clock_sig = {"status": "not_collected", "missing_reason": "no_chrony_output"}

    has_metrics = bool(metrics)
    has_clock_ok = clock_sig.get("status") == "ok" and not clock_sig.get("uncertainty_exceeded", False)

    if has_metrics and has_clock_ok:
        status = "ok"
    elif has_metrics or has_clock_ok:
        status = "partial"
    else:
        status = "empty"

    return {
        "alias": alias,
        "profile": profile,
        "status": status,
        "captured_at": effective_captured_at,
        "probe_started_at": probe_started_at,
        "probe_finished_at": probe_finished_at,
        "probe_duration_seconds": probe_duration_seconds,
        "readyz": readyz_status,
        "metrics": metrics,
        "clock": clock_sig,
    }


def collect_node_telemetry_safe(nodes, fetcher_fn, captured_at=None, now_unix=None, elapsed_seconds=0.0):
    """
    Safely collects telemetry across a list of nodes, handling individual node timeouts,
    connection failures, or collection errors gracefully without aborting the cluster capture.
    Per-probe timestamps and durations are captured per node to ensure sequential execution
    latencies do not distort clock sample age calculations.
    Returns a structured dictionary with per-node snapshots, overall status, and missing reasons.
    """
    collection_started_at = datetime.datetime.now(datetime.timezone.utc).isoformat()
    if not nodes:
        return {
            "status": "not_collected",
            "missing_reason": "no_nodes_specified",
            "captured_at": captured_at or collection_started_at,
            "collection_started_at": collection_started_at,
            "collection_finished_at": collection_started_at,
            "nodes": {},
        }

    effective_captured_at = captured_at or collection_started_at
    node_snapshots = {}
    has_ok = False
    has_partial = False
    has_error = False

    for node in nodes:
        alias = node.get("alias", "unknown")
        profile = node.get("profile", "cloud")
        t_start = datetime.datetime.now(datetime.timezone.utc)
        t_mono_start = time.monotonic()
        try:
            res = fetcher_fn(node)
            t_finish = datetime.datetime.now(datetime.timezone.utc)
            t_mono_finish = time.monotonic()
            probe_duration = round(t_mono_finish - t_mono_start, 4)

            metrics_raw = res.get("metrics_raw")
            chrony_raw = res.get("chrony_raw")
            chrony_error = res.get("chrony_error")
            readyz = res.get("readyz_status")

            node_probe_started = res.get("probe_started_at") or t_start.isoformat()
            node_probe_finished = res.get("probe_finished_at") or t_finish.isoformat()
            node_duration = res.get("probe_duration_seconds") if res.get("probe_duration_seconds") is not None else probe_duration

            if now_unix is not None:
                node_now_unix = now_unix
            elif "now_unix" in res and res["now_unix"] is not None:
                node_now_unix = res["now_unix"]
            else:
                node_now_unix = t_finish.timestamp()

            if elapsed_seconds > 0.0:
                node_elapsed = elapsed_seconds
            elif "chrony_elapsed_seconds" in res and res["chrony_elapsed_seconds"] is not None:
                node_elapsed = res["chrony_elapsed_seconds"]
            else:
                node_elapsed = node_duration

            snap = build_node_telemetry_snapshot(
                alias=alias,
                profile=profile,
                metrics_raw=metrics_raw,
                chrony_raw=chrony_raw,
                chrony_error=chrony_error,
                readyz_status=readyz,
                captured_at=node_probe_finished,
                now_unix=node_now_unix,
                elapsed_seconds=node_elapsed,
                probe_started_at=node_probe_started,
                probe_finished_at=node_probe_finished,
                probe_duration_seconds=node_duration,
            )
            node_snapshots[alias] = snap
            snap_status = snap.get("status")
            if snap_status == "ok":
                has_ok = True
            elif snap_status == "partial":
                has_partial = True
            else:
                has_error = True
        except Exception as e:
            t_finish = datetime.datetime.now(datetime.timezone.utc)
            t_mono_finish = time.monotonic()
            probe_duration = round(t_mono_finish - t_mono_start, 4)
            has_error = True
            node_snapshots[alias] = build_node_telemetry_snapshot(
                alias=alias,
                profile=profile,
                error=redact_sensitive_strings(str(e)),
                captured_at=t_finish.isoformat(),
                probe_started_at=t_start.isoformat(),
                probe_finished_at=t_finish.isoformat(),
                probe_duration_seconds=probe_duration,
            )

    collection_finished_at = datetime.datetime.now(datetime.timezone.utc).isoformat()

    if has_ok and not has_error and not has_partial:
        cluster_status = "ok"
    elif has_ok or has_partial:
        cluster_status = "partial"
    else:
        cluster_status = "error"

    return {
        "status": cluster_status,
        "captured_at": effective_captured_at,
        "collection_started_at": collection_started_at,
        "collection_finished_at": collection_finished_at,
        "nodes": node_snapshots,
        "summary": {
            "total_nodes": len(nodes),
            "healthy_nodes": sum(1 for s in node_snapshots.values() if s.get("status") == "ok"),
            "partial_nodes": sum(1 for s in node_snapshots.values() if s.get("status") == "partial"),
            "failed_nodes": sum(1 for s in node_snapshots.values() if s.get("status") == "error"),
        }
    }


def collect_node_journals_safe(nodes, journal_fetcher_fn):
    """
    Safely captures systemd/journalctl logs across nodes, capturing error metadata
    per-node on failure so that log collection failure on one node does not lose logs
    from other active nodes. Enforces deterministic string redaction on every line.
    Returns (node_journals_list, node_status_dict).
    """
    node_journals = []
    node_status = {}

    for node in nodes:
        alias = node.get("alias", "unknown")
        report = {"alias": alias}
        if "id" in node:
            report["id"] = redact_sensitive_strings(node["id"])
        try:
            res = journal_fetcher_fn(node)
            raw_lines = res.get("lines", [])
            scrubbed_lines = [redact_sensitive_strings(line) for line in raw_lines]
            report["count"] = len(scrubbed_lines)
            report["lines"] = scrubbed_lines
            node_status[alias] = "ok" if scrubbed_lines else "empty"
        except Exception as e:
            report["count"] = 0
            report["lines"] = []
            report["error_type"] = type(e).__name__
            report["error"] = redact_sensitive_strings(str(e))
            node_status[alias] = "error"
        node_journals.append(report)

    return node_journals, node_status


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
    alb_evidence=None,
    schema_version=1
):
    """
    Constructs the structured failed-window diagnostic artifact, including
    sanitized benchmark failures, per-node log summaries, correlated failures,
    release provenance, UTC window, and before/after telemetry snapshots.
    Deterministic redaction is applied recursively across all fields.
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
        node_status=node_status,
        alb_evidence=alb_evidence
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

    collection_complete = all(s in ("ok", "empty") for s in node_status.values()) if node_status else False

    payload = {
        "name": name,
        "schema_version": schema_version,
        "release_hash": release_hash or "unspecified",
        "captured_at": captured_at,
        "utc_window": effective_utc_window,
        "sanitized_summary": sanitized,
        "node_journals": node_journals,
        "node_status": node_status,
        "log_coverage": {
            "nodes_polled": len(node_status) if node_status else 0,
            "collection_complete": collection_complete,
            "statuses": dict(node_status) if node_status else {}
        },
        "correlated_failures": correlated,
        "telemetry": {
            "before": before_metrics if before_metrics is not None else {"status": "not_collected", "missing_reason": "before_metrics_not_provided"},
            "after": after_metrics if after_metrics is not None else {"status": "not_collected", "missing_reason": "after_metrics_not_provided"}
        }
    }

    return redact_sensitive_payload(payload)
