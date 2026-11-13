"""Operator-driven Cold lifecycle. One command dispatches at most one phase.

Waiting is a durable state, not a failed trial or a reason to reseed. Mutation
intents are never replayed automatically after a lost response.
"""

import base64
import datetime as dt
import json
import secrets

from deploy.acceptance.common import ROOT, digest, now, require
from deploy.acceptance import approvals, trial_state
from deploy.acceptance.workload import admin

STAGES = ("transition", "put-cold", "part-cold", "restore-request", "read",
          "put-restored", "part-restored", "protection", "policy-restore", "finish")
REMOTE = "/home/ec2-user/acceptance"


def slot(r, case_id, repetition):
    matrix = json.loads((ROOT / "benchmarks/workload-matrix.json").read_text())
    cases = [c for c in matrix["cases"] if c["id"] == case_id and c["scenario"] == "cold"]
    require(len(cases) == 1 and repetition in (1, 2, 3), "Not a Cold matrix slot")
    name = f"{case_id}-rep{repetition}"
    state = r.read(f"trials/{name}/cold-state.json")
    require(state["source"] == r.meta["source"] and state["run_id"] == r.id,
            "Cold state belongs to another source/run")
    return name, state


def prepare(r, settings):
    name = settings["name"]
    prefix = f"trials/{name}"
    # Persist before remote creation/seed. Losing SSH response is unresolved,
    # never permission to create a second bucket or overwrite the first seed.
    r.write(prefix+"/cold-state.json", {
        "run_id": r.id, "source": r.meta["source"], "variant": settings["variant"],
        "status": "prepare-unknown", "done": {}, "jobs": [], "pending": "prepare",
    }, new=True)
    r.command(["scp", "-F", r.file("ssh-config"), ROOT / "deploy/acceptance/client.py",
               "client:"+REMOTE+"/client.py"])
    r.ssh("client", "python3.12", REMOTE+"/client.py", "benchmark",
          input=json.dumps(settings).encode(), timeout=1340, log=prefix+"/prepare.log")
    value = json.loads(r.ssh("client", "cat", REMOTE+"/"+name+"/result/result.json"))
    require(value["phase"] == "inspect" and value["status"] == "observed",
            "Cold Warm seed/native inspect not verified")
    r.write(prefix+"/seed.json", value, new=True)
    state = r.read(prefix+"/cold-state.json")
    state.update(status="waiting-protection", pending=None, latest=value,
                 manifest_sha256=value["manifest_sha256"])
    r.write(prefix+"/cold-state.json", state)
    print(json.dumps({"slot": name, "status": state["status"],
                      "next": "cold-status, then cold-plan --stage transition"}))


def refresh_credentials(r, settings):
    """New session is allowed; the original paid-window deadline never extends."""
    native = trial_state.native_binding(r, settings["name"])
    native["logical_bucket"] = settings["tenant1"]["bucket"]
    temporary = json.loads(r.command([
        "aws", "--profile", r.c["deploy_profile"], "--region", r.c["region"],
        "configure", "export-credentials", "--format", "process"], env=r.environment()))
    expires = dt.datetime.fromisoformat(temporary["Expiration"].replace("Z", "+00:00"))
    stop = min(dt.datetime.fromisoformat(native["stop_at"]),
               expires-dt.timedelta(minutes=1), now()+dt.timedelta(minutes=18))
    require(stop > now()+dt.timedelta(minutes=4), "Session/cleanup reserve too short")
    native["stop_at"] = stop.isoformat()
    return native, {**{k: temporary[k] for k in (
        "Version", "AccessKeyId", "SecretAccessKey", "SessionToken", "Expiration")},
        "expected_account": r.c["account_id"]}


def phase(r, name, state, action):
    settings = r.read(f"trials/{name}/input.json")
    native, credentials = refresh_credentials(r, settings)
    segment = secrets.token_hex(8)
    prefix = f"trials/{name}/steps/{segment}"
    request = {"name": name, "segment": segment, "native": native,
               "recovery_aws": credentials, "phase": action, "variant": state["variant"]}
    r.write(prefix+"/intent.json", {"phase": action, "at": now().isoformat()}, new=True)
    # Caller persists pending before entering. A failed SSH still retains the
    # remote segment ID, so read-only reconciliation can collect exact receipts.
    state["segment"] = segment
    if action not in {"inspect", "restore-status"}:
        state["mutation_segment"] = segment
    r.write(f"trials/{name}/cold-state.json", state)
    from deploy.acceptance import cloud_telemetry
    telemetry = None if action in {"inspect", "restore-status"} else cloud_telemetry.start(r, name, segment)
    try:
        r.ssh("client", "python3.12", REMOTE+"/client.py", "cold-phase",
              input=json.dumps(request).encode(), timeout=1130, log=prefix+"/dispatch.log")
    finally:
        if telemetry:
            cloud_telemetry.stop(r, telemetry)
    result = json.loads(r.ssh("client", "cat",
        f"{REMOTE}/{name}/steps/{segment}/result/result.json"))
    r.write(prefix+"/result.json", result, new=True)
    require(result["manifest_sha256"] == state["manifest_sha256"], "Dataset changed on resume")
    require(result["status"] in {"observed", "ready", "waiting", "passed"}, "Cold phase failed")
    if telemetry:
        cloud_telemetry.verify(r, telemetry, result["started_at"], result["finished_at"])
    state["latest"] = result
    state["latest_path"] = prefix+"/result.json"
    return result


def at(value):
    return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))


def current_packs(snapshot):
    refs = {(c["Location"]["Pack"]["ID"], c["Location"]["Pack"]["Generation"])
            for c in snapshot["chunks"]}
    packs = [p for p in snapshot["packs"] if (p["Ref"]["ID"], p["Ref"]["Generation"]) in refs]
    require(len(packs) == len(refs) and bool(packs), "Missing current native packs")
    return packs


def require_archived(snapshot, variant):
    expected = "GLACIER_IR" if variant == "ir" else "GLACIER"
    physical = {(p["Ref"]["ID"], p["Ref"]["Generation"]): p for p in snapshot["physical"]}
    for p in current_packs(snapshot):
        native = physical[(p["Ref"]["ID"], p["Ref"]["Generation"])]
        require(p["Class"] == expected and native["Class"] == expected,
                "Tiering not yet verified in both metadata and native S3")


def status(r, case_id, repetition):
    r.identity()
    r.live_window(needed=5)
    name, state = slot(r, case_id, repetition)
    # Never redispatch an unknown write. Status is always observational, including
    # after restore request; it does not infer that an uncertain mutation passed.
    action = "restore-status" if state["done"].get("restore-request") else "inspect"
    result = phase(r, name, state, action)
    snap = result["snapshot"]
    state["status"] = result["status"]
    state["transition_ready"] = all(at(p["ProtectedUntil"]) <= at(snap["bounds"]["Lower"])
                                    for p in current_packs(snap))
    state["status_at"] = now().isoformat()
    r.write(f"trials/{name}/cold-state.json", state)
    print(json.dumps({"slot": name, "status": state["status"], "pending": state["pending"],
                      "transition_ready": state["transition_ready"]}))


def reconcile(r, case_id, repetition):
    """Collect an exact lost-response receipt; never repeat the HTTP mutation."""
    name, state = slot(r, case_id, repetition)
    pending = state["pending"]
    if pending == "prepare":
        value = json.loads(r.ssh("client", "cat", f"{REMOTE}/{name}/result/result.json"))
        require(value["phase"] == "inspect" and value["status"] == "observed", "Seed still unresolved")
        require(value["snapshot"]["binding"]["run_id"] == r.id
                and value["snapshot"]["binding"]["source"] == r.meta["source"]
                and value["snapshot"]["binding"]["slot"] == name, "Foreign seed receipt")
        r.write(f"trials/{name}/seed.json", value, new=True)
        state.update(latest=value, manifest_sha256=value["manifest_sha256"], pending=None,
                     status="waiting-protection")
    elif pending in {"transition", "protection"}:
        require(state.get("native_done", {}).get(pending),
                "Native job outcome unresolved; inspect exact jobs/policy without redispatch")
        from deploy.acceptance import cloud_telemetry
        receipt = r.read(state["job_telemetry"])
        window = r.read(receipt["prefix"]+"/window.json")
        cloud_telemetry.stop(r, receipt)
        cloud_telemetry.verify(r, receipt, window["start"], window["end"])
        state["done"][pending] = state["native_done"][pending]
        state.update(pending=None, status="await-operator")
    else:
        require(pending in {"read", "restore-request", "put-cold", "part-cold", "put-restored", "part-restored"},
                "No phase receipt to reconcile; unknown policy/job outcome needs operator inspection")
        segment = state["mutation_segment"]
        value = json.loads(r.ssh("client", "cat", f"{REMOTE}/{name}/steps/{segment}/result/result.json"))
        require(value["phase"] == pending and value["manifest_sha256"] == state["manifest_sha256"]
                and value["status"] == ("waiting" if pending == "restore-request" else "passed"),
                "Phase missing/failed; do not retry an unknown write")
        from deploy.acceptance import cloud_telemetry
        receipt = r.read(f"trials/{name}/telemetry/{segment}/receipt.json")
        cloud_telemetry.stop(r, receipt)
        cloud_telemetry.verify(r, receipt, value["started_at"], value["finished_at"])
        path = f"trials/{name}/steps/{segment}/reconciled.json"
        r.write(path, value, new=True)
        state["done"][pending] = {"at": now().isoformat(), "evidence": path}
        state.update(latest=value, latest_path=path, pending=None, status="await-operator")
    r.write(f"trials/{name}/cold-state.json", state)


def plan(r, case_id, repetition, stage):
    name, state = slot(r, case_id, repetition)
    require(stage in STAGES, "Invalid Cold stage")
    require(not state["pending"] or stage == "policy-restore", "Unknown outcome; reconcile before dispatch")
    if stage == "transition":
        require(state.get("transition_ready") and (now()-at(state["status_at"])).total_seconds() < 60,
                "Obtain fresh cold-status after the real protection deadline")
    r.write("cold-plan.json", {"slot": name, "stage": stage, "source": r.meta["source"],
        "state_sha256": digest(r.file(f"trials/{name}/cold-state.json")),
        "packs": [p["Ref"] for p in current_packs(state["latest"]["snapshot"])],
        "policy": admin(r, "policy"), "at": now().isoformat()})


def queue(r, prefix, state, packs, revision):
    for index, p in enumerate(packs):
        path = f"{prefix}/job-{len(state['jobs'])}-{index}"
        r.write(path+"-intent.json", {"pack": p["Ref"], "policy_revision": revision}, new=True)
        job = admin(r, "jobs", {"type": "tiering", "policy_revision": revision,
                               "params": {"pack_id": p["Ref"]["ID"], "generation": p["Ref"]["Generation"]}})
        r.write(path+".json", job, new=True)
        state["jobs"].append(job["job_id"])
        # Persist every known job before dispatching another.
        r.write(prefix+"/cold-state.json", state)


def restore_policy(r, prefix, state):
    original = state.get("policy_original")
    require(original is not None and state.get("policy_owned"), "No owned policy to restore")
    current = admin(r, "policy")
    require(current == state["policy_owned"], "Concurrent policy change; do not overwrite")
    r.write(prefix+"/policy-restore-intent.json", {"expected_revision": current["revision"]})
    restored = admin(r, "policy", {"expected_revision": current["revision"],
                    **{k: original[k] for k in ("tiering", "compression", "chunking")}}, method="PUT")
    state["policy_restored"] = restored
    state["policy_owned"] = None
    r.write(prefix+"/cold-state.json", state)


def step(r, case_id, repetition, stage):
    # Tiering/protection jobs also need the actual node clock and OS window.
    # HTTP data phases own their narrower collector in phase().
    if stage not in {"transition", "protection"}:
        return _step(r, case_id, repetition, stage)
    r.identity()
    r.live_window(needed=25)
    approvals.check(r, "cold-step")
    name, state = slot(r, case_id, repetition)
    reviewed = r.read("cold-plan.json")
    require(reviewed["slot"] == name and reviewed["stage"] == stage
            and reviewed["state_sha256"] == digest(r.file(f"trials/{name}/cold-state.json")),
            "Cold state changed since review")
    from deploy.acceptance import cloud_telemetry
    receipt = cloud_telemetry.start(r, name, secrets.token_hex(8))
    start = now().isoformat()
    state = r.read(f"trials/{name}/cold-state.json")
    state["job_telemetry"] = receipt["prefix"]+"/receipt.json"
    r.write(f"trials/{name}/cold-state.json", state)
    diagnostic_digest = digest(r.file(f"trials/{name}/cold-state.json"))
    # The original state was checked above. Only this diagnostic pointer changed;
    # keep the reviewed artifact immutable throughout execution.
    try:
        _step(r, case_id, repetition, stage, diagnostic_state=diagnostic_digest)
    finally:
        end = now().isoformat()
        r.write(receipt["prefix"]+"/window.json", {"start": start, "end": end}, new=True)
        cloud_telemetry.stop(r, receipt)
    cloud_telemetry.verify(r, receipt, start, end)
    state = r.read(f"trials/{name}/cold-state.json")
    state["done"][stage] = state["native_done"][stage]
    state.update(pending=None, status="await-operator")
    r.write(f"trials/{name}/cold-state.json", state)


def _step(r, case_id, repetition, stage, diagnostic_state=None):
    r.identity()
    r.live_window(needed=25)
    approvals.check(r, "cold-step")
    name, state = slot(r, case_id, repetition)
    prefix = f"trials/{name}"
    reviewed = r.read("cold-plan.json")
    require(reviewed["slot"] == name and reviewed["stage"] == stage
            and (diagnostic_state or reviewed["state_sha256"]) == digest(r.file(prefix+"/cold-state.json")),
            "Cold plan/state changed; review again")
    require(not state["pending"] or stage == "policy-restore", "Reconcile unknown outcome first")
    require(not state["done"].get(stage), "Cold mutation already completed")
    if stage == "policy-restore":
        restore_policy(r, prefix, state)
        return
    done = state["done"]
    prerequisites = {"put-cold": "transition", "part-cold": "put-cold",
        "restore-request": "part-cold", "read": "part-cold", "put-restored": "read",
        "part-restored": "put-restored", "protection": "part-cold"}
    if stage in prerequisites:
        require(done.get(prerequisites[stage]), "Earlier Cold phase not verified")
    if state["variant"] == "ir":
        require(stage not in {"restore-request", "put-restored", "part-restored"}, "IR requires no restore/revival")
    if stage == "read" and state["variant"] == "flexible":
        require(done.get("restore-request") and state["latest"]["status"] == "ready",
                "Run cold-status until native restore is ready")
    if stage in {"transition", "protection"}:
        packs = current_packs(state["latest"]["snapshot"])
        if stage == "transition":
            require(all(at(p["ProtectedUntil"]) <= at(state["latest"]["snapshot"]["bounds"]["Lower"])
                        for p in packs), "Protection has not elapsed")
        else:
            require(state["variant"] == "flexible" and done.get("part-restored"),
                    "Protection proof requires both Cold and Restored revival first")
            snap = state["latest"]["snapshot"]
            last_probe = state["latest"]["probes"][3]
            keys = snap["files"][last_probe]
            refs = [c["Location"]["Pack"] for c in snap["chunks"] if c["Key"] in keys]
            packs = [p for p in packs if p["Class"] == "STANDARD" and p["Ref"] in refs]
            require(packs and all(at(p["ProtectedUntil"]) > at(state["latest"]["snapshot"]["bounds"]["Upper"])
                                 for p in packs), "No currently protected revived Warm pack")
        policy = admin(r, "policy")
        require(policy == reviewed["policy"], "Policy changed since review")
        require(not policy["tiering"]["enabled"] and not policy["tiering"]["auto_restore"],
                "Use a dedicated acceptance installation with background tiering/auto_restore disabled")
        membership = admin(r, "cluster/nodes?limit=100")
        require(not membership.get("next_cursor") and membership.get("items") and all(
            n["ready"] and not n["draining"] and n["active_requests"] == 0 for n in membership["items"]),
            "Another workload/drain is active; do not change global acceptance policy")
        state["pending"] = stage
        state["policy_original"] = policy
        r.write(prefix+"/cold-state.json", state)
        tiering = {**policy["tiering"], "enabled": True, "cold_after_days": 0,
                   "min_pack_live_ratio": 0, "auto_restore": False,
                   "cold_storage_class": "GLACIER_IR" if state["variant"] == "ir" else "GLACIER"}
        r.write(prefix+f"/{stage}-policy-intent.json", {"expected_revision": policy["revision"], "tiering": tiering}, new=True)
        owned = admin(r, "policy", {"expected_revision": policy["revision"], "tiering": tiering}, method="PUT")
        state["policy_owned"] = owned
        r.write(prefix+"/cold-state.json", state)
        # GetPolicy(revision) is historical. Restore the disabled current policy
        # before enqueue; even a large pack list must not leave discovery enabled.
        restore_policy(r, prefix, state)
        queue(r, prefix, state, packs, owned["revision"])
        if stage == "protection":
            binding = trial_state.native_binding(r, name)
            job_ids = state["jobs"][-len(packs):]
            def deferred():
                proofs = []
                for job_id in job_ids:
                    item = r.aws("dynamodb", "get-item", "--table-name", binding["tables"]["Jobs"],
                                 "--key", json.dumps({"job_id": {"S": job_id}}), "--consistent-read")["Item"]
                    checkpoint = json.loads(base64.b64decode(item.get("checkpoint", {}).get("B", "e30=")))
                    require(item["state"]["S"] not in {"succeeded", "failed", "cancelled"}, "Protection job terminated without deferral")
                    if checkpoint.get("deferred") != "r1_busy_or_retained": return False
                    proofs.append(item)
                r.write(prefix+"/protection-deferred.json", proofs, new=True)
                return True
            r.wait("native protected-pack deferral", deferred, 90)
            for job_id in job_ids:
                admin(r, "jobs/"+job_id, method="DELETE")
            r.wait("protected job cancellation", lambda: all(
                admin(r, "jobs/"+j)["state"] == "cancelled" for j in job_ids), 90)
            phase(r, name, state, "inspect")
            protected_refs = [p["Ref"] for p in packs]
            require(all(p["Class"] == "STANDARD" for p in state["latest"]["snapshot"]["packs"]
                        if p["Ref"] in protected_refs),
                    "Protected revived pack transitioned unexpectedly")
    elif stage == "finish":
        required = {"transition", "put-cold", "part-cold", "read"}
        if state["variant"] == "flexible":
            required |= {"restore-request", "put-restored", "part-restored", "protection"}
        require(all(done.get(s) for s in required) and not state.get("policy_owned"), "Cold lifecycle incomplete")
        phase(r, name, state, "inspect")
        r.write(prefix+"/case-verdict.json", {"status": "passed", "scope": "live-cold-lifecycle",
                "source": r.meta["source"], "manifest_sha256": state["manifest_sha256"],
                "phases": done, "latest": state["latest_path"], "measured": True}, new=True)
    else:
        if stage == "put-cold":
            require_archived(state["latest"]["snapshot"], state["variant"])
        state["pending"] = stage
        r.write(prefix+"/cold-state.json", state)
        result = phase(r, name, state, stage)
        if stage == "restore-request":
            require(result["status"] == "waiting", "Restore did not return an asynchronous receipt")
        else:
            require(result["status"] == "passed", "Cold phase did not pass")
    gate = {"at": now().isoformat(), "evidence": state.get("latest_path")}
    if stage in {"transition", "protection"}:
        state.setdefault("native_done", {})[stage] = gate
    else:
        state["done"][stage] = gate
        state["pending"] = None
    state["status"] = "passed" if stage == "finish" else "await-operator"
    r.write(prefix+"/cold-state.json", state)
    print(json.dumps({"slot": name, "stage": stage, "status": state["status"]}))
