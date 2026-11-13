"""Exact-node telemetry through a hash-pinned, fixed SSM diagnostic document."""

import base64
import datetime as dt
import gzip
import hashlib
import io
import json

from deploy.acceptance.common import now, private, require
from scripts.edge_host_telemetry import HostTelemetryRecorder

MAX_BYTES = 8 << 20


def targets(r):
    inventory = r.outputs("active")["runtime_inventory"]
    doc = inventory["telemetry_document"]
    require(doc and doc["hash_type"] == "Sha256" and len(doc["hash"]) == 64,
            "Apply reviewed telemetry document before foreground trials")
    actual = r.aws("ssm", "describe-document", "--name", doc["name"],
                   "--document-version", str(doc["version"]))["Document"]
    require(actual["Hash"] == doc["hash"] and actual["HashType"] == "Sha256"
            and actual["Owner"] == r.c["account_id"] and actual["DocumentType"] == "Command",
            "Telemetry document changed or is not owned by this account")
    fleet = r.aws("elbv2", "describe-target-health", "--target-group-arn", inventory["target_group_arn"])
    rows = fleet["TargetHealthDescriptions"]
    ids = sorted({row["Target"]["Id"] for row in rows})
    require(len(ids) == 2 and len(rows) == 2 and all(row["TargetHealth"]["State"] == "healthy" for row in rows),
            "Expected exactly two healthy Cloud instances")
    cloud_ids = set(ids)
    edge = r.outputs("hosts")["acceptance_nodes"]["edge"]["instance_id"]
    require(edge not in cloud_ids, "Edge and Cloud inventory overlap")
    ids.append(edge)
    instances = r.aws("ec2", "describe-instances", "--instance-ids", *ids)
    found = {}
    for reservation in instances["Reservations"]:
        require(reservation["OwnerId"] == r.c["account_id"], "Foreign Cloud target account")
        for instance in reservation["Instances"]:
            tags = {t["Key"]: t["Value"] for t in instance["Tags"]}
            require(all(tags.get(k) == v for k,v in {
                "Project": "Miles", "Environment": "dev", "Installation": "miles-dev"}.items())
                    and instance["State"]["Name"] == "running", "Cloud target installation/tag mismatch")
            node = instance["InstanceId"]
            require(node not in found, "Duplicate instance in diagnostic inventory")
            found[node] = {"address": instance["PrivateIpAddress"]+":9100",
                           "profile": "cloud" if node in cloud_ids else "edge"}
    require(set(found) == set(ids), "Cloud EC2/ALB inventory differs")
    return doc, found


def send(r, receipt, node, action, offset=0):
    doc = receipt["document"]
    params = {"action": [action], "runid": [r.id.lower()], "slot": [receipt["slot"]],
              "segment": [receipt["segment"]], "source": [r.meta["source"]], "offset": [str(offset)]}
    # Persist before dispatch, including read/control calls. No generic shell
    # document permission and no caller-provided command text.
    path = receipt["prefix"]+f"/{node}/{action}-{offset}-{now().strftime('%H%M%S%f')}"
    r.write(path+"-intent.json", {"parameters": params, "node": node, "document": doc}, new=True)
    command = r.aws("ssm", "send-command", "--instance-ids", node, "--document-name", doc["name"],
        "--document-version", str(doc["version"]), "--document-hash", doc["hash"],
        "--document-hash-type", "Sha256", "--parameters", json.dumps(params), "--timeout-seconds", "60")
    r.write(path+".json", command, new=True)
    return command["Command"]["CommandId"]


def invocation(r, node, command):
    return r.aws("ssm", "get-command-invocation", "--command-id", command,
                 "--instance-id", node, "--plugin-name", "diagnostics")


def control(r, receipt, node, action, offset=0):
    command = send(r, receipt, node, action, offset)
    result = {}
    def finished():
        try:
            value = invocation(r, node, command)
        except ValueError:
            # Eventual invocation visibility; reads only, never repeat SendCommand.
            return False
        if value["Status"] in {"Pending", "InProgress", "Delayed"}: return False
        require(value["Status"] == "Success" and value["ResponseCode"] == 0, "Cloud diagnostic command failed")
        result.update(value)
        return True
    r.wait("Cloud telemetry "+action, finished, 60)
    return json.loads(result["StandardOutputContent"])


def start(r, slot, segment):
    doc, nodes = targets(r)
    prefix = f"trials/{slot}/telemetry/{segment}"
    receipt = {"run_id": r.id, "source": r.meta["source"], "slot": slot, "segment": segment,
               "document": doc, "nodes": nodes, "commands": {}, "prefix": prefix}
    r.write(prefix+"/receipt.json", receipt, new=True)
    try:
        for node in nodes:
            # A lost SendCommand ACK leaves this deterministic remote directory
            # reconcilable via probe/stop; never run a replacement recorder.
            receipt["commands"][node] = None
            r.write(prefix+"/receipt.json", receipt)
            receipt["commands"][node] = send(r, receipt, node, "run")
            r.write(prefix+"/receipt.json", receipt)
        ready = {}
        for node in nodes:
            def growing():
                try:
                    value = control(r, receipt, node, "probe")
                except ValueError:
                    return False
                require(value["identity"]["instance"] == node
                        and value["identity"]["account"] == r.c["account_id"]
                        and value["identity"]["region"] == r.c["region"]
                        and value["identity"]["source"] == r.meta["source"]
                        and all(value["identity"][k] == v for k,v in nodes[node].items()),
                        "Cloud/Edge recorder identity differs")
                if value["samples"] < 2: return False
                ready[node] = value
                return True
            r.wait("Cloud recorder growing samples", growing, 90)
        receipt["ready"] = ready
        r.write(prefix+"/receipt.json", receipt)
        return receipt
    except BaseException:
        # Preserve the original failure if diagnostic stop also fails.
        for node in receipt["commands"]:
            try:
                control(r, receipt, node, "stop")
            except (ValueError, OSError):
                pass
        raise


def stop(r, receipt):
    require(receipt["run_id"] == r.id and receipt["source"] == r.meta["source"], "Foreign telemetry receipt")
    for node, command in receipt["commands"].items():
        control(r, receipt, node, "stop")
        if command:
            def finished():
                try:
                    result = invocation(r, node, command)
                except ValueError:
                    return False
                if result["Status"] in {"Pending", "InProgress", "Delayed"}: return False
                require(result["Status"] == "Success" and result["ResponseCode"] == 0,
                        "Cloud sampler failed; retain raw evidence")
                return True
            r.wait("Cloud recorder stopped", finished, 60)


def verify_bundle(payload, expected, start, end):
    identity = payload["identity"]
    require(identity == expected, "Node process/boot/source changed during trial")
    records, metrics = payload["records"], payload["metrics"]
    require(len(records) >= 2 and len(records) == len(metrics), "Incomplete Cloud metrics/clock series")
    def at(value):
        return dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    require(at(records[0]["timestamp_utc"]) <= at(start) and at(records[-1]["timestamp_utc"]) >= at(end),
            "Cloud samples do not cover foreground window")
    for index, (record, sample) in enumerate(zip(records, metrics), 1):
        require(record["sequence"] == index and sample["sequence"] == index
                and sample["at"] == record["timestamp_utc"]
                and "miles_" in sample["text"], "Missing/reordered per-node metric sample")
        require(all(record.get("os_pressure", {}).get(k) for k in ("stat", "meminfo", "loadavg")),
                "Cloud OS telemetry unavailable")
        require(all("error" not in record["os_pressure"][k] for k in ("stat", "meminfo", "loadavg")),
                "Cloud OS probe failed")


def verify(r, receipt, start, end):
    doc, nodes = targets(r)
    require(doc == receipt["document"] and nodes == receipt["nodes"], "Cloud fleet/document changed during trial")
    for node in nodes:
        summary = control(r, receipt, node, "summary")
        require(summary["identity"] == receipt["ready"][node]["identity"]
                and 0 < summary["bytes"] <= MAX_BYTES and 0 < summary["raw_bytes"] <= MAX_BYTES,
                "Invalid Cloud telemetry summary")
        compressed = bytearray()
        while len(compressed) < summary["bytes"]:
            chunk = control(r, receipt, node, "read", len(compressed))
            require(chunk["offset"] == len(compressed), "Cloud chunk offset differs")
            body = base64.b64decode(chunk["data"], validate=True)
            require(0 < len(body) <= 12000, "Empty/oversized Cloud chunk")
            compressed.extend(body)
            require(len(compressed) <= summary["bytes"], "Cloud artifact exceeds declared size")
        require(hashlib.sha256(compressed).hexdigest() == summary["sha256"], "Cloud telemetry SHA256 differs")
        with gzip.GzipFile(fileobj=io.BytesIO(compressed)) as stream:
            raw = stream.read(MAX_BYTES+1)
        require(len(raw) == summary["raw_bytes"], "Cloud decompressed artifact truncated/oversized")
        payload = json.loads(raw)
        verify_bundle(payload, summary["identity"], start, end)
        prefix = receipt["prefix"]+"/"+node
        r.file(prefix+"/samples.json.gz").write_bytes(compressed)
        private(r.file(prefix+"/host.jsonl"), "\n".join(json.dumps(x) for x in payload["records"])+"\n")
        recorder = HostTelemetryRecorder(r.file(prefix+"/host.jsonl"), max_bytes=MAX_BYTES)
        report = recorder.summarize_window(since_utc=start, until_utc=end)
        r.write(prefix+"/summary.json", {**summary, "clock": report, "window": [start, end]})
        require(report.get("status") == "OK" and report.get("clock_evidence_verified") is True,
                "Cloud Chrony window unverified")
