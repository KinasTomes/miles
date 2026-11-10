import unittest
from unittest import mock

from scripts.capture_account_inventory import SPECS, collect, compare_baseline, identifiers, managed_policy_snapshot, role_policy_snapshot


class AccountInventoryTests(unittest.TestCase):
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
            if operation == "list-role-policies":
                return {"PolicyNames": ["temporary"]}
            if operation == "get-role-policy":
                return {"PolicyDocument": {"Version": "2012-10-17", "Statement": []}}
            if operation == "list-attached-role-policies":
                return {"AttachedPolicies": []}
            self.fail(f"unexpected operation {operation}")
        entries = role_policy_snapshot("p", "r", ["deploy"], fetch)
        self.assertEqual(len(entries), 1)
        self.assertEqual(compare_baseline({"role_policies": []}, {"role_policies": entries})["role_policies"], entries)
        self.assertEqual(compare_baseline({"role_policies": entries}, {"role_policies": []})["role_policies_removed"], entries)

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
