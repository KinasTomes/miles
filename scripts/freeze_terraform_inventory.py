"""Freeze managed Terraform resource identities before teardown; never stores state values."""

import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import sys

# Only direct ID mappings are listed. Unknown types remain explicit blockers.
STATE_TYPES = {
    "aws_vpc": "vpc_id",
    "aws_subnet": "subnets",
    "aws_security_group": "security_groups",
    "aws_vpc_endpoint": "vpc_endpoints",
    "aws_nat_gateway": "nat_gateways",
    "aws_eip": "elastic_ips",
    "aws_instance": "instances",
    "aws_ebs_volume": "volumes",
    "aws_s3_bucket": "s3_buckets",
    "aws_dynamodb_table": "dynamodb_tables",
    "aws_iam_role": "iam_roles",
    "aws_iam_instance_profile": "instance_profiles",
    "aws_autoscaling_group": "asg_names",
    "aws_ssm_document": "ssm_documents",
    "aws_ssm_parameter": "ssm_parameters",
    "aws_cloudwatch_metric_alarm": "cloudwatch_alarms",
}


def compare_inventory(state_resources, inventory):
    """Fail closed for state resources absent from inventory or without a verified mapping."""
    errors = []
    for resource in state_resources:
        kind, identity = resource["type"], resource["id"]
        inventory_type = STATE_TYPES.get(kind)
        if inventory_type is None:
            errors.append(f"unsupported_state_resource:{resource['address']}:{kind}")
            continue
        actual = inventory.get(inventory_type)
        expected = [actual] if isinstance(actual, str) else actual if isinstance(actual, list) else []
        if identity not in expected:
            errors.append(f"state_resource_missing:{resource['address']}:{identity}")
    return errors


def extract_resources(state):
    """Walk terraform show -json state, including every nested managed resource."""
    if not isinstance(state, dict) or not isinstance(state.get("values"), dict):
        raise ValueError("invalid Terraform state JSON")
    root = state["values"].get("root_module")
    if not isinstance(root, dict):
        raise ValueError("missing root_module")
    resources = []
    addresses = set()

    def walk(module):
        if not isinstance(module, dict):
            raise ValueError("invalid module")
        for resource in module.get("resources", []):
            if resource.get("mode") != "managed":
                continue
            address, kind = resource.get("address"), resource.get("type")
            values = resource.get("values")
            if not isinstance(address, str) or not address or address in addresses:
                raise ValueError("missing or duplicate resource address")
            if not isinstance(kind, str) or not kind or not isinstance(values, dict):
                raise ValueError(f"invalid resource {address}")
            identity = values.get("id")
            if not isinstance(identity, str) or not identity:
                raise ValueError(f"missing resource ID: {address}")
            addresses.add(address)
            resources.append({"address": address, "type": kind, "id": identity})
        for child in module.get("child_modules", []):
            walk(child)

    walk(root)
    if not resources:
        raise ValueError("empty Terraform state")
    return sorted(resources, key=lambda resource: resource["address"])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--state-json", required=True, help="Private pre-destroy terraform show -json file")
    parser.add_argument("--output", required=True, help="New private inventory file")
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--account-id", required=True)
    parser.add_argument("--region", required=True)
    parser.add_argument("--inventory-json", help="Optional extracted verifier inventory for an offline completeness check")
    args = parser.parse_args()
    if not args.run_id.strip() or not args.region.strip() or len(args.account_id) != 12 or not args.account_id.isdigit():
        parser.error("run ID, region and 12-digit account ID required")
    state_bytes = Path(args.state_json).read_bytes()
    result = {
        "schema_version": "miles-state-inventory/v1",
        "run_id": args.run_id,
        "account_id": args.account_id,
        "region": args.region,
        "captured_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
        "state_sha256": hashlib.sha256(state_bytes).hexdigest(),
        "resources": extract_resources(json.loads(state_bytes)),
    }
    payload = (json.dumps(result, indent=2, sort_keys=True) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    print(hashlib.sha256(payload).hexdigest())
    if args.inventory_json:
        inventory = json.loads(Path(args.inventory_json).read_text(encoding="utf-8"))
        errors = compare_inventory(result["resources"], inventory)
        if errors:
            for error in errors:
                print(error, file=sys.stderr)
            return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
