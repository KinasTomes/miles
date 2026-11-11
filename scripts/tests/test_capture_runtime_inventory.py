import hashlib
import json
from pathlib import Path
import sys
import tempfile
import unittest
from unittest import mock

from scripts.capture_runtime_inventory import assemble_inventory, main, reconcile_snapshot, runtime_residuals, validate_volume_baseline, volume_ids


class RuntimeInventoryTests(unittest.TestCase):
    def setUp(self):
        self.groups = {"AutoScalingGroups": [{"AutoScalingGroupName": "asg", "Instances": [{"InstanceId": "i-1"}]}]}
        self.instances = {"Reservations": [{"Instances": [
            {"InstanceId": "i-1", "VpcId": "vpc-1", "BlockDeviceMappings": [{"Ebs": {"VolumeId": "vol-1"}}]},
            {"InstanceId": "i-2", "VpcId": "vpc-1", "BlockDeviceMappings": [{"Ebs": {"VolumeId": "vol-2"}}]},
        ]}]}

    def test_asg_and_additional_instance_volumes(self):
        result = assemble_inventory(self.groups, self.instances, ["asg"], ["i-2"], "vpc-1")
        self.assertEqual(result["instances"], ["i-1", "i-2"])
        self.assertEqual(result["volumes"], ["vol-1", "vol-2"])

    def test_partial_or_malformed_discovery_fails(self):
        with self.assertRaises(ValueError):
            assemble_inventory({"AutoScalingGroups": []}, self.instances, ["asg"], ["i-2"], "vpc-1")
        with self.assertRaises(ValueError):
            assemble_inventory(self.groups, {"Reservations": []}, ["asg"], ["i-2"], "vpc-1")
        with self.assertRaises(ValueError):
            assemble_inventory(self.groups, {"Reservations": [{"Instances": [{"InstanceId": "i-1", "VpcId": "vpc-1"}]}]}, ["asg"], [], "vpc-1")

    def test_snapshot_requires_asg_members_and_attached_volumes(self):
        resources = [{"type": "aws_vpc", "id": "vpc-1"}, {"type": "aws_autoscaling_group", "id": "asg"}, {"type": "aws_instance", "id": "i-2"}]
        snapshot = {"schema_version": "miles-runtime-inventory/v1", "run_id": "run", "account_id": "123456789012", "region": "ap-southeast-1", "captured_at_utc": "2026-11-10T00:00:00Z", **assemble_inventory(self.groups, self.instances, ["asg"], ["i-2"], "vpc-1")}
        snapshot.update(new_regional_volumes=["vol-1", "vol-2"], volume_baseline_sha256="a" * 64)
        self.assertEqual(reconcile_snapshot(snapshot, resources, snapshot, "run", "123456789012", "ap-southeast-1"), [])
        truncated = dict(snapshot, volumes=["vol-1"])
        self.assertIn("runtime_inventory_missing_volumes", reconcile_snapshot(snapshot, resources, truncated, "run", "123456789012", "ap-southeast-1"))
        self.assertIn("runtime_snapshot_identity_mismatch", reconcile_snapshot(snapshot, resources, snapshot, "other", "123456789012", "ap-southeast-1"))

    def test_regional_volume_baseline_catches_detached_new_volumes(self):
        baseline = json.dumps({"schema_version": "miles-volume-baseline/v1", "run_id": "run", "account_id": "123456789012", "region": "ap-southeast-1", "captured_at_utc": "2026-11-10T00:00:00Z", "volumes": ["vol-old"]}).encode()
        digest = hashlib.sha256(baseline).hexdigest()
        previous = validate_volume_baseline(baseline, digest, "run", "123456789012", "ap-southeast-1")
        current = volume_ids({"Volumes": [{"VolumeId": "vol-old"}, {"VolumeId": "vol-detached"}]})
        self.assertEqual(current - previous, {"vol-detached"})
        with self.assertRaises(ValueError):
            validate_volume_baseline(baseline, "0" * 64, "run", "123456789012", "ap-southeast-1")
        with self.assertRaises(ValueError):
            volume_ids({"Volumes": [{"VolumeId": "vol-1"}, {"VolumeId": "vol-1"}]})

    def test_post_teardown_catches_replacement_and_orphan_volume(self):
        instances = {"Reservations": [{"Instances": [{"InstanceId": "i-new", "VpcId": "vpc-1", "State": {"Name": "running"}}]}]}
        volumes = {"Volumes": [{"VolumeId": "vol-old"}, {"VolumeId": "vol-orphan"}]}
        residuals = runtime_residuals(instances, volumes, {"vol-old"}, "vpc-1")
        self.assertEqual(residuals, {"instances": ["i-new"], "new_regional_volumes": ["vol-orphan"]})
        instances["Reservations"][0]["Instances"][0]["State"]["Name"] = "terminated"
        self.assertEqual(runtime_residuals(instances, {"Volumes": [{"VolumeId": "vol-old"}]}, {"vol-old"}, "vpc-1"), {"instances": [], "new_regional_volumes": []})

    def test_capture_cli_freezes_vpc_and_new_regional_volume(self):
        with tempfile.TemporaryDirectory() as directory:
            baseline = json.dumps({"schema_version": "miles-volume-baseline/v1", "run_id": "run", "account_id": "123456789012", "region": "ap-southeast-1", "captured_at_utc": "2026-11-10T00:00:00Z", "volumes": ["vol-old"]}).encode()
            baseline_file = Path(directory) / "baseline.json"
            baseline_file.write_bytes(baseline)
            output = Path(directory) / "runtime.json"
            arguments = ["capture", "--profile", "test", "--region", "ap-southeast-1", "--account-id", "123456789012", "--run-id", "run", "--vpc-id", "vpc-1", "--asg", "asg", "--instance", "i-2", "--volume-baseline", str(baseline_file), "--volume-baseline-sha256", hashlib.sha256(baseline).hexdigest(), "--output", str(output)]
            replies = [{"Account": "123456789012"}, self.groups, self.instances, {"Volumes": [{"VolumeId": "vol-old"}, {"VolumeId": "vol-orphan"}, {"VolumeId": "vol-1"}, {"VolumeId": "vol-2"}]}]
            with mock.patch.object(sys, "argv", arguments), mock.patch("scripts.capture_runtime_inventory.aws_json", side_effect=replies), mock.patch("builtins.print"):
                main()
            snapshot = json.loads(output.read_text())
            self.assertEqual(snapshot["new_regional_volumes"], ["vol-1", "vol-2", "vol-orphan"])
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)


if __name__ == "__main__":
    unittest.main()
