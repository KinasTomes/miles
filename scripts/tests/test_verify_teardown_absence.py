"""
Unit and regression tests for Gate E AWS Teardown Absence Verifier (scripts/verify_teardown_absence.py).
Includes regression tests for:
- [P1] Empty / unverified inventory fail-closed (returns UNVERIFIED / all_absent=False).
- [P1] Full extraction and dynamic absence checking of subnets, security groups, VPC endpoints,
  instance profiles, trust anchors, rolesanywhere profiles, instances, volumes, target groups, ASGs, SSM parameters.
- [P2] CloudWatch describe-alarms returncode 0 with empty lists correctly classified as ABSENT (not leak).
- [P2] Gate E evidence ledger private file permissions (0600) and explicit mode ("dry-run" vs "live").
"""

import json
import os
from pathlib import Path
import tempfile
import unittest

from scripts.verify_teardown_absence import (
    EXPECTED_ABSENCE_ERRORS,
    TeardownAbsenceVerifier,
    main,
)


class VerifyTeardownAbsenceTests(unittest.TestCase):

    def setUp(self):
        self.tmpdir = tempfile.TemporaryDirectory()
        self.tmppath = Path(self.tmpdir.name)

    def tearDown(self):
        self.tmpdir.cleanup()

    def test_extract_inventory_from_comprehensive_outputs(self):
        """Verifies parsing of comprehensive Terraform output format covering all owned resource types."""
        sample_outputs = {
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                    "public_subnets": {"ap-southeast-1a": "subnet-pub1", "ap-southeast-1b": "subnet-pub2"},
                    "private_subnets": {"ap-southeast-1a": "subnet-priv1", "ap-southeast-1b": "subnet-priv2"},
                    "security_groups": {"runtime": "sg-111", "lb": "sg-222"},
                    "gateway_endpoints": {"s3": "vpce-gw-s3"},
                    "interface_endpoints": {"ec2": "vpce-if-ec2"},
                    "alb_name": "miles-dev-alb",
                }
            },
            "runtime_inventory": {
                "value": {
                    "expected_tables": ["miles-dev-records", "miles-dev-leases"],
                    "data_bucket_name": "miles-dev-data-12345",
                    "kms_key_arn": "arn:aws:kms:ap-southeast-1:123456789012:key/test-key-id",
                    "secret_metadata_arn": "arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:test-secret",
                    "bootstrap_parameter_arn": "arn:aws:ssm:ap-southeast-1:123456789012:parameter/miles/dev/config",
                    "target_group_arn": "arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:targetgroup/tg-1/123",
                    "iam_roles": {
                        "worker": "arn:aws:iam::123456789012:role/miles-dev-worker",
                        "controller": "arn:aws:iam::123456789012:role/miles-dev-controller",
                    },
                    "instance_profiles": {
                        "worker": "miles-dev-worker-profile",
                    },
                    "compute": {
                        "instance_id": "i-0123456789abcdef0",
                        "data_volume_id": "vol-0123456789abcdef0",
                        "session_document": "miles-shell-doc",
                    },
                }
            },
            "edge_provider": {
                "value": {
                    "role_name": "miles-edge-role",
                    "trust_anchor_arn": "arn:aws:rolesanywhere:ap-southeast-1:123456789012:trust-anchor/ta-anchor-1",
                    "profile_arn": "arn:aws:rolesanywhere:ap-southeast-1:123456789012:profile/prof-edge-1",
                }
            },
            "fleet_contract": {
                "value": {
                    "asg_name": "miles-gateway-asg",
                }
            },
            "alarm_contract": {
                "value": {
                    "miles-recovery-cut-age": {},
                    "miles-integrity": {},
                }
            },
            "data_bucket_name": {
                "value": "miles-dev-data-12345"  # duplicate should be de-duped
            },
        }

        inv = TeardownAbsenceVerifier.extract_inventory_from_outputs(sample_outputs)
        self.assertEqual(inv["vpc_id"], "vpc-0123456789abcdef0")
        self.assertCountEqual(inv["subnets"], ["subnet-pub1", "subnet-pub2", "subnet-priv1", "subnet-priv2"])
        self.assertCountEqual(inv["security_groups"], ["sg-111", "sg-222"])
        self.assertCountEqual(inv["vpc_endpoints"], ["vpce-gw-s3", "vpce-if-ec2"])
        self.assertEqual(inv["alb_names"], ["miles-dev-alb"])
        self.assertEqual(inv["target_groups"], ["arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:targetgroup/tg-1/123"])
        self.assertCountEqual(inv["iam_roles"], ["miles-dev-worker", "miles-dev-controller", "miles-edge-role"])
        self.assertEqual(inv["instance_profiles"], ["miles-dev-worker-profile"])
        self.assertEqual(inv["trust_anchors"], ["ta-anchor-1"])
        self.assertEqual(inv["rolesanywhere_profiles"], ["prof-edge-1"])
        self.assertEqual(inv["instances"], ["i-0123456789abcdef0"])
        self.assertEqual(inv["volumes"], ["vol-0123456789abcdef0"])
        self.assertEqual(inv["asg_names"], ["miles-gateway-asg"])
        self.assertEqual(inv["ssm_parameters"], ["arn:aws:ssm:ap-southeast-1:123456789012:parameter/miles/dev/config"])
        self.assertEqual(inv["ssm_documents"], ["miles-shell-doc"])
        self.assertEqual(inv["dynamodb_tables"], ["miles-dev-records", "miles-dev-leases"])
        self.assertEqual(inv["s3_buckets"], ["miles-dev-data-12345"])
        self.assertEqual(inv["kms_keys"], ["arn:aws:kms:ap-southeast-1:123456789012:key/test-key-id"])
        self.assertEqual(inv["secrets"], ["arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:test-secret"])
        self.assertCountEqual(inv["cloudwatch_alarms"], ["miles-recovery-cut-age", "miles-integrity"])

    def test_fail_closed_on_empty_inventory(self):
        """[P1 Regression] Empty inventory must fail-closed with status UNVERIFIED and all_absent=False."""
        empty_inv = {}
        verifier = TeardownAbsenceVerifier(inventory=empty_inv)
        summary = verifier.verify_all()

        self.assertFalse(summary["all_absent"])
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertEqual(summary["error"], "EMPTY_OR_UNVERIFIED_INVENTORY")
        self.assertEqual(summary["total_checked"], 0)

    def test_cloudwatch_alarm_describe_empty_list_is_absent(self):
        """[P2 Regression] CloudWatch describe-alarms returning 0 with empty lists is ABSENT, not LEAK."""
        # 1. Empty alarms returned (alarm deleted) -> ABSENT
        def mock_empty_alarms_runner(cmd):
            return 0, json.dumps({"MetricAlarms": [], "CompositeAlarms": []}), ""

        v_empty = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_empty_alarms_runner)
        r_empty = v_empty.check_cloudwatch_alarm_absent("miles-dev-recovery-cut-age")
        self.assertEqual(r_empty["status"], "ABSENT")
        self.assertIn("EMPTY_OR_UNMATCHED_ALARMS_LIST", r_empty["reason"])

        # 2. Alarms returned and matching alarm present -> STILL_PRESENT leak
        def mock_present_alarms_runner(cmd):
            return 0, json.dumps({
                "MetricAlarms": [{"AlarmName": "miles-dev-recovery-cut-age"}],
                "CompositeAlarms": [],
            }), ""

        v_present = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_present_alarms_runner)
        r_present = v_present.check_cloudwatch_alarm_absent("miles-dev-recovery-cut-age")
        self.assertEqual(r_present["status"], "STILL_PRESENT")
        self.assertEqual(r_present["reason"], "RESOURCE_EXISTS_LEAK")

        # 3. AWS error returned (ResourceNotFoundException) -> ABSENT
        def mock_error_absent_runner(cmd):
            return 255, "", "ResourceNotFound: The specified alarm does not exist."

        v_err = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_error_absent_runner)
        r_err = v_err.check_cloudwatch_alarm_absent("miles-dev-recovery-cut-age")
        self.assertEqual(r_err["status"], "ABSENT")

    def test_asg_and_ec2_instance_volume_absence_checks(self):
        """Verifies ASG empty list, EC2 instance terminated, and EBS volume deleted states."""
        # ASG absent (empty AutoScalingGroups)
        def mock_asg_empty(cmd):
            return 0, json.dumps({"AutoScalingGroups": []}), ""

        v_asg = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_asg_empty)
        r_asg = v_asg.check_asg_absent("miles-asg")
        self.assertEqual(r_asg["status"], "ABSENT")

        # ASG present
        def mock_asg_present(cmd):
            return 0, json.dumps({"AutoScalingGroups": [{"AutoScalingGroupName": "miles-asg"}]}), ""

        v_asg_p = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_asg_present)
        r_asg_p = v_asg_p.check_asg_absent("miles-asg")
        self.assertEqual(r_asg_p["status"], "STILL_PRESENT")

        # EC2 instance terminated -> ABSENT
        def mock_inst_terminated(cmd):
            return 0, json.dumps({"Reservations": [{"Instances": [{"InstanceId": "i-123", "State": {"Name": "terminated"}}]}]}), ""

        v_inst = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_inst_terminated)
        r_inst = v_inst.check_instance_absent("i-123")
        self.assertEqual(r_inst["status"], "ABSENT")
        self.assertEqual(r_inst["reason"], "INSTANCE_TERMINATED")

        # EC2 instance running -> STILL_PRESENT leak
        def mock_inst_running(cmd):
            return 0, json.dumps({"Reservations": [{"Instances": [{"InstanceId": "i-123", "State": {"Name": "running"}}]}]}), ""

        v_inst_r = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_inst_running)
        r_inst_r = v_inst_r.check_instance_absent("i-123")
        self.assertEqual(r_inst_r["status"], "STILL_PRESENT")

        # EBS volume deleted -> ABSENT
        def mock_vol_deleted(cmd):
            return 0, json.dumps({"Volumes": [{"VolumeId": "vol-123", "State": "deleted"}]}), ""

        v_vol = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_vol_deleted)
        r_vol = v_vol.check_volume_absent("vol-123")
        self.assertEqual(r_vol["status"], "ABSENT")

    def test_ledger_export_file_mode_and_permissions(self):
        """[P2 Regression] Verifies ledger file permissions 0600 and explicit execution mode recorded."""
        ledger_file = self.tmppath / "gate-e-test-ledger.json"

        inventory = {
            "dynamodb_tables": ["tbl1"],
        }

        def mock_runner(cmd):
            return 255, "", "ResourceNotFoundException"

        verifier = TeardownAbsenceVerifier(
            inventory=inventory,
            mode="dry-run",
            cli_runner=mock_runner,
        )

        verifier.export_summary(ledger_file)
        self.assertTrue(ledger_file.exists())

        # Check permissions: 0600
        mode = ledger_file.stat().st_mode & 0o777
        self.assertEqual(mode, 0o600)

        # Check explicit mode
        data = json.loads(ledger_file.read_text(encoding="utf-8"))
        self.assertEqual(data["mode"], "dry-run")
        self.assertEqual(data["status"], "PASS")
        self.assertTrue(data["all_absent"])

    def test_verify_all_comprehensive_clean_teardown(self):
        """Verifies verify_all returns PASS and all_absent=True across all resource types."""
        inventory = {
            "vpc_id": "vpc-1",
            "subnets": ["subnet-1"],
            "security_groups": ["sg-1"],
            "vpc_endpoints": ["vpce-1"],
            "alb_names": ["alb-1"],
            "target_groups": ["tg-1"],
            "iam_roles": ["role-1"],
            "instance_profiles": ["prof-1"],
            "trust_anchors": ["ta-1"],
            "rolesanywhere_profiles": ["rap-1"],
            "instances": ["i-1"],
            "volumes": ["vol-1"],
            "asg_names": ["asg-1"],
            "ssm_parameters": ["param-1"],
            "dynamodb_tables": ["table1"],
            "s3_buckets": ["bucket1"],
            "acm_certificates": ["cert1"],
            "ssm_documents": ["doc1"],
            "key_pairs": ["kp1"],
            "cloudwatch_alarms": ["alarm1"],
            "kms_keys": ["kms1"],
            "secrets": ["secret1"],
        }

        def mock_clean_runner(cmd):
            if "describe-key" in cmd:
                return 0, json.dumps({"State": "PendingDeletion", "DeletionDate": "2026-12-10T00:00:00Z"}), ""
            if "describe-secret" in cmd:
                return 0, json.dumps({"DeletedDate": "2026-12-10T00:00:00Z"}), ""
            if "describe-alarms" in cmd:
                return 0, json.dumps({"MetricAlarms": [], "CompositeAlarms": []}), ""
            if "describe-auto-scaling-groups" in cmd:
                return 0, json.dumps({"AutoScalingGroups": []}), ""
            if "describe-instances" in cmd:
                return 0, json.dumps({"Reservations": [{"Instances": [{"InstanceId": "i-1", "State": {"Name": "terminated"}}]}]}), ""
            if "describe-volumes" in cmd:
                return 0, json.dumps({"Volumes": [{"VolumeId": "vol-1", "State": "deleted"}]}), ""
            # All others return expected not found
            return 255, "", "ResourceNotFoundException 404 Not Found LoadBalancerNotFound TargetGroupNotFound InvalidVpcID.NotFound InvalidSubnetID.NotFound InvalidGroup.NotFound InvalidVpcEndpointId.NotFound NoSuchEntity InvalidDocument InvalidKeyPair.NotFound ParameterNotFound"

        verifier = TeardownAbsenceVerifier(inventory=inventory, cli_runner=mock_clean_runner)
        summary = verifier.verify_all()
        self.assertTrue(summary["all_absent"])
        self.assertEqual(summary["status"], "PASS")
        self.assertEqual(summary["still_present_count"], 0)
        self.assertEqual(summary["unexpected_error_count"], 0)
        self.assertEqual(summary["total_checked"], 22)
        self.assertEqual(summary["pending_deletion_count"], 2)
        self.assertEqual(summary["absent_count"], 20)

    def test_cli_dry_run_with_outputs_file(self):
        """Verifies CLI execution with --dry-run and --outputs-json."""
        outputs_file = self.tmppath / "outputs.json"
        ledger_file = self.tmppath / "ledger.json"

        outputs_file.write_text(json.dumps({
            "runtime_inventory": {
                "value": {
                    "expected_tables": ["test-tab"],
                    "data_bucket_name": "test-bkt",
                    "kms_key_arn": "test-kms",
                    "secret_metadata_arn": "test-sec",
                }
            }
        }))

        import sys
        old_argv = sys.argv
        try:
            sys.argv = [
                "verify_teardown_absence.py",
                "--outputs-json", str(outputs_file),
                "--output-ledger", str(ledger_file),
                "--dry-run",
            ]
            rc = main()
            self.assertEqual(rc, 0)
            self.assertTrue(ledger_file.exists())
            ledger_data = json.loads(ledger_file.read_text(encoding="utf-8"))
            self.assertTrue(ledger_data["all_absent"])
            self.assertEqual(ledger_data["mode"], "dry-run")
            self.assertEqual(ledger_data["total_checked"], 4)
        finally:
            sys.argv = old_argv


if __name__ == "__main__":
    unittest.main()
