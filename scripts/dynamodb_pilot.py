#!/usr/bin/env python3
"""Combine matched Warm runs and runtime observations, without AWS mutations."""
import argparse
import datetime as dt
import json
import math
import os
from pathlib import Path
import re


COUNTERS = (
    "miles_ddb_calls_total", "miles_ddb_transactions_total", "miles_ddb_consumed_units_total",
    "miles_ddb_read_units_total", "miles_ddb_write_units_total", "miles_ddb_capacity_responses_total",
    "miles_ddb_lease_write_attempts_total", "miles_ddb_retries_total", "miles_ddb_throttles_total",
    "miles_metadata_request_body_bytes_total", "miles_metadata_response_body_bytes_total",
    "miles_gateway_s3_request_body_bytes_total", "miles_gateway_s3_response_body_bytes_total",
    "miles_client_gateway_body_bytes_total", "miles_gateway_client_body_bytes_total",
    "miles_warm_read_requests_total", "miles_warm_read_integrity_failures_total",
    "miles_warm_read_cache_hits_total", "miles_warm_read_cache_misses_total",
    "miles_runtime_observation_failures_total",
)
GAUGES = (
    "miles_recovery_cut_age_seconds", "miles_r1_gate_closed_seconds",
    "miles_stuck_completing_uploads", "miles_journal_export_lag_seconds",
    "miles_runtime_observation_age_seconds",
)


def number(value):
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not math.isfinite(value) or value < 0:
        raise ValueError("invalid nonnegative observation")
    return value


def timestamp(text):
    value = dt.datetime.fromisoformat(text.replace("Z", "+00:00"))
    if value.tzinfo is None:
        raise ValueError("timestamp requires timezone")
    return value


def metrics(text):
    result = {}
    for line in text.splitlines():
        match = re.fullmatch(r"([a-z_][a-z0-9_]*) ([0-9.eE+-]+)", line)
        if not match:
            continue
        name, value = match.groups()
        if name in COUNTERS + GAUGES + ("miles_process_start_time_seconds",):
            if name in result:
                raise ValueError("duplicate process metric")
            result[name] = number(float(value))
    return result


def run_summary(run):
    if run["schema_version"] != 1 or run["status"] != "passed" or run["mvp_threshold_applied"]:
        raise ValueError("require successful preliminary Warm run")
    start, end = timestamp(run["started_at"]), timestamp(run["finished_at"])
    if end <= start or not re.fullmatch(r"[0-9a-f]{64}", run["manifest_sha256"]):
        raise ValueError("invalid run identity/window")
    phases = {p["name"]: p for p in run["phases"]}
    if len(phases) != len(run["phases"]):
        raise ValueError("duplicate workload phase")
    out = {}
    for name, phase in phases.items():
        if phase["failed"] or phase["samples"] < 1 or phase["elapsed_ns"] <= 0:
            raise ValueError("failed/empty phase")
        out[name] = {k: number(phase[k]) for k in (
            "samples", "elapsed_ns", "successful_logical_bytes", "successful_logical_bytes_per_second", "p50_ns", "p95_ns", "max_ns")}
    for phase in ("put", "head", "verify-get", "list"):
        if phase not in out:
            raise ValueError("missing workload phase")
    if not run["samples"] or any(s.get("error") or (s["operation"] == "GetObject" and not re.fullmatch(r"[0-9a-f]{64}", s.get("sha256", ""))) for s in run["samples"]):
        raise ValueError("unverified/failed workload sample")
    routes = run["http_body_bytes"]
    expected = {"client_to_gateway", "gateway_to_client"} if run["target"] == "miles-warm" else {"client_to_s3", "s3_to_client"}
    if set(routes) != expected or any(number(v) <= 0 for v in routes.values()):
        raise ValueError("missing measured client HTTP body routes")
    return {"started_at": run["started_at"], "finished_at": run["finished_at"], "phases": out, "http_body_bytes": routes, "cleanup": run["cleanup"]}


def build_report(runs, observation):
    if set(runs) != {"cloud", "edge", "direct"}:
        raise ValueError("require Cloud, Edge and direct runs")
    summaries = {name: run_summary(run) for name, run in runs.items()}
    # Physical statistics require every matched workload to retain its owned prefix.
    if any(summary["cleanup"] != "retained_owned_prefix_for_observation" for summary in summaries.values()):
        raise ValueError("matched pilot statistics require retained owned prefixes in all three runs")
    direct = runs["direct"]
    emulated = direct["target"] == "local-s3-smoke"
    if direct["target"] not in ("direct-s3", "local-s3-smoke"):
        raise ValueError("direct comparator has wrong target")
    fields = ("manifest_sha256", "concurrency", "read_repeats", "region")
    for name in ("cloud", "edge"):
        run = runs[name]
        if run["target"] != "miles-warm" or run["declared_environment"]["profile"] != name:
            raise ValueError("wrong Cloud/Edge declaration")
        if run["declared_environment"]["metadata"] != ("dynamodb-local" if emulated else "dynamodb"):
            raise ValueError("wrong metadata backend for comparator")
        if any(run[k] != direct[k] for k in fields) or run["client"]["executable_sha256"] != direct["client"]["executable_sha256"]:
            raise ValueError("unmatched workload/toolchain")
        if set(summaries[name]["phases"]) != set(summaries["direct"]["phases"]):
            raise ValueError("unmatched workload phases/cleanup policy")
        for field in ("host", "cpu", "disk", "network", "encryption"):
            if run["declared_environment"][field] != direct["declared_environment"][field]:
                raise ValueError("unmatched declared client environment")
    if runs["cloud"]["declared_environment"]["gateway_commit"] != runs["edge"]["declared_environment"]["gateway_commit"] or runs["cloud"]["declared_environment"]["policy_revision"] != runs["edge"]["declared_environment"]["policy_revision"]:
        raise ValueError("Cloud/Edge release or policy mismatch")
    windows = {}
    for name in ("cloud", "edge"):
        window = observation[name]
        before, after = timestamp(window["before_at"]), timestamp(window["after_at"])
        if before > timestamp(runs[name]["started_at"]) or after < timestamp(runs[name]["finished_at"]):
            raise ValueError("telemetry window does not enclose workload")
        totals = {k: 0 for k in COUNTERS}
        peaks = {}
        aliases = set()
        profiles = []
        for node in window["nodes"]:
            if node["alias"] in aliases or node["profile"] not in ("cloud", "edge"):
                raise ValueError("duplicate node or invalid profile")
            aliases.add(node["alias"])
            profiles.append(node["profile"])
            a, b = metrics(node["before"]), metrics(node["after"])
            if "miles_process_start_time_seconds" not in a or a["miles_process_start_time_seconds"] != b.get("miles_process_start_time_seconds"):
                raise ValueError("node restarted: split observation window")
            for k in COUNTERS:
                if k not in a or k not in b or b[k] < a[k]:
                    raise ValueError("missing/reset counter")
                totals[k] += b[k] - a[k]
            if b.get("miles_runtime_observation_age_seconds", 60) >= 60:
                raise ValueError("stale runtime gauges")
            for k in GAUGES:
                if k not in b:
                    raise ValueError("missing runtime gauge")
                peaks[k] = max(peaks.get(k, 0), b[k])
        if profiles.count("cloud") < 2 or profiles.count("edge") != 1:
            raise ValueError("require both Cloud nodes and one Edge; ALB scrapes alone can miss nodes")
        if totals["miles_warm_read_integrity_failures_total"]:
            raise ValueError("integrity failure blocks acceptance")
        stats = window["statistics"]
        if not stats["physical_complete"] or timestamp(stats["collection_started_at"]) < timestamp(runs[name]["finished_at"]) or timestamp(stats["as_of"]) > after:
            raise ValueError("statistics must be a complete post-workload observation")
        logical, raw, stored, physical = (number(stats[k]) for k in ("bytes_logical", "bytes_unique_raw", "bytes_unique_stored", "bytes_stored"))
        if not logical >= raw >= stored or physical < stored or logical < runs[name]["dataset"]["total_bytes"]:
            raise ValueError("inconsistent retained live/physical statistics")
        ratio = lambda n, d: n / d if d else None
        windows[name] = {"before_at": window["before_at"], "after_at": window["after_at"], "node_count": len(aliases),
            "process_counter_deltas": totals, "max_observed_gauges": peaks,
            "retained_statistics": {"bytes_logical": logical, "bytes_unique_raw": raw, "bytes_unique_stored": stored, "bytes_physical": physical,
                "dedup_ratio": ratio(logical, raw), "compression_ratio": ratio(raw, stored), "total_reduction": ratio(logical, physical)}}
    return {"schema_version": 1, "status": "local_contract_only" if emulated else "preliminary_aws_measurement",
        "mvp_threshold_applied": False, "manifest_sha256": direct["manifest_sha256"],
        "concurrency": direct["concurrency"], "runs": summaries, "observations": windows,
        "put_throughput_vs_direct": {n: ratio(summaries[n]["phases"]["put"]["successful_logical_bytes_per_second"], summaries["direct"]["phases"]["put"]["successful_logical_bytes_per_second"]) for n in ("cloud", "edge")},
        "limitations": ["HTTP body bytes exclude headers/TLS/TCP and do not equal billed traffic.",
            "Counters include background R1/R2, statistics and probes across the declared window; capacity reports can omit failed/unknown requests. Cross-check native AWS/DynamoDB consumption.",
            "Gauges are sampled observations, not exact duration maxima; never sum shared cut ages across nodes.",
            "Statistics cover all retained live data and listed physical versions, not just this workload. Record isolation and baseline separately.",
            "Cloud Internet logical GET bytes remain fully chargeable in cost models regardless of cache hits; Edge on EC2 is a functional profile, not on-prem network pricing.",
            "PITR size/status, endpoint/NAT hours and processing bytes, alarm transitions and billed costs need separate AWS evidence; this report does not invent them."]}


def read_json(path):
    p = Path(path)
    if p.is_symlink() or not p.is_file() or p.stat().st_size > 32 << 20:
        raise ValueError("require bounded regular JSON input")
    return json.loads(p.read_text())


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("cloud", "edge", "direct", "observations", "out"):
        parser.add_argument("--" + name, required=True)
    args = parser.parse_args()
    try:
        report = build_report({n: read_json(getattr(args, n)) for n in ("cloud", "edge", "direct")}, read_json(args.observations))
        fd = os.open(args.out, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
        with os.fdopen(fd, "w") as out:
            json.dump(report, out, indent=2, allow_nan=False)
            out.write("\n")
    except (ValueError, KeyError, TypeError, OSError):
        parser.exit(1, "Invalid pilot inputs or output. Keep originals private and inspect against the runbook schema.\n")


if __name__ == "__main__":
    main()
