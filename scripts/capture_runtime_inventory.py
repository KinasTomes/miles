"""Capture current ASG instances and EBS attachments before teardown."""

import argparse
import datetime
import hashlib
import hmac
import json
import os
from pathlib import Path
import subprocess


def utc_timestamp(value):
    if not isinstance(value, str):
        raise ValueError("timestamp required")
    parsed = datetime.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamp must include timezone")
    return parsed.astimezone(datetime.timezone.utc)


def volume_ids(response):
    volumes = response.get("Volumes") if isinstance(response, dict) else None
    if not isinstance(volumes, list):
        raise ValueError("invalid EC2 volumes response")
    ids = [volume.get("VolumeId") if isinstance(volume, dict) else None for volume in volumes]
    if any(not isinstance(identity, str) or not identity for identity in ids) or len(ids) != len(set(ids)):
        raise ValueError("missing or duplicate volume ID")
    return set(ids)


def validate_volume_baseline(baseline_bytes, expected_sha256, run_id, account_id, region):
    if not hmac.compare_digest(hashlib.sha256(baseline_bytes).hexdigest(), expected_sha256):
        raise ValueError("volume baseline hash mismatch")
    baseline = json.loads(baseline_bytes)
    if baseline.get("schema_version") != "miles-volume-baseline/v1" or (baseline.get("run_id"), baseline.get("account_id"), baseline.get("region")) != (run_id, account_id, region):
        raise ValueError("volume baseline identity mismatch")
    utc_timestamp(baseline.get("captured_at_utc"))
    ids = baseline.get("volumes")
    if not isinstance(ids, list) or any(not isinstance(item, str) or not item for item in ids) or len(ids) != len(set(ids)):
        raise ValueError("invalid volume baseline")
    return set(ids)


def runtime_residuals(instance_response, volume_response, baseline_ids, vpc_id):
    reservations = instance_response.get("Reservations") if isinstance(instance_response, dict) else None
    if not isinstance(reservations, list):
        raise ValueError("invalid EC2 instance response")
    residual_instances = []
    seen = set()
    for reservation in reservations:
        instances = reservation.get("Instances") if isinstance(reservation, dict) else None
        if not isinstance(instances, list):
            raise ValueError("invalid EC2 reservation")
        for instance in instances:
            identity = instance.get("InstanceId")
            state = instance.get("State", {}).get("Name")
            if not isinstance(identity, str) or not identity or identity in seen or instance.get("VpcId") != vpc_id or not isinstance(state, str):
                raise ValueError("invalid EC2 instance")
            seen.add(identity)
            if state != "terminated":
                residual_instances.append(identity)
    return {"instances": sorted(residual_instances), "new_regional_volumes": sorted(volume_ids(volume_response) - baseline_ids)}


def reconcile_snapshot(snapshot, state_resources, inventory, run_id, account_id, region):
    errors = []
    if not isinstance(snapshot, dict) or snapshot.get("schema_version") != "miles-runtime-inventory/v1":
        return ["invalid_runtime_snapshot"]
    try:
        utc_timestamp(snapshot.get("captured_at_utc"))
    except ValueError:
        errors.append("invalid_runtime_snapshot_timestamp")
    if (snapshot.get("run_id"), snapshot.get("account_id"), snapshot.get("region")) != (run_id, account_id, region):
        errors.append("runtime_snapshot_identity_mismatch")
    groups = {r["id"] for r in state_resources if r["type"] == "aws_autoscaling_group"}
    standalone = {r["id"] for r in state_resources if r["type"] == "aws_instance"}
    vpcs = {r["id"] for r in state_resources if r["type"] == "aws_vpc"}
    if vpcs != {snapshot.get("vpc_id")} or inventory.get("vpc_id") != snapshot.get("vpc_id"):
        errors.append("runtime_vpc_mismatch")
    for key in ("asg_names", "instances", "volumes", "new_regional_volumes"):
        value = snapshot.get(key)
        if not isinstance(value, list) or any(not isinstance(item, str) or not item for item in value) or len(value) != len(set(value)):
            errors.append(f"invalid_runtime_snapshot_{key}")
    if errors:
        return errors
    baseline_hash = snapshot.get("volume_baseline_sha256")
    if not isinstance(baseline_hash, str) or len(baseline_hash) != 64 or any(c not in "0123456789abcdef" for c in baseline_hash):
        errors.append("invalid_volume_baseline_hash")
    if not set(snapshot["new_regional_volumes"]).issubset(snapshot["volumes"]):
        errors.append("runtime_missing_new_regional_volumes")
    if set(snapshot["asg_names"]) != groups:
        errors.append("runtime_asg_mismatch")
    if not standalone.issubset(snapshot["instances"]):
        errors.append("runtime_missing_state_instances")
    for key in ("asg_names", "instances", "volumes"):
        if not set(snapshot[key]).issubset(set(inventory.get(key, []))):
            errors.append(f"runtime_inventory_missing_{key}")
    return errors


def assemble_inventory(asg_response, instance_response, asg_names, extra_instances, vpc_id):
    groups = asg_response.get("AutoScalingGroups")
    reservations = instance_response.get("Reservations")
    if not isinstance(groups, list) or not isinstance(reservations, list):
        raise ValueError("invalid AWS response")
    names = [group.get("AutoScalingGroupName") for group in groups]
    if len(names) != len(set(names)) or set(names) != set(asg_names):
        raise ValueError("ASG discovery does not match expected groups")
    instance_ids = set(extra_instances)
    for group in groups:
        instances = group.get("Instances")
        if not isinstance(instances, list):
            raise ValueError("invalid ASG instance list")
        for item in instances:
            identity = item.get("InstanceId")
            if not isinstance(identity, str) or not identity:
                raise ValueError("missing ASG instance ID")
            instance_ids.add(identity)
    instances = [instance for reservation in reservations for instance in reservation.get("Instances", [])]
    ids = [instance.get("InstanceId") for instance in instances]
    if len(ids) != len(set(ids)) or not instance_ids.issubset(ids):
        raise ValueError("EC2 discovery does not include expected instances")
    if any(instance.get("VpcId") != vpc_id for instance in instances):
        raise ValueError("EC2 instance is outside the owned VPC")
    instance_ids.update(ids)
    volumes = set()
    for instance in instances:
        mappings = instance.get("BlockDeviceMappings")
        if mappings is None and instance.get("State", {}).get("Name") == "terminated":
            mappings = []
        if not isinstance(mappings, list):
            raise ValueError("missing block device mappings")
        for mapping in mappings:
            ebs = mapping.get("Ebs")
            if ebs is not None:
                identity = ebs.get("VolumeId")
                if not isinstance(identity, str) or not identity:
                    raise ValueError("missing EBS volume ID")
                volumes.add(identity)
    return {"vpc_id": vpc_id, "asg_names": sorted(asg_names), "instances": sorted(instance_ids), "volumes": sorted(volumes)}


def aws_json(profile, region, *args):
    command = ["aws", "--profile", profile, "--region", region, "--no-cli-pager", *args, "--output", "json"]
    result = subprocess.run(command, check=True, capture_output=True, text=True, timeout=60)
    return json.loads(result.stdout)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--profile", required=True)
    parser.add_argument("--region", required=True)
    parser.add_argument("--account-id", required=True)
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--vpc-id", required=True, help="Owned VPC, reviewed from Terraform state")
    parser.add_argument("--volume-baseline", required=True, help="Private pre-run regional volume baseline")
    parser.add_argument("--volume-baseline-sha256", required=True)
    parser.add_argument("--asg", action="append", default=[])
    parser.add_argument("--instance", action="append", default=[])
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    if not args.run_id.strip() or len(args.account_id) != 12 or not args.account_id.isdigit():
        parser.error("run ID and 12-digit account ID required")
    identity = aws_json(args.profile, args.region, "sts", "get-caller-identity")
    if identity.get("Account") != args.account_id:
        raise ValueError("AWS account mismatch")
    if len(args.asg) != len(set(args.asg)) or len(args.instance) != len(set(args.instance)):
        raise ValueError("duplicate resource IDs")
    baseline_bytes = Path(args.volume_baseline).read_bytes()
    baseline_ids = validate_volume_baseline(baseline_bytes, args.volume_baseline_sha256, args.run_id, args.account_id, args.region)
    groups = aws_json(args.profile, args.region, "autoscaling", "describe-auto-scaling-groups", "--auto-scaling-group-names", *args.asg) if args.asg else {"AutoScalingGroups": []}
    instances = aws_json(args.profile, args.region, "ec2", "describe-instances", "--filters", f"Name=vpc-id,Values={args.vpc_id}")
    snapshot = {"schema_version": "miles-runtime-inventory/v1", "run_id": args.run_id, "account_id": args.account_id, "region": args.region, "captured_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(), **assemble_inventory(groups, instances, args.asg, args.instance, args.vpc_id)}
    current_volumes = volume_ids(aws_json(args.profile, args.region, "ec2", "describe-volumes"))
    snapshot["new_regional_volumes"] = sorted(current_volumes - baseline_ids)
    snapshot["volumes"] = sorted(set(snapshot["volumes"]) | set(snapshot["new_regional_volumes"]))
    snapshot["volume_baseline_sha256"] = args.volume_baseline_sha256
    payload = (json.dumps(snapshot, sort_keys=True, indent=2) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    print(hashlib.sha256(payload).hexdigest())


if __name__ == "__main__":
    main()
