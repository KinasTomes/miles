"""Capture native account resource IDs for pre-run/post-teardown comparison."""

import argparse
import datetime
import hashlib
import hmac
import json
import os
from pathlib import Path
import re
import sys

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
from scripts.capture_runtime_inventory import aws_json

# List APIs are intentionally account-wide: concurrent resources produce a
# conservative residual, never permission to delete them.
SPECS = {
    "buckets": (("s3api", "list-buckets"), "Buckets", "Name"),
    "tables": (("dynamodb", "list-tables"), "TableNames", None),
    "vpcs": (("ec2", "describe-vpcs"), "Vpcs", "VpcId"),
    "subnets": (("ec2", "describe-subnets"), "Subnets", "SubnetId"),
    "security_groups": (("ec2", "describe-security-groups"), "SecurityGroups", "GroupId"),
    "endpoints": (("ec2", "describe-vpc-endpoints"), "VpcEndpoints", "VpcEndpointId"),
    "internet_gateways": (("ec2", "describe-internet-gateways"), "InternetGateways", "InternetGatewayId"),
    "route_tables": (("ec2", "describe-route-tables"), "RouteTables", "RouteTableId"),
    "addresses": (("ec2", "describe-addresses"), "Addresses", "AllocationId"),
    "volumes": (("ec2", "describe-volumes"), "Volumes", "VolumeId"),
    "launch_templates": (("ec2", "describe-launch-templates"), "LaunchTemplates", "LaunchTemplateId"),
    "key_pairs": (("ec2", "describe-key-pairs"), "KeyPairs", "KeyName"),
    "load_balancers": (("elbv2", "describe-load-balancers"), "LoadBalancers", "LoadBalancerArn"),
    "target_groups": (("elbv2", "describe-target-groups"), "TargetGroups", "TargetGroupArn"),
    "asgs": (("autoscaling", "describe-auto-scaling-groups"), "AutoScalingGroups", "AutoScalingGroupName"),
    "roles": (("iam", "list-roles"), "Roles", "RoleName"),
    "managed_policies": (("iam", "list-policies", "--scope", "Local"), "Policies", "Arn"),
    "instance_profiles": (("iam", "list-instance-profiles"), "InstanceProfiles", "InstanceProfileName"),
    "log_groups": (("logs", "describe-log-groups"), "logGroups", "logGroupName"),
    "kms_keys": (("kms", "list-keys"), "Keys", "KeyId"),
    "kms_aliases": (("kms", "list-aliases"), "Aliases", "AliasName"),
    "secrets": (("secretsmanager", "list-secrets", "--include-planned-deletion"), "SecretList", "ARN"),
    "parameters": (("ssm", "describe-parameters"), "Parameters", "Name"),
    "ssm_documents": (("ssm", "list-documents", "--filters", "Key=Owner,Values=Self"), "DocumentIdentifiers", "Name"),
    "certificates": (("acm", "list-certificates"), "CertificateSummaryList", "CertificateArn"),
    "trust_anchors": (("rolesanywhere", "list-trust-anchors"), "trustAnchors", "trustAnchorId"),
    "rolesanywhere_profiles": (("rolesanywhere", "list-profiles"), "profiles", "profileId"),
}


def identifiers(response, list_key, field):
    items = response.get(list_key) if isinstance(response, dict) else None
    if not isinstance(items, list):
        raise ValueError(f"missing resource list {list_key}")
    result = [item if field is None else item.get(field) if isinstance(item, dict) else None for item in items]
    if any(not isinstance(item, str) or not item for item in result) or len(result) != len(set(result)):
        raise ValueError(f"invalid or duplicate resource ID in {list_key}")
    return sorted(result)


def role_policy_snapshot(profile, region, roles, fetch):
    entries = []
    for role in sorted(roles):
        if not isinstance(role, str) or not re.fullmatch(r"[A-Za-z0-9+=,.@_-]{1,64}", role):
            raise ValueError("invalid IAM role name")
        inline = identifiers(fetch(profile, region, "iam", "list-role-policies", "--role-name", role), "PolicyNames", None)
        for name in inline:
            data = fetch(profile, region, "iam", "get-role-policy", "--role-name", role, "--policy-name", name)
            if "PolicyDocument" not in data:
                raise ValueError("missing inline policy document")
            digest = hashlib.sha256(json.dumps(data["PolicyDocument"], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
            entries.append(f"{role}|inline|{name}|{digest}")
        attached = fetch(profile, region, "iam", "list-attached-role-policies", "--role-name", role).get("AttachedPolicies")
        if not isinstance(attached, list):
            raise ValueError("invalid attached policies")
        for policy in attached:
            arn = policy.get("PolicyArn") if isinstance(policy, dict) else None
            if not isinstance(arn, str) or not arn:
                raise ValueError("missing managed policy ARN")
            detail = fetch(profile, region, "iam", "get-policy", "--policy-arn", arn).get("Policy", {})
            version = detail.get("DefaultVersionId")
            if not isinstance(version, str) or not version:
                raise ValueError("missing default policy version")
            version_data = fetch(profile, region, "iam", "get-policy-version", "--policy-arn", arn, "--version-id", version).get("PolicyVersion", {})
            if "Document" not in version_data:
                raise ValueError("missing managed policy document")
            digest = hashlib.sha256(json.dumps(version_data["Document"], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
            entries.append(f"{role}|managed|{arn}|{version}|{digest}")
    if len(entries) != len(set(entries)):
        raise ValueError("duplicate IAM policy identity")
    return sorted(entries)


def managed_policy_snapshot(profile, region, arns, fetch):
    entries = []
    for arn in sorted(arns):
        versions = fetch(profile, region, "iam", "list-policy-versions", "--policy-arn", arn).get("Versions")
        if not isinstance(versions, list) or not versions or sum(1 for item in versions if isinstance(item, dict) and item.get("IsDefaultVersion") is True) != 1:
            raise ValueError("invalid managed policy versions")
        for item in versions:
            version = item.get("VersionId") if isinstance(item, dict) else None
            is_default = item.get("IsDefaultVersion") if isinstance(item, dict) else None
            if not isinstance(version, str) or not version or not isinstance(is_default, bool):
                raise ValueError("invalid managed policy version")
            version_data = fetch(profile, region, "iam", "get-policy-version", "--policy-arn", arn, "--version-id", version).get("PolicyVersion", {})
            if "Document" not in version_data:
                raise ValueError("missing managed policy document")
            digest = hashlib.sha256(json.dumps(version_data["Document"], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
            entries.append(f"{arn}|{version}|{is_default}|{digest}")
    if len(entries) != len(set(entries)):
        raise ValueError("duplicate managed policy version")
    return sorted(entries)


def collect(profile, region, fetch=None, iam_roles=()):
    fetch = fetch or aws_json
    resources = {}
    for kind, (command, list_key, field) in SPECS.items():
        resources[kind] = identifiers(fetch(profile, region, *command), list_key, field)
    # AWS retains NAT gateways in 'deleted' state; only non-deleted states are residual.
    gateways = fetch(profile, region, "ec2", "describe-nat-gateways").get("NatGateways")
    if not isinstance(gateways, list) or any(not isinstance(g, dict) or not g.get("NatGatewayId") or not g.get("State") for g in gateways):
        raise ValueError("invalid NAT gateway response")
    resources["nat_gateways"] = sorted(g["NatGatewayId"] for g in gateways if g["State"] != "deleted")
    if len(resources["nat_gateways"]) != len(set(resources["nat_gateways"])):
        raise ValueError("duplicate NAT gateway ID")
    alarms = fetch(profile, region, "cloudwatch", "describe-alarms", "--alarm-types", "MetricAlarm", "CompositeAlarm")
    resources["metric_alarms"] = identifiers(alarms, "MetricAlarms", "AlarmName")
    resources["composite_alarms"] = identifiers(alarms, "CompositeAlarms", "AlarmName")
    resources["role_policies"] = role_policy_snapshot(profile, region, iam_roles, fetch)
    resources["managed_policy_versions"] = managed_policy_snapshot(profile, region, resources["managed_policies"], fetch)
    return resources


def compare_baseline(baseline, final):
    if not isinstance(baseline, dict) or not isinstance(final, dict) or set(baseline) != set(final):
        raise ValueError("account inventory types differ")
    added = {}
    for kind in baseline:
        old, new = baseline[kind], final[kind]
        if not isinstance(old, list) or not isinstance(new, list) or any(not isinstance(x, str) or not x for x in old + new) or len(old) != len(set(old)) or len(new) != len(set(new)):
            raise ValueError(f"invalid account inventory {kind}")
        extra = sorted(set(new) - set(old))
        if extra:
            added[kind] = extra
        if kind in ("role_policies", "managed_policy_versions"):
            removed = sorted(set(old) - set(new))
            if removed:
                added[f"{kind}_removed"] = removed
    return added


def validate_baseline(baseline_bytes, expected_sha256, run_id, account_id, region):
    if not isinstance(expected_sha256, str) or not hmac.compare_digest(hashlib.sha256(baseline_bytes).hexdigest(), expected_sha256):
        raise ValueError("account baseline hash mismatch")
    baseline = json.loads(baseline_bytes)
    if not isinstance(baseline, dict) or baseline.get("schema_version") != "miles-account-inventory/v1" or (baseline.get("run_id"), baseline.get("account_id"), baseline.get("region")) != (run_id, account_id, region):
        raise ValueError("account baseline identity mismatch")
    resources = baseline.get("resources")
    roles = baseline.get("iam_roles_to_audit")
    if not isinstance(roles, list) or not roles or any(not isinstance(role, str) or not re.fullmatch(r"[A-Za-z0-9+=,.@_-]{1,64}", role) for role in roles) or len(roles) != len(set(roles)):
        raise ValueError("missing or invalid IAM audit roles")
    if not isinstance(resources, dict) or set(resources) != set(SPECS) | {"nat_gateways", "metric_alarms", "composite_alarms", "role_policies", "managed_policy_versions"}:
        raise ValueError("account baseline scope is incomplete")
    compare_baseline(resources, resources)
    return resources


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("profile", "region", "account-id", "run-id", "output"):
        parser.add_argument("--" + name, required=True)
    parser.add_argument("--iam-role", action="append", required=True, help="Persistent role whose policies must be restored")
    args = parser.parse_args()
    if len(args.account_id) != 12 or not args.account_id.isdigit() or not args.run_id.strip():
        parser.error("12-digit account ID and run ID required")
    if aws_json(args.profile, args.region, "sts", "get-caller-identity").get("Account") != args.account_id:
        raise ValueError("AWS account mismatch")
    if len(args.iam_role) != len(set(args.iam_role)):
        raise ValueError("duplicate IAM audit roles")
    snapshot = {"schema_version": "miles-account-inventory/v1", "run_id": args.run_id, "account_id": args.account_id, "region": args.region, "captured_at_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(), "iam_roles_to_audit": sorted(args.iam_role), "resources": collect(args.profile, args.region, iam_roles=args.iam_role)}
    payload = (json.dumps(snapshot, sort_keys=True, indent=2) + "\n").encode()
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as output:
        output.write(payload)
        output.flush()
        os.fsync(output.fileno())
    print(hashlib.sha256(payload).hexdigest())


if __name__ == "__main__":
    main()
