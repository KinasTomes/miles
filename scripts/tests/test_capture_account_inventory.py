import unittest
from pathlib import Path
import re
from unittest import mock

from scripts.capture_account_inventory import INVENTORY_COVERAGE, SPECS, collect, compare_baseline, identifiers, managed_policy_snapshot, role_policy_snapshot
from scripts.freeze_terraform_inventory import CHILD_PARENTS, STATE_TYPES
from scripts.verify_teardown_absence import SUPPORTED_RESOURCE_TYPES


class AccountInventoryTests(unittest.TestCase):
    def test_every_dev_terraform_type_has_state_and_native_coverage(self):
        root = Path(__file__).resolve().parents[2] / "infra/terraform/cloud"
        paths = list((root / "modules").rglob("*.tf")) + list((root / "environments/dev").rglob("*.tf"))
        kinds = set()
        for path in paths:
            kinds.update(re.findall(r'resource\s+"(aws_[a-z_]+)"', path.read_text()))
        self.assertFalse(kinds - set(STATE_TYPES) - set(CHILD_PARENTS))
        self.assertFalse(set(STATE_TYPES.values()) - set(INVENTORY_COVERAGE))
        self.assertFalse(SUPPORTED_RESOURCE_TYPES - set(INVENTORY_COVERAGE))
        self.assertFalse(set(INVENTORY_COVERAGE.values()) - set(SPECS) - {"nat_gateways", "metric_alarms", "vpc_instances"})
    def test_native_inventory_fails_closed_on_incomplete_response(self):
        with self.assertRaises(ValueError):
            identifiers({}, "Volumes", "VolumeId")
        with self.assertRaises(ValueError):
            identifiers({"Volumes": [{"VolumeId": "v"}, {"VolumeId": "v"}]}, "Volumes", "VolumeId")

    def test_collect_and_residuals(self):
        by_command = {command: {list_key: []} for command, list_key, _ in SPECS.values()}
        by_command[("ec2", "describe-nat-gateways")] = {"NatGateways": [{"NatGatewayId": "nat-deleting", "State": "deleting"}, {"NatGatewayId": "nat-deleted", "State": "deleted"}]}
        by_command[("cloudwatch", "describe-alarms", "--alarm-types", "MetricAlarm", "CompositeAlarm")] = {"MetricAlarms": [], "CompositeAlarms": []}
        with mock.patch("scripts.capture_account_inventory.aws_json", side_effect=lambda profile, region, *command: by_command[command]):
            inventory = collect("profile", "region")
        self.assertEqual(inventory["nat_gateways"], ["nat-deleting"])
        baseline = {key: [] for key in inventory}
        self.assertEqual(compare_baseline(baseline, inventory), {"nat_gateways": ["nat-deleting"]})
        with self.assertRaises(ValueError):
            compare_baseline({}, inventory)

    def test_role_policy_changes_and_removal_are_detected(self):
        def fetch(profile, region, service, operation, *args):
            if operation == "get-role":
                return {"Role": {"RoleName": "deploy", "RoleId": "deploy-id", "AssumeRolePolicyDocument": {"Statement": []}}}
            if operation == "list-role-policies":
                return {"PolicyNames": ["temporary"]}
            if operation == "get-role-policy":
                return {"PolicyDocument": {"Version": "2012-10-17", "Statement": []}}
            if operation == "list-attached-role-policies":
                return {"AttachedPolicies": []}
            self.fail(f"unexpected operation {operation}")
        entries = role_policy_snapshot("p", "r", ["deploy"], fetch)
        self.assertEqual(len(entries), 2)
        self.assertEqual(compare_baseline({"role_policies": []}, {"role_policies": entries})["role_policies"], entries)
        self.assertEqual(compare_baseline({"role_policies": entries}, {"role_policies": []})["role_policies_removed"], entries)

    def test_baseline_preservation_detects_missing_resources(self):
        baseline = {"buckets": ["existing-bucket"], "roles": ["existing-role"], "vpcs": ["vpc-existing"]}
        final = {"buckets": [], "roles": [], "vpcs": ["vpc-new"]}
        self.assertEqual(compare_baseline(baseline, final), {
            "buckets_removed": ["existing-bucket"], "roles_removed": ["existing-role"],
            "vpcs_removed": ["vpc-existing"], "vpcs": ["vpc-new"],
        })

    def test_all_managed_policy_versions_are_audited(self):
        def fetch(profile, region, service, operation, *args):
            if operation == "list-policy-versions":
                return {"Versions": [{"VersionId": "v1", "IsDefaultVersion": True}, {"VersionId": "v2", "IsDefaultVersion": False}]}
            return {"PolicyVersion": {"Document": {"version": args[-1]}}}
        entries = managed_policy_snapshot("p", "r", ["arn:policy"], fetch)
        self.assertEqual(len(entries), 2)
        self.assertIn("managed_policy_versions", compare_baseline({"managed_policy_versions": []}, {"managed_policy_versions": entries}))


if __name__ == "__main__":
    unittest.main()
