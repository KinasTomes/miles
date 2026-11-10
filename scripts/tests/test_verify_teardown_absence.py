"""
Unit and regression tests for Gate E AWS Teardown Absence Verifier (scripts/verify_teardown_absence.py).
Includes regression tests for:
- [P1] Full extraction and dynamic absence checking matching real Terraform schema
  (extracting exact ALB name and full AWS alarm names with ${var.name}-${key} prefix).
- [P1] Catching ALB and CloudWatch alarm leaks when queried against exact native names.
- [P1] Fail-closed when expected deployed resources (e.g. ALB, alarms) are omitted from inventory.
- [P1] Empty / unverified inventory fail-closed (returns UNVERIFIED / all_absent=False).
- [P2] CloudWatch describe-alarms returncode 0 with empty lists correctly classified as ABSENT (not leak).
- [P2] Gate E evidence ledger private file permissions (0600) enforced both on creation AND overwrite of existing 0664 files.
"""

import json
import os
from pathlib import Path
import tempfile
import unittest
import unittest.mock

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

    def test_extract_inventory_from_real_terraform_schema(self):
        """
        Verifies parsing matching real Terraform outputs (dev/outputs.tf).
        Extracts exact ALB name and exact full AWS alarm names (${var.name}-${key}).
        """
        real_tf_outputs = {
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                    "public_subnets": {"ap-southeast-1a": "subnet-pub1", "ap-southeast-1b": "subnet-pub2"},
                    "private_subnets": {"ap-southeast-1a": "subnet-priv1", "ap-southeast-1b": "subnet-priv2"},
                    "security_groups": {"runtime": "sg-111", "lb": "sg-222"},
                    "gateway_endpoints": {"s3": "vpce-gw-s3"},
                    "interface_endpoints": {"ec2": "vpce-if-ec2"},
                }
            },
            "runtime_inventory": {
                "value": {
                    "expected_tables": ["miles-dev-records", "miles-dev-leases"],
                    "data_bucket_name": "miles-dev-data-12345",
                    "kms_key_arn": "arn:aws:kms:ap-southeast-1:123456789012:key/test-key-id",
                    "secret_metadata_arn": "arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:test-secret",
                    "bootstrap_parameter_arn": "arn:aws:ssm:ap-southeast-1:123456789012:parameter/miles/dev/config",
                    "alb_dns": "miles-dev-123456.ap-southeast-1.elb.amazonaws.com",
                    "alb_name": "miles-dev",
                    "target_group_arn": "arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:targetgroup/miles-dev-s3/0123456789abcdef",
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
                    "cloud-recovery_cut_age": {
                        "metric": "miles_recovery_cut_age_seconds",
                        "dimensions": {"profile": "cloud", "installation": "miles-dev-"},
                        "alarm_name": "miles-dev-cloud-recovery_cut_age",
                    },
                    "edge-integrity": {
                        "metric": "miles_warm_read_integrity_failures_total",
                        "dimensions": {"profile": "edge", "installation": "miles-dev-"},
                    },
                }
            },
            "session_configuration": {
                "value": {
                    "enable_alb": True,
                    "enable_compute": True,
                }
            },
        }

        inv = TeardownAbsenceVerifier.extract_inventory_from_outputs(real_tf_outputs)
        self.assertEqual(inv["vpc_id"], "vpc-0123456789abcdef0")
        self.assertIn("miles-dev", inv["alb_names"])
        self.assertIn("arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:targetgroup/miles-dev-s3/0123456789abcdef", inv["target_groups"])
        self.assertIn("miles-dev-cloud-recovery_cut_age", inv["cloudwatch_alarms"])
        self.assertIn("miles-dev-edge-integrity", inv["cloudwatch_alarms"])
        self.assertEqual(len(inv["expected_missing"]), 0)

    def test_fail_closed_when_expected_resource_omitted_from_inventory(self):
        """[P1 Regression] When ALB was enabled but alb_names omitted, verifier must fail closed."""
        incomplete_outputs = {
            "runtime_inventory": {
                "value": {
                    "expected_tables": ["miles-dev-records"],
                    "data_bucket_name": "miles-dev-data",
                    # target_group_arn present implies ALB was deployed, but alb_names is missing
                    "target_group_arn": "arn:aws:elasticloadbalancing:ap-southeast-1:123456789012:targetgroup/other-tg/123",
                }
            },
            "session_configuration": {
                "value": {
                    "enable_alb": True,
                }
            }
        }
        inv = TeardownAbsenceVerifier.extract_inventory_from_outputs(incomplete_outputs)
        self.assertIn("alb", inv["expected_missing"])

        verifier = TeardownAbsenceVerifier(inventory=inv)
        summary = verifier.verify_all()
        self.assertFalse(summary["all_absent"])
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertEqual(summary["error"], "INCOMPLETE_INVENTORY_MISSING_EXPECTED_RESOURCES")

    def test_alb_and_real_alarm_leak_detection(self):
        """[P1 Regression] Verifies that an existing ALB and existing real alarm are caught as leaks."""
        inventory = {
            "alb_names": ["miles-dev"],
            "cloudwatch_alarms": ["miles-dev-cloud-recovery_cut_age"],
            "dynamodb_tables": ["miles-dev-records"],
        }

        # Runner returns ALB and Alarm still present on AWS
        def mock_leak_runner(cmd):
            if "describe-load-balancers" in cmd:
                return 0, json.dumps({"LoadBalancers": [{"LoadBalancerName": "miles-dev"}]}), ""
            if "describe-alarms" in cmd:
                return 0, json.dumps({
                    "MetricAlarms": [{"AlarmName": "miles-dev-cloud-recovery_cut_age"}],
                    "CompositeAlarms": [],
                }), ""
            return 255, "", "ResourceNotFoundException"

        verifier = TeardownAbsenceVerifier(inventory=inventory, cli_runner=mock_leak_runner)
        summary = verifier.verify_all()
        self.assertFalse(summary["all_absent"])
        self.assertEqual(summary["status"], "FAIL")
        self.assertIn("miles-dev", summary["still_present"])
        self.assertIn("miles-dev-cloud-recovery_cut_age", summary["still_present"])

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
        def mock_empty_alarms_runner(cmd):
            return 0, json.dumps({"MetricAlarms": [], "CompositeAlarms": []}), ""

        v_empty = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_empty_alarms_runner)
        r_empty = v_empty.check_cloudwatch_alarm_absent("miles-dev-cloud-recovery_cut_age")
        self.assertEqual(r_empty["status"], "ABSENT")
        self.assertIn("EMPTY_OR_UNMATCHED_ALARMS_LIST", r_empty["reason"])

        def mock_present_alarms_runner(cmd):
            return 0, json.dumps({
                "MetricAlarms": [{"AlarmName": "miles-dev-cloud-recovery_cut_age"}],
                "CompositeAlarms": [],
            }), ""

        v_present = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_present_alarms_runner)
        r_present = v_present.check_cloudwatch_alarm_absent("miles-dev-cloud-recovery_cut_age")
        self.assertEqual(r_present["status"], "STILL_PRESENT")
        self.assertEqual(r_present["reason"], "RESOURCE_EXISTS_LEAK")

    def test_ledger_export_file_mode_and_permissions_on_existing_file(self):
        """[P2 Regression] Verifies ledger file permissions 0600 are strictly enforced even on pre-existing 0664 files."""
        ledger_file = self.tmppath / "gate-e-test-ledger.json"

        # Pre-create file with loose permissions (0664)
        ledger_file.write_text("pre-existing content", encoding="utf-8")
        os.chmod(str(ledger_file), 0o664)
        self.assertEqual(ledger_file.stat().st_mode & 0o777, 0o664)

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

        # Check permissions: strictly enforced to 0600 on the overwritten file
        mode = ledger_file.stat().st_mode & 0o777
        self.assertEqual(mode, 0o600)

        # Check explicit mode
        data = json.loads(ledger_file.read_text(encoding="utf-8"))
        self.assertEqual(data["mode"], "dry-run")
        self.assertEqual(data["status"], "PASS")
        self.assertTrue(data["all_absent"])

    def test_partial_outputs_missing_essential_resources_fails_closed(self):
        """
        [P1 Regression] Verifies that partial Terraform outputs (e.g. VPC only, missing DDB/S3/IAM/alarms)
        fails closed with UNVERIFIED, preventing false State 0 confirmation.
        """
        partial_outputs = {
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                }
            },
            "session_configuration": {
                "value": {
                    "enable_alb": False,
                    "enable_compute": False,
                }
            },
        }

        inv = TeardownAbsenceVerifier.extract_inventory_from_outputs(partial_outputs)
        self.assertIn("missing_root_output:runtime_inventory", inv["expected_missing"])
        self.assertIn("missing_essential_resource:dynamodb_tables", inv["expected_missing"])
        self.assertIn("missing_essential_resource:s3_buckets", inv["expected_missing"])
        self.assertIn("missing_essential_resource:iam_roles", inv["expected_missing"])
        self.assertIn("missing_essential_resource:cloudwatch_alarms", inv["expected_missing"])

        # Even if mock runner reports NotFound for the VPC, summary must fail closed
        def mock_vpc_absent_runner(cmd):
            return 255, "", "InvalidVpcID.NotFound: The vpc ID does not exist"

        outputs_file = self.tmppath / "partial-outputs.json"
        outputs_file.write_text(json.dumps(partial_outputs), encoding="utf-8")

        verifier = TeardownAbsenceVerifier.from_terraform_outputs(
            outputs_file,
            cli_runner=mock_vpc_absent_runner,
        )
        summary = verifier.verify_all()

        self.assertFalse(summary["all_absent"])
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertEqual(summary["error"], "INCOMPLETE_INVENTORY_MISSING_EXPECTED_RESOURCES")
        self.assertIn("missing_essential_resource:dynamodb_tables", summary["missing_expected_types"])

    def test_authoritative_manifest_validation_fail_closed_on_missing_resource(self):
        """
        [P1 Regression] Verifies pre-teardown authoritative manifest enforcement:
        missing manifest resources cause UNVERIFIED, complete manifest with all absent passes,
        and living resources are correctly detected as leaks.
        """
        manifest = {
            "run_id": "run-20261110-edge-01",
            "expected_resources": {
                "vpc_id": "vpc-0123456789abcdef0",
                "dynamodb_tables": ["miles-dev-records"],
                "s3_buckets": ["miles-dev-data"],
                "iam_roles": ["miles-dev-worker"],
                "cloudwatch_alarms": ["miles-dev-alarm"],
            },
            "expected_types": ["vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms"],
        }
        manifest_file = self.tmppath / "pre-teardown-manifest.json"
        manifest_file.write_text(json.dumps(manifest), encoding="utf-8")

        # 1. Partial outputs missing DDB/S3/IAM/alarm fails closed against manifest
        partial_outputs = {
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                }
            }
        }
        outputs_file = self.tmppath / "partial-tf.json"
        outputs_file.write_text(json.dumps(partial_outputs), encoding="utf-8")

        verifier_incomplete = TeardownAbsenceVerifier.from_terraform_outputs(
            outputs_file,
            manifest_path=manifest_file,
            cli_runner=lambda cmd: (255, "", "NotFound"),
        )
        summary_incomplete = verifier_incomplete.verify_all()
        self.assertFalse(summary_incomplete["all_absent"])
        self.assertEqual(summary_incomplete["status"], "UNVERIFIED")
        self.assertEqual(summary_incomplete["error"], "INCOMPLETE_INVENTORY_MISSING_EXPECTED_RESOURCES")

        # 2. Complete inventory matching manifest with all native checks reporting absent returns PASS
        complete_inventory = {
            "vpc_id": "vpc-0123456789abcdef0",
            "dynamodb_tables": ["miles-dev-records"],
            "s3_buckets": ["miles-dev-data"],
            "iam_roles": ["miles-dev-worker"],
            "cloudwatch_alarms": ["miles-dev-alarm"],
        }

        def mock_all_absent_runner(cmd):
            if "describe-alarms" in cmd:
                return 0, json.dumps({"MetricAlarms": [], "CompositeAlarms": []}), ""
            return 255, "", "ResourceNotFoundException NoSuchBucket NoSuchEntity InvalidVpcID.NotFound"

        verifier_clean = TeardownAbsenceVerifier(
            inventory=complete_inventory,
            manifest=manifest_file,
            cli_runner=mock_all_absent_runner,
        )
        summary_clean = verifier_clean.verify_all()
        self.assertTrue(summary_clean["all_absent"])
        self.assertEqual(summary_clean["status"], "PASS")
        self.assertEqual(summary_clean["still_present_count"], 0)

        # 3. Live mode strictly requires manifest; omitting manifest in live mode fails closed
        verifier_live_no_manifest = TeardownAbsenceVerifier(
            inventory=complete_inventory,
            mode="live",
            cli_runner=mock_all_absent_runner,
        )
        summary_no_manifest = verifier_live_no_manifest.verify_all()
        self.assertFalse(summary_no_manifest["all_absent"])
        self.assertEqual(summary_no_manifest["status"], "UNVERIFIED")
        self.assertEqual(summary_no_manifest["error"], "MANIFEST_REQUIRED_IN_LIVE_MODE")

        # 4. Removing one role that was provisioned in pre-teardown manifest causes UNVERIFIED
        manifest_multi_role = {
            "run_id": "run-20261110-edge-02",
            "expected_resources": {
                "vpc_id": "vpc-0123456789abcdef0",
                "dynamodb_tables": ["miles-dev-records"],
                "s3_buckets": ["miles-dev-data"],
                "iam_roles": ["miles-dev-worker", "miles-dev-controller"],
                "cloudwatch_alarms": ["miles-dev-alarm"],
            },
        }
        manifest_multi_file = self.tmppath / "manifest-multi.json"
        manifest_multi_file.write_text(json.dumps(manifest_multi_role), encoding="utf-8")

        verifier_missing_role = TeardownAbsenceVerifier(
            inventory=complete_inventory,  # only has miles-dev-worker, missing miles-dev-controller
            manifest=manifest_multi_file,
            cli_runner=mock_all_absent_runner,
        )
        summary_missing_role = verifier_missing_role.verify_all()
        self.assertFalse(summary_missing_role["all_absent"])
        self.assertEqual(summary_missing_role["status"], "UNVERIFIED")
        self.assertIn("manifest_missing:iam_roles:miles-dev-controller", summary_missing_role["missing_expected_types"])

        # 5. CLI in live mode without --manifest fails with exit code 1
        with unittest.mock.patch("sys.argv", ["verify_teardown_absence.py", "--inventory-json", str(outputs_file)]):
            rc = main()
            self.assertEqual(rc, 1)

    def test_ledger_export_prevents_symlink_clobber(self):
        """
        [P2 Regression] Verifies that both temporary file symlinks and destination symlinks
        cannot clobber another private file, and destination symlinks are explicitly rejected.
        """
        sensitive_file = self.tmppath / "other-private-evidence.txt"
        sensitive_file.write_text("SUPER_SECRET_EVIDENCE_DO_NOT_OVERWRITE", encoding="utf-8")
        os.chmod(str(sensitive_file), 0o600)

        inventory = {
            "dynamodb_tables": ["tbl1"],
        }
        verifier = TeardownAbsenceVerifier(
            inventory=inventory,
            mode="dry-run",
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )

        # 1. Pre-planted temporary symlink at predictable tmp path location
        ledger_file = self.tmppath / "absence-ledger.json"
        predictable_tmp = self.tmppath / f"absence-ledger.tmp.{os.getpid()}"
        try:
            os.symlink(str(sensitive_file), str(predictable_tmp))
        except OSError:
            pass

        verifier.export_summary(ledger_file)
        # Sensitive file must remain completely untouched!
        self.assertEqual(sensitive_file.read_text(encoding="utf-8"), "SUPER_SECRET_EVIDENCE_DO_NOT_OVERWRITE")
        self.assertTrue(ledger_file.exists())
        self.assertEqual(ledger_file.stat().st_mode & 0o777, 0o600)

        # 2. Pre-planted destination symlink: export_summary must refuse to write through symlink
        dest_symlink = self.tmppath / "dest-symlink-ledger.json"
        try:
            os.symlink(str(sensitive_file), str(dest_symlink))
        except OSError:
            pass

        with self.assertRaises(ValueError) as ctx:
            verifier.export_summary(dest_symlink)
        self.assertIn("is a symlink", str(ctx.exception))

        # Target file must still be completely untouched
        self.assertEqual(sensitive_file.read_text(encoding="utf-8"), "SUPER_SECRET_EVIDENCE_DO_NOT_OVERWRITE")

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


if __name__ == "__main__":
    unittest.main()
