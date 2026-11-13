#!/usr/bin/env python3
"""Offline 60-slot budget worksheet; unknown rates never become zero dollars."""

import argparse
import datetime as dt
from decimal import Decimal, InvalidOperation
import json
from pathlib import Path

from deploy.acceptance.common import ROOT, digest, now, require, save


def number(value):
    try:
        n = Decimal(str(value))
    except InvalidOperation as error:
        raise ValueError("Invalid finite budget quantity/rate") from error
    require(n.is_finite() and n >= 0, "Budget values must be finite and nonnegative")
    return n


def estimate(matrix, inputs):
    require(len(matrix["cases"]) == 20 and len({c["id"] for c in matrix["cases"]}) == 20,
            "Budget requires the reviewed 20-case matrix")
    require(inputs["region"] == "ap-southeast-1", "Refresh rates/model for another Region")
    require(inputs["currency"] == "USD", "Budget rates must use USD before credits/tax")
    hours = number(inputs["paid_hours"])
    quantities = {
        "ec2_hours": hours*5, "ebs_gb_months": hours*100/730,
        "alb_hours": hours, "alb_lcu_hours": hours*number(inputs["lcu"]),
        "endpoint_az_hours": hours*22, "endpoint_gb": number(inputs["endpoint_gb"]),
        "nat_hours": number(inputs["nat_count"])*hours, "nat_gb": number(inputs["nat_gb"]),
        "ipv4_hours": number(inputs["ipv4_count"])*hours,
        "ddb_read_units": number(inputs["ddb_read_units"]), "ddb_write_units": number(inputs["ddb_write_units"]),
        "ddb_gb_months": number(inputs["ddb_gb"])*hours/730,
        "ddb_pitr_gb_months": number(inputs["ddb_gb"])*hours/730,
        "kms_key_months": number(inputs["kms_keys"])*hours/730,
        "kms_requests": number(inputs["kms_requests"]),
        "secret_months": number(inputs["secrets"])*hours/730,
        "secret_requests": number(inputs["secret_requests"]),
        "logs_ingest_gb": number(inputs["logs_gb"]), "logs_storage_gb_months": number(inputs["logs_gb"])*number(inputs["evidence_days"])/30,
        "metric_months": number(inputs["metrics"])*hours/730, "monitoring_api_requests": number(inputs["monitoring_api_requests"]),
        "internet_egress_gb": number(inputs["internet_egress_gb"]),
        "cross_az_gb": number(inputs["cross_az_gb"]), "inter_region_gb": number(inputs["inter_region_gb"]),
        "pending_secret_months": number(inputs["pending_secret_billable_months"]),
        "pending_kms_key_months": number(inputs["pending_kms_billable_months"]),
        "edge_external_host_hours": number(inputs["external_edge_hours"]),
    }
    slots, pairs, cold_ir, cold_flex = [], 0, 0, 0
    total_bytes, requests, read_requests = 0, 0, 0
    for case in matrix["cases"]:
        for rep in (1, 2, 3):
            slots.append(case["id"]+f"-rep{rep}")
            size = number(inputs["dataset_bytes"][case["dataset"]])
            count = number(inputs["dataset_files"][case["dataset"]])
            # Covers seed, overwrite, secondary tenant, clone and revival without
            # assuming dedup/compression savings before measurements exist.
            total_bytes += size*number(inputs["write_amplification_allowance"])
            requests += count*number(inputs["request_amplification_allowance"])
            read_requests += count*number(inputs["read_amplification_allowance"])
            if case["comparator"] == "direct-s3":
                pairs += 1; total_bytes += size; requests += count; read_requests += count*4
            if case["scenario"] == "cold":
                if case["variant"] == "ir": cold_ir += 1
                else: cold_flex += 1
    gb = Decimal(1024**3)
    pack_count = number(inputs["cold_objects_per_slot"])
    cold_bytes = number(inputs["cold_bytes_per_slot"])
    # Every retained version is a distinct charge. Flexible includes the native
    # 32KiB archive + 8KiB Standard metadata overhead. IR has 128KiB minimum.
    ir_billable = max(cold_bytes, pack_count*131072)*cold_ir
    flex_billable = (cold_bytes+pack_count*32768)*cold_flex
    quantities.update({
        "standard_gb_months": total_bytes/gb*hours/730 + pack_count*8192*cold_flex/gb*hours/730,
        "s3_put_requests": requests, "s3_get_head_list_requests": read_requests,
        "s3_transition_ir_requests": pack_count*cold_ir,
        "s3_transition_flexible_requests": pack_count*cold_flex,
        "ir_gb_months_90d": ir_billable/gb*3, "flexible_gb_months_90d": flex_billable/gb*3,
        "ir_retrieval_gb": cold_bytes/gb*cold_ir*number(inputs["cold_read_passes"]),
        "ir_get_requests": pack_count*cold_ir*number(inputs["cold_read_passes"]),
        "flexible_restore_requests": pack_count*cold_flex*number(inputs["restore_attempts"]),
        "flexible_restore_gb": cold_bytes/gb*cold_flex*number(inputs["restore_attempts"]),
        "restored_standard_gb_months": cold_bytes/gb*cold_flex*number(inputs["restore_days"])/30,
        "state_evidence_gb_months": number(inputs["state_evidence_gb"])*number(inputs["evidence_days"])/30,
    })
    rates = inputs["rates"]
    require(set(rates) == set(quantities), "Every billing dimension needs an explicit rate entry")
    total = Decimal(0); missing = []; lines = []
    for key,qty in quantities.items():
        rate = rates[key]
        try:
            age = (now().date()-dt.date.fromisoformat(rate["as_of"])).days
            fresh = 0 <= age <= 7
        except (KeyError, TypeError, ValueError):
            fresh = False
        usd = None
        if rate.get("usd") is not None:
            usd = qty*number(rate["usd"]); total += usd
        # An explicit zero quantity is a reviewed exclusion, not a missing rate.
        if qty and (usd is None or not rate.get("source_url", "").startswith("https://")
                    or not fresh or rate.get("verified") is not True): missing.append(key)
        lines.append({"id": key, "quantity": str(qty), "rate": rate, "estimated_usd": str(usd) if usd is not None else None})
    reserve = total*number(inputs["contingency_fraction"])
    return {"schema_version": 1, "generated_at": now().isoformat(), "region": inputs["region"],
            "slots": slots, "slot_count": len(slots), "direct_pairs": pairs,
            "status": "UNKNOWN" if missing else "ESTIMATED", "missing_or_unverified_rates": missing,
            "line_items": lines, "known_subtotal_usd": str(total), "contingency_usd": str(reserve),
            "gross_total_usd": None if missing else str(total+reserve),
            "credits_free_tier_applied": False, "tax_included": False,
            "approval": False, "benchmark_executed": False,
            "window_hours": str(hours), "cleanup_reserve_minutes": 90,
            "window_fit": number(inputs["estimated_work_minutes"]) <= hours*60-90,
            "notes": ["90-day archive charges include early deletion; do not charge them twice.",
                      "Budgets alerts are not a hard billing cap.",
                      "Standard Flexible restore may exceed one 4h window; no implicit extension."]}


def write(r, inputs_path):
    matrix_file = ROOT / "benchmarks/workload-matrix.json"
    out = estimate(json.loads(matrix_file.read_text()), json.loads(Path(inputs_path).read_text()))
    out.update(run_id=r.id, source=r.meta["source"], matrix_sha256=digest(matrix_file), inputs_sha256=digest(inputs_path))
    r.write("benchmark-budget.json", out)
    return out


if __name__ == "__main__":
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--inputs", required=True, type=Path)
    p.add_argument("--out", required=True, type=Path)
    a = p.parse_args()
    value = estimate(json.loads((ROOT/"benchmarks/workload-matrix.json").read_text()), json.loads(a.inputs.read_text()))
    save(a.out, value, new=True)
    print(json.dumps({k: value[k] for k in ("slot_count", "direct_pairs", "status", "gross_total_usd", "window_fit")}))
