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
    """
    if not client_result:
        return {"status": "no_data", "failures": []}

    failures = []
    phases = client_result.get("phases", {})
    for phase_name, phase_data in phases.items():
        if isinstance(phase_data, dict):
            for sample in phase_data.get("samples", []):
                err = sample.get("error")
                http_code = sample.get("status_code") or sample.get("http_status")
                if err or (http_code and http_code >= 400):
                    failures.append({
                        "phase": phase_name,
                        "request_id": sample.get("request_id", ""),
                        "http_status": http_code,
                        "bytes_read": sample.get("bytes_read", 0),
                        "duration_ms": sample.get("duration_ms", 0),
                    })

    return {
        "status": client_result.get("status", "unknown"),
        "error": client_result.get("error"),
        "failures": failures,
    }
