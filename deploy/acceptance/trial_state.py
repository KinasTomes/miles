"""Standalone trial isolation and per-node telemetry via native cluster heartbeats."""

import datetime as dt
import hashlib
import json

from deploy.acceptance.common import now, require


def domains(run_id, slot, case):
    prefix = "bench-" + hashlib.sha256((run_id+"/"+slot).encode()).hexdigest()[:24]
    if case["scenario"] == "domain" and case["variant"] == "shared-default":
        return [prefix+"-shared", prefix+"-shared"]
    return [prefix+"-a", prefix+"-b"]


def native_binding(r, slot):
    inv = r.outputs("active")["runtime_inventory"]
    gate = r.read("bootstrap-gate.json")["Item"]
    tables = inv["expected_tables"]
    names = ("AdminTokens", "BucketGrants", "Buckets", "Chunks", "CompletionPages", "Credentials",
             "Jobs", "Leases", "ManifestPages", "Nodes", "Objects", "Outbox", "Packs", "QuotaReservations",
             "RecoveryEpochs", "SystemConfig", "UploadPartPages", "UploadParts", "Uploads", "Users")
    mapped = {}
    for name in names:
        matches = [t for t in tables if t.endswith("-"+name)]
        require(len(matches) == 1, "Native table not bound in Terraform inventory: " + name)
        mapped[name] = matches[0]
    return {"account": r.c["account_id"], "role_arn": r.c["deploy_role_arn"], "region": r.c["region"],
            "bucket": inv["data_bucket_name"], "kms_key": inv["kms_key_arn"], "tables": mapped,
            "tags": {"Project": "Miles", "Environment": "dev"}, "run_id": r.id, "slot": slot,
            "source": r.meta["source"], "history": gate["history_id"]["S"],
            "incarnation": gate["incarnation"]["S"],
            "stop_at": (dt.datetime.fromisoformat(r.read("paid-window.json")["cleanup_by"])
                        - dt.timedelta(minutes=90)).isoformat()}


def cache_nodes(report, fresh=True):
    require(report.get("complete") is True, "Incomplete per-node cache telemetry")
    nodes = {}
    at = dt.datetime.fromisoformat(report["observed_at"].replace("Z", "+00:00"))
    if fresh:
        require(abs((now()-at).total_seconds()) < 30, "Stale per-node report")
    for n in report["nodes"]:
        sample = n.get("sample")
        require(n["node_id"] not in nodes and n["state"] == "succeeded" and sample,
                "Missing/duplicate/unavailable node telemetry")
        sampled = dt.datetime.fromisoformat(sample["sampled_at"].replace("Z", "+00:00"))
        require(-2 <= (at-sampled).total_seconds() < 30, "Stale node cache sample")
        nodes[n["node_id"]] = n
    require(nodes, "No per-node samples")
    return nodes


def prepare(r, slot, case, admin):
    ds = domains(r.id, slot, case)
    # A new prefix or bucket is not enough. Consistently scan the Chunks base
    # table for either proposed domain and retain the native response.
    binding = native_binding(r, slot)
    response = r.aws("dynamodb", "scan", "--table-name", binding["tables"]["Chunks"], "--consistent-read",
        "--filter-expression", "begins_with(chunk_id, :a) OR begins_with(chunk_id, :b)",
        "--expression-attribute-values", json.dumps({":a": {"S": ds[0]+"#"}, ":b": {"S": ds[1]+"#"}}),
        "--projection-expression", "chunk_id")
    r.write(f"trials/{slot}/domain-before.json", response, new=True)
    require(not response.get("Items") and not response.get("LastEvaluatedKey"), "Domain already contains chunks or scan incomplete")
    membership = admin(r, "cluster/nodes?limit=100")
    require(not membership.get("next_cursor"), "Cluster member list truncated")
    require(all(n["ready"] and not n["draining"] and n["active_requests"] == 0
                for n in membership["items"]), "Another client/drain is active; refuse cache purge")
    before = admin(r, "cache/stats?scope=cluster")
    nodes = cache_nodes(before)
    require({n["node_id"] for n in membership["items"]} == set(nodes), "Cache/membership set differs")
    cloud = r.aws("elbv2", "describe-target-health", "--target-group-arn", r.outputs("active")["runtime_inventory"]["target_group_arn"])
    targets = cloud["TargetHealthDescriptions"]
    expected = {t["Target"]["Id"] for t in targets}
    require(len(expected) == 2 and all(t["TargetHealth"]["State"] == "healthy" for t in targets), "Expected two healthy Cloud targets")
    edge = r.outputs("hosts")["acceptance_nodes"]["edge"]["instance_id"]
    hosts = expected | {edge}
    matched = {next((h for h in hosts if n == h or n.startswith(h + "-")), n) for n in nodes}
    require(matched == hosts and len(nodes) == len(hosts), "Per-node telemetry does not match exact Cloud fleet + Edge")
    prefix = f"trials/{slot}"
    r.write(prefix+"/cache-before-purge.json", before, new=True)
    if "Jobs" in binding["tables"]:
        def jobs_quiescent():
            res = r.aws(
                "dynamodb", "scan",
                "--table-name", binding["tables"]["Jobs"],
                "--filter-expression", "#s IN (:q, :r, :c)",
                "--expression-attribute-names", json.dumps({"#s": "state"}),
                "--expression-attribute-values", json.dumps({
                    ":q": {"S": "queued"},
                    ":r": {"S": "running"},
                    ":c": {"S": "cancelling"}
                }),
            )
            return len(res.get("Items", [])) == 0
        r.wait("cluster background jobs quiescent before cache purge", jobs_quiescent, 120)
    # Record before dispatch: lost POST response must be reconciled, not resent.
    r.write(prefix+"/purge-intent.json", {"nodes": sorted(nodes), "at": now().isoformat()}, new=True)
    job = admin(r, "cache/purge", {"scope": "cluster"})
    r.write(prefix+"/purge-dispatch.json", job, new=True)
    state = {}
    def purged():
        r.live_window(needed=5)
        current = admin(r, "jobs/"+job["job_id"])
        r.write(prefix+"/purge-job.json", current)
        require(current["state"] not in {"Failed", "Cancelled", "failed", "cancelled"}, "Cache purge failed")
        if current["state"].lower() != "succeeded": return False
        result = current["result"]
        require(result["scope"] == "cluster" and {n["node_id"] for n in result["nodes"]} == set(nodes), "Purge target set changed")
        require(all(n["state"] == "succeeded" and n["result"].get("purged") is True
                    and n["result"].get("dry_run") is False for n in result["nodes"]), "Incomplete cache purge proof")
        report = admin(r, "cache/stats?scope=cluster")
        samples = cache_nodes(report)
        require(set(samples) == set(nodes), "Fleet changed during reset")
        for key,n in samples.items():
            require(n["incarnation"] == nodes[key]["incarnation"], "Node restarted during reset")
            s = n["sample"]
            if dt.datetime.fromisoformat(s["sampled_at"].replace("Z", "+00:00")) <= dt.datetime.fromisoformat(current["finished_at"].replace("Z", "+00:00")): return False
            if any(s[t]["entries"] != 0 or s[t]["used_bytes"] != 0 for t in ("memory", "disk")): return False
        state.update(report)
        return True
    r.wait("native cluster cache purge and fresh empty samples", purged, 120)
    proof = {"domains": ds, "cache": state, "cloud_nodes": sorted(expected), "edge_node": edge,
             "at": now().isoformat(), "scope": "fresh dedup domains plus verified cluster cache purge; Bloom is retained"}
    r.write(prefix+"/isolation.json", proof, new=True)
    return proof


def finish(r, slot, case, admin):
    prefix = f"trials/{slot}"
    baseline = r.read(prefix+"/isolation.json")
    before = cache_nodes(baseline["cache"], fresh=False)
    result = r.read(prefix+"/result.json")
    end = dt.datetime.fromisoformat(result["finished_at"].replace("Z", "+00:00"))
    report = {}
    def observed():
        value = admin(r, "cache/stats?scope=cluster")
        nodes = cache_nodes(value)
        require(set(nodes) == set(before), "Fleet changed during trial")
        for key,node in nodes.items():
            require(node["incarnation"] == before[key]["incarnation"], "Node restarted during trial")
            sampled = dt.datetime.fromisoformat(node["sample"]["sampled_at"].replace("Z", "+00:00"))
            if sampled < end: return False
        report.update(value)
        return True
    r.wait("per-node samples cover the full trial window", observed, 45)
    r.write(prefix+"/per-node-after.json", report, new=True)
    after = cache_nodes(report)
    require(set(before) == set(after), "Fleet changed during trial")
    deltas = {}
    for key,node in after.items():
        old = before[key]
        require(node["incarnation"] == old["incarnation"], "Node restarted; counters not comparable")
        require(node["sample"]["sampled_at"] > old["sample"]["sampled_at"], "No new per-node heartbeat during trial")
        delta = {k: node["sample"][k] - old["sample"][k] for k in ("hits", "misses", "lookups", "errors", "puts")}
        require(all(v >= 0 for v in delta.values()), "Node counters reset mid-trial")
        deltas[key] = delta
    r.write(prefix+"/per-node-deltas.json", {"source": r.meta["source"], "slot": slot, "nodes": deltas}, new=True)
