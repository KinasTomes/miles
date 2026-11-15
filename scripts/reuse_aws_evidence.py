"""Reconcile private historical warmbench results without AWS or leaking private data.

Reads only explicitly named local result/clock-summary files. The output contains
allowlisted measurements, never source JSON, objects, request IDs, URLs or secrets.
A successful record is *not* benchmark acceptance or a matched direct-S3 pair.
"""

import argparse
from collections import Counter
from datetime import datetime
import hashlib
import json
import math
import os
from pathlib import Path
import re
import stat
import sys

HEX64 = re.compile(r"[0-9a-f]{64}\Z")
HEX40 = re.compile(r"[0-9a-f]{40}\Z")
BASENAME = re.compile(r"[a-z0-9][a-z0-9-]*-(?:result|telemetry-summary)\.json\Z")
LABEL = re.compile(r"[a-z0-9][a-z0-9-]{0,63}\Z")
ROOTS = {"pr76-early", "pr76-mid", "pr76-late", "pr86"}
PHASES = ("preflight-list", "put", "head", "verify-get", "list",
          "repeat-get-1", "repeat-get-2", "repeat-get-3")
OPERATIONS = {"put": "PutObject", "head": "HeadObject", "verify-get": "GetObject",
              "list": "ListObjectsV2", "preflight-list": "ListObjectsV2",
              "repeat-get-1": "GetObject", "repeat-get-2": "GetObject", "repeat-get-3": "GetObject"}


def require(ok, msg):
    if not ok:
        raise ValueError(msg)


def unique_json(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, "duplicate JSON key")
        result[key] = value
    return result


def no_constant(_):
    raise ValueError("nonfinite JSON number")


def local_json(path, cap):
    """No symlinks, bounded size, regular files only; no source byte output."""
    fd = os.open(path, os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0))
    try:
        meta = os.fstat(fd)
        require(stat.S_ISREG(meta.st_mode) and 0 < meta.st_size <= cap, "invalid file type/size")
        data = b""
        while len(data) <= cap:
            piece = os.read(fd, min(65536, cap + 1 - len(data)))
            if not piece:
                break
            data += piece
        require(len(data) == meta.st_size, "file changed or exceeded cap")
    finally:
        os.close(fd)
    return json.loads(data.decode("utf-8"), object_pairs_hook=unique_json,
                      parse_constant=no_constant), hashlib.sha256(data).hexdigest()


def safe_time(value):
    require(isinstance(value, str) and len(value) <= 42 and (value.endswith("Z") or value.endswith("+00:00")), "invalid UTC time")
    t = datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(t.utcoffset().total_seconds() == 0, "non-UTC time")
    return t


def number(value, *, integer=False):
    require(type(value) in ((int,) if integer else (int, float)), "invalid numeric type")
    require(math.isfinite(value) and value >= 0, "invalid numeric value")
    return value


def phase_summary(run, manifest):
    phases = run["phases"]
    require(isinstance(phases, list) and 1 <= len(phases) <= len(PHASES), "invalid phases")
    counts = Counter()
    sample_latency = {}
    successful_payloads = Counter()
    failures = 0
    get_valid = 0
    get_invalid = 0
    manifest_by_file = {f["path"]: f for f in manifest}
    require(len(manifest_by_file) == len(manifest), "duplicate manifest path")
    for sample in run["samples"]:
        require(isinstance(sample, dict), "invalid sample")
        phase, op = sample["phase"], sample["operation"]
        require(phase in OPERATIONS and op == OPERATIONS[phase], "invalid sample phase/operation")
        counts[phase] += 1
        sample_latency.setdefault(phase, []).append(number(sample["latency_ns"], integer=True))
        failed = bool(sample.get("error"))
        if not failed and op in ("PutObject", "GetObject"):
            successful_payloads[phase] += number(sample["payload_bytes"], integer=True)
        failures += failed
        if op != "ListObjectsV2":
            f = manifest_by_file.get(sample.get("file"))
            require(f is not None and sample.get("group") == f["group"], "sample is not from dataset")
            require(number(sample["payload_bytes"], integer=True) <= f["bytes"], "sample bytes exceed file")
            if op == "GetObject":
                if not failed and sample.get("sha256") == f["sha256"] and sample["payload_bytes"] == f["bytes"]:
                    get_valid += 1
                else:
                    get_invalid += 1
    result = {}
    for p in phases:
        name = p["name"]
        require(name in PHASES and name not in result, "duplicate/invalid phase")
        metrics = ("samples", "planned", "dispatched", "completed", "skipped", "failed", "elapsed_ns", "successful_logical_bytes", "p95_ns")
        for field in metrics:
            number(p[field], integer=True)
        number(p["successful_logical_bytes_per_second"])
        require(p["planned"] == p["dispatched"] + p["skipped"] and
                p["dispatched"] == p["completed"] and p["samples"] == counts[name] and
                p["completed"] == p["samples"] and p["failed"] <= p["completed"],
                "phase counts inconsistent")
        require(p["samples"] > 0 and p["elapsed_ns"] > 0, "empty phase cannot be measured")
        raw_p95 = sorted(sample_latency[name])[math.ceil(0.95 * len(sample_latency[name])) - 1]
        calculated_bps = p["successful_logical_bytes"] * 1e9 / p["elapsed_ns"]
        require(p["p95_ns"] == raw_p95 and p["successful_logical_bytes"] == successful_payloads[name] and
                math.isclose(p["successful_logical_bytes_per_second"], calculated_bps,
                             rel_tol=1e-8, abs_tol=1e-7), "phase summary does not match samples")
        result[name] = {k: p[k] for k in metrics}
        result[name]["bytes_per_second"] = round(p["successful_logical_bytes_per_second"], 3)
    require(set(counts) == set(result), "sample/phase mismatch")
    require(sum(p["failed"] for p in phases) == failures, "failed sample count inconsistent")
    return result, get_valid, get_invalid


def sanitize_run(run, artifact_sha, clock=None, clock_sha=None):
    require(isinstance(run, dict) and run.get("schema_version") == 1 and
            run.get("mvp_threshold_applied") is False and run.get("target") == "miles-warm"
            and run.get("region") == "ap-southeast-1" and run.get("status") in ("passed", "failed"),
            "unsupported run status or target")
    c = run["concurrency"]
    require(type(c) is int and c in (1, 4) and run["read_repeats"] == 3, "unmatched concurrency/repeat contract")
    require(HEX64.fullmatch(run["manifest_sha256"]), "invalid manifest SHA")
    start, end = safe_time(run["started_at"]), safe_time(run["finished_at"])
    require(start < end, "invalid run window")
    dataset = run["dataset"]
    require(dataset["profile"] == "backup30" and len(dataset["files"]) == 90,
            "dataset is not backup30")
    require(sum(number(f["bytes"], integer=True) for f in dataset["files"]) == number(dataset["total_bytes"], integer=True),
            "dataset byte count mismatch")
    for f in dataset["files"]:
        require(HEX64.fullmatch(f["sha256"]), "invalid full-file hash")
    env, client = run["declared_environment"], run["client"]
    require(env["profile"] == "edge" and env["metadata"] == "dynamodb" and HEX40.fullmatch(env["gateway_commit"]),
            "invalid Edge runtime binding")
    require(HEX64.fullmatch(client["executable_sha256"]), "invalid client executable hash")
    require(run["cleanup"] in ("retained_owned_prefix_for_observation", "objects_may_remain_on_failure"),
            "unsupported cleanup declaration")
    phases, verified, not_verified = phase_summary(run, dataset["files"])
    all_pass = (run["status"] == "passed" and set(phases) == set(PHASES) and
                all(phases[n]["failed"] == 0 and phases[n]["skipped"] == 0 for n in PHASES) and
                all(phases[n]["completed"] == (1 if n in ("list", "preflight-list") else 90) for n in PHASES) and
                verified == 360 and not_verified == 0 and
                phases["put"]["successful_logical_bytes"] == dataset["total_bytes"])
    if run["status"] == "passed":
        require(all_pass, "claimed completed run lacks verified phases")
    clock_result = {"status": "not-provided", "reported_verified": False,
                    "sample_count": None, "window_encloses_run": False}
    if clock is not None:
        require(isinstance(clock, dict) and clock.get("status") in ("OK", "UNVERIFIED", "FAILED") and
                type(clock.get("clock_evidence_verified")) is bool, "invalid clock summary")
        window = (safe_time(clock["window_start_utc"]), safe_time(clock["window_end_utc"]))
        require(window[0] <= window[1], "invalid clock summary window")
        n = number(clock["chrony_sample_count"], integer=True)
        reported_verified = clock["status"] == "OK" and clock["clock_evidence_verified"]
        clock_result = {"status": clock["status"], "reported_verified": reported_verified,
                        "sample_count": n, "window_encloses_run": window[0] <= start and window[1] >= end}
    return {
        "artifact_sha256": artifact_sha, "clock_artifact_sha256": clock_sha,
        "result_status": run["status"], "warm_workload_verified": bool(all_pass),
        "concurrency": c, "runtime_sha": env["gateway_commit"],
        "client_executable_sha256": client["executable_sha256"],
        "client_source_sha": client.get("vcs.revision") if isinstance(client.get("vcs.revision"), str) and HEX40.fullmatch(client["vcs.revision"]) else None,
        "policy_revision": env["policy_revision"] if isinstance(env["policy_revision"], str) and len(env["policy_revision"]) <= 32 and env["policy_revision"].isalnum() else None,
        "manifest_sha256": run["manifest_sha256"], "dataset_file_count": len(dataset["files"]),
        "dataset_payload_bytes": dataset["total_bytes"], "started_at_utc": run["started_at"],
        "finished_at_utc": run["finished_at"], "get_sha256_verified": verified,
        "get_missing_or_invalid": not_verified, "phases": phases, "clock": clock_result,
        "scope": "observed-edge-warm-only", "benchmark_threshold_applied": False,
        "matched_direct_s3": False, "repetitions_accepted": False,
    }


def build(selection, roots):
    require(isinstance(selection, dict) and selection.get("schema_version") == 1 and
            set(selection) == {"schema_version", "runs"} and isinstance(selection["runs"], list),
            "invalid selection")
    require(0 < len(selection["runs"]) <= 40, "invalid run count")
    output, seen, artifact_ids, run_windows, seen_hashes = [], set(), set(), set(), set()
    for item in selection["runs"]:
        require(type(item) is dict and set(item) == {"id", "source", "result", "clock"},
                "invalid run spec")
        rid, source = item["id"], item["source"]
        require(type(rid) is str and LABEL.fullmatch(rid) and rid not in seen, "invalid run ID")
        seen.add(rid)
        require(type(source) is str and source in roots and source in ROOTS and
                type(item["result"]) is str and BASENAME.fullmatch(item["result"]),
                "unknown root or invalid result filename")
        require((source, item["result"]) not in artifact_ids, "duplicate input result")
        artifact_ids.add((source, item["result"]))
        root = roots[source]
        raw, raw_hash = local_json(root / item["result"], 2 * 1024 * 1024)
        summary, digest = None, None
        if item["clock"] is not None:
            require(type(item["clock"]) is str and BASENAME.fullmatch(item["clock"]) and
                    item["clock"].endswith("-telemetry-summary.json"), "invalid telemetry filename")
            summary, digest = local_json(root / item["clock"], 128 * 1024)
        record = sanitize_run(raw, raw_hash, summary, digest)
        window_identity = (record["runtime_sha"], record["concurrency"], record["started_at_utc"], record["finished_at_utc"])
        require(raw_hash not in seen_hashes and window_identity not in run_windows,
                "duplicate artifact or workload window")
        seen_hashes.add(raw_hash)
        run_windows.add(window_identity)
        record.update({"id": rid, "origin_pr": 86 if source == "pr86" else 76,
                       "origin_session": source, "source_result_basename": item["result"]})
        output.append(record)
    observed_cases = sorted({f"warm-edge-c{r['concurrency']}" for r in output if r["warm_workload_verified"]})
    return {"schema_version": 1, "kind": "sanitized-historical-edge-warm-evidence",
            "historically_observed_matrix_case_ids": observed_cases,
            "aws_invoked": False, "benchmark_accepted": False,
            "number_of_runs": len(output), "verified_warm_runs": sum(r["warm_workload_verified"] for r in output),
            "incomplete_or_failed_runs": sum(not r["warm_workload_verified"] for r in output),
            "limitations": ["Only selected historic Edge/Miles runs; no matched direct S3 comparator.",
                            "Clock summary is an independent reported verdict and window-enclosure check; not a newly replayed Chrony proof.",
                            "No pricing, physical inventory, cross-domain, multipart or GLACIER claim.",
                            "Client tool hash and gateway runtime are distinct; a result is not a repeated trial of a later release."],
            "runs": output}


def parse_roots(values):
    roots = {}
    for entry in values:
        label, sep, path = entry.partition("=")
        require(sep and label in ROOTS and label not in roots and path.startswith("/"), "root must be source=/absolute/private/dir")
        p = Path(path)
        require(p.is_dir() and not p.is_symlink(), "missing/symlink private source directory")
        roots[label] = p
    return roots


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--selection", required=True, help="bounded public JSON run selection")
    parser.add_argument("--root", action="append", required=True, help="source=/private/absolute/directory; never persisted")
    parser.add_argument("--out", required=True, help="new local report path (refuses overwrite)")
    args = parser.parse_args(argv)
    try:
        roots = parse_roots(args.root)
        selection, _ = local_json(Path(args.selection), 64 * 1024)
        report = build(selection, roots)
        out = Path(args.out)
        require(out.parent.is_dir() and not out.exists() and not out.is_symlink(), "output must be new within existing directory")
        fd = os.open(out, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            json.dump(report, f, indent=2, ensure_ascii=False, sort_keys=True)
            f.write("\n")
        print(json.dumps({"status": "sanitized", "runs": report["number_of_runs"],
                          "verified": report["verified_warm_runs"], "incomplete": report["incomplete_or_failed_runs"]}))
        return 0
    except (KeyError, TypeError, ValueError, OSError, UnicodeError, OverflowError) as exc:
        print(f"evidence not exported: {type(exc).__name__}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
