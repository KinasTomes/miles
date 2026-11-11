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
    "aws_cloudwatch_log_group": "cloudwatch_log_groups",
    "aws_launch_template": "launch_templates",
    "aws_kms_alias": "kms_aliases",
    "aws_lb": "alb_names",
    "aws_lb_target_group": "target_groups",
    "aws_rolesanywhere_trust_anchor": "trust_anchors",
    "aws_rolesanywhere_profile": "rolesanywhere_profiles",
    "aws_kms_key": "kms_keys",
    "aws_secretsmanager_secret": "secrets",
    "aws_internet_gateway": "internet_gateways",
    "aws_route_table": "route_tables",
    "aws_iam_service_linked_role": "iam_roles",
}

# Child resources can disappear only with their owning parent. Both parent ID
# and parent state entry must be present and covered by a native absence check.
CHILD_PARENTS = {
    "aws_route": ("route_table_id", "aws_route_table"),
    "aws_route_table_association": ("route_table_id", "aws_route_table"),
    "aws_vpc_security_group_ingress_rule": ("security_group_id", "aws_security_group"),
    "aws_vpc_security_group_egress_rule": ("security_group_id", "aws_security_group"),
    "aws_s3_bucket_ownership_controls": ("bucket", "aws_s3_bucket"),
    "aws_s3_bucket_public_access_block": ("bucket", "aws_s3_bucket"),
    "aws_s3_bucket_versioning": ("bucket", "aws_s3_bucket"),
    "aws_s3_bucket_server_side_encryption_configuration": ("bucket", "aws_s3_bucket"),
    "aws_s3_bucket_policy": ("bucket", "aws_s3_bucket"),
    "aws_iam_role_policy": ("role", "aws_iam_role"),
    "aws_lb_listener": ("load_balancer_arn", "aws_lb"),
    "aws_volume_attachment": ("volume_id", "aws_ebs_volume"),
}


def inventory_identity(kind, identity):
    if kind == "aws_lb" and ":loadbalancer/" in identity:
        parts = identity.split(":loadbalancer/", 1)[1].split("/")
        return parts[1] if len(parts) == 3 else identity
    if kind == "aws_iam_service_linked_role":
        return identity.rsplit("/", 1)[-1]
    return identity


def augment_inventory(state_resources, inventory):
    """Include direct state-owned resources that are not exposed by Terraform outputs."""
    result = dict(inventory)
    for resource in state_resources:
        kind = resource["type"]
        key = STATE_TYPES.get(kind)
        if not key:
            continue
        identity = inventory_identity(kind, resource["id"])
        if key == "vpc_id":
            if result.get(key) and result[key] != identity:
                raise ValueError("conflicting VPC IDs")
            result[key] = identity
            continue
        existing = result.get(key, [])
        if not isinstance(existing, list):
            raise ValueError(f"invalid inventory type: {key}")
        existing = list(existing)
        if kind == "aws_kms_key" and any(isinstance(x, str) and x.rsplit("/", 1)[-1] == identity for x in existing):
            continue
        if identity not in existing:
            existing.append(identity)
        result[key] = existing
    return result


def compare_inventory(state_resources, inventory):
    """Fail closed for state resources absent from inventory or without a verified mapping."""
    errors = []
    parents = {(r["type"], r["id"]) for r in state_resources}
    for resource in state_resources:
        kind, identity = resource["type"], resource["id"]
        if kind in CHILD_PARENTS:
            parent_type = CHILD_PARENTS[kind][1]
            if (parent_type, resource.get("parent_id")) not in parents:
                errors.append(f"uncovered_parent:{resource['address']}:{parent_type}")
            continue
        inventory_type = STATE_TYPES.get(kind)
        if inventory_type is None:
            errors.append(f"unsupported_state_resource:{resource['address']}:{kind}")
            continue
        actual = inventory.get(inventory_type)
        expected = [actual] if isinstance(actual, str) else actual if isinstance(actual, list) else []
        identity = inventory_identity(kind, identity)
        if kind == "aws_kms_key":
            expected = [value.rsplit("/", 1)[-1] for value in expected if isinstance(value, str)]
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
            entry = {"address": address, "type": kind, "id": identity}
            if kind in CHILD_PARENTS:
                parent = values.get(CHILD_PARENTS[kind][0])
                if not isinstance(parent, str) or not parent:
                    raise ValueError(f"missing parent ID: {address}")
                entry["parent_id"] = parent
            resources.append(entry)
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
