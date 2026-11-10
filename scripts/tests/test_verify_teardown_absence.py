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

    def test_deleting_volume_is_not_absent(self):
        runner = lambda cmd: (0, json.dumps({"Volumes": [{"VolumeId": "vol-1", "State": "deleting"}]}), "")
        result = TeardownAbsenceVerifier(inventory={"volumes": ["vol-1"]}, cli_runner=runner).verify_all()
        self.assertFalse(result["all_absent"])
        self.assertEqual(result["status"], "FAIL")

    def test_live_never_passes_without_state_reconciliation(self):
        inventory = {"account_id": "123456789012", "dynamodb_tables": ["table"]}
        runner = lambda cmd: (0, json.dumps({"Account": "123456789012"}), "")
        verifier = TeardownAbsenceVerifier(inventory=inventory, mode="live", manifest={"run_id": "test"}, cli_runner=runner)
        result = verifier.verify_all()
        self.assertFalse(result["all_absent"])
        self.assertEqual(result["error"], "TERRAFORM_STATE_RECONCILIATION_UNAVAILABLE")

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

        v_invalid = TeardownAbsenceVerifier(inventory={}, cli_runner=lambda cmd: (255, "", "ValidationError: Invalid alarm names"))
        self.assertEqual(v_invalid.check_cloudwatch_alarm_absent("bad")["status"], "UNEXPECTED_ERROR")
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
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-20261110-edge-01",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles", "environment": "dev"},
            "expected_types": ["vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms"],
            "expected_counts": {
                "vpc_id": 1,
                "dynamodb_tables": 1,
                "s3_buckets": 1,
                "iam_roles": 1,
                "cloudwatch_alarms": 1,
            },
            "expected_resources": {
                "vpc_id": "vpc-0123456789abcdef0",
                "dynamodb_tables": ["miles-dev-records"],
                "s3_buckets": ["miles-dev-data"],
                "iam_roles": ["miles-dev-worker"],
                "cloudwatch_alarms": ["miles-dev-alarm"],
            },
            "provenance": {
                "source": "terraform_pre_destroy",
                "created_at_utc": "2026-11-10T08:00:00Z",
                "sha256": "abcdef1234567890abcdef1234567890abcdef1234567890abcdef1234567890",
            },
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

        # 2. Complete inventory matching manifest with all native checks reporting absent returns PASS
        complete_inventory = {
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "vpc_id": "vpc-0123456789abcdef0",
            "dynamodb_tables": ["miles-dev-records"],
            "s3_buckets": ["miles-dev-data"],
            "iam_roles": ["miles-dev-worker"],
            "cloudwatch_alarms": ["miles-dev-alarm"],
        }

        def mock_all_absent_runner(cmd):
            if "get-caller-identity" in cmd:
                return 0, json.dumps({"Account": "123456789012", "Arn": "arn:aws:iam::123456789012:user/tester"}), ""
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
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-20261110-edge-02",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles", "environment": "dev"},
            "expected_types": ["vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms"],
            "expected_counts": {
                "vpc_id": 1,
                "dynamodb_tables": 1,
                "s3_buckets": 1,
                "iam_roles": 2,
                "cloudwatch_alarms": 1,
            },
            "expected_resources": {
                "vpc_id": "vpc-0123456789abcdef0",
                "dynamodb_tables": ["miles-dev-records"],
                "s3_buckets": ["miles-dev-data"],
                "iam_roles": ["miles-dev-worker", "miles-dev-controller"],
                "cloudwatch_alarms": ["miles-dev-alarm"],
            },
            "provenance": {
                "source": "terraform_pre_destroy",
                "created_at_utc": "2026-11-10T08:00:00Z",
            },
        }
        manifest_multi_file = self.tmppath / "manifest-multi.json"
        manifest_multi_file.write_text(json.dumps(manifest_multi_role), encoding="utf-8")

        verifier_missing_role = TeardownAbsenceVerifier(
            inventory=dict(complete_inventory),  # only has miles-dev-worker, missing miles-dev-controller
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

    def test_manifest_schema_validation_missing_fields_and_invalid_types_fails_closed(self):
        """
        [P1 Regression] Verifies that manifests missing account/region, having invalid expected_types,
        count mismatches, or unknown keys fail closed with UNVERIFIED.
        """
        base_manifest = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-test-001",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles"},
            "expected_types": ["vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms"],
            "expected_counts": {"vpc_id": 1, "dynamodb_tables": 1, "s3_buckets": 1, "iam_roles": 1, "cloudwatch_alarms": 1},
            "expected_resources": {
                "vpc_id": "vpc-1",
                "dynamodb_tables": ["tbl-1"],
                "s3_buckets": ["bkt-1"],
                "iam_roles": ["role-1"],
                "cloudwatch_alarms": ["alarm-1"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }

        matching_inv = {
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "vpc_id": "vpc-1",
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["role-1"],
            "cloudwatch_alarms": ["alarm-1"],
        }

        # 1. Missing account_id
        m_no_account = dict(base_manifest)
        del m_no_account["account_id"]
        v1 = TeardownAbsenceVerifier(inventory=matching_inv, manifest=m_no_account)
        s1 = v1.verify_all()
        self.assertEqual(s1["status"], "UNVERIFIED")
        self.assertIn("missing_manifest_field:account_id", s1["manifest_errors"])

        # 2. Missing region
        m_no_region = dict(base_manifest)
        del m_no_region["region"]
        v2 = TeardownAbsenceVerifier(inventory=matching_inv, manifest=m_no_region)
        s2 = v2.verify_all()
        self.assertEqual(s2["status"], "UNVERIFIED")
        self.assertIn("missing_manifest_field:region", s2["manifest_errors"])

        # 3. Invalid expected_types: "not-an-array"
        m_bad_types = dict(base_manifest)
        m_bad_types["expected_types"] = "not-an-array"
        v3 = TeardownAbsenceVerifier(inventory=matching_inv, manifest=m_bad_types)
        s3 = v3.verify_all()
        self.assertEqual(s3["status"], "UNVERIFIED")
        self.assertIn("invalid_manifest_expected_types_not_a_list", s3["manifest_errors"])

        # 4. Unknown fields rejected
        m_unknown = dict(base_manifest)
        m_unknown["arbitrary_unknown_key"] = "bogus_value"
        v4 = TeardownAbsenceVerifier(inventory=matching_inv, manifest=m_unknown)
        s4 = v4.verify_all()
        self.assertEqual(s4["status"], "UNVERIFIED")
        self.assertIn("unknown_manifest_field:arbitrary_unknown_key", s4["manifest_errors"])

        # 5. Expected counts mismatch
        m_count_mismatch = dict(base_manifest)
        m_count_mismatch["expected_counts"] = dict(base_manifest["expected_counts"])
        m_count_mismatch["expected_counts"]["iam_roles"] = 5  # Only 1 role in expected_resources
        v5 = TeardownAbsenceVerifier(inventory=matching_inv, manifest=m_count_mismatch)
        s5 = v5.verify_all()
        self.assertEqual(s5["status"], "UNVERIFIED")
        self.assertIn("manifest_count_mismatch:iam_roles:expected_5_got_1", s5["manifest_errors"])

    def test_manifest_reconciliation_two_way_discrepancy_and_live_caller_identity(self):
        """
        [P1 Regression] Verifies two-way reconciliation:
        - Provisioned resource in inventory omitted from manifest fails closed.
        - Essential resource types omitted from manifest fails closed.
        - Live mode STS caller identity mismatch fails closed.
        """
        manifest = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-test-002",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles"},
            "expected_types": ["vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms"],
            "expected_counts": {"vpc_id": 1, "dynamodb_tables": 1, "s3_buckets": 1, "iam_roles": 1, "cloudwatch_alarms": 1},
            "expected_resources": {
                "vpc_id": "vpc-1",
                "dynamodb_tables": ["tbl-1"],
                "s3_buckets": ["bkt-1"],
                "iam_roles": ["role-1"],
                "cloudwatch_alarms": ["alarm-1"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }

        # 1. Inventory has unmanifested role (hidden resource omitted from manifest)
        leak_inv = {
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "vpc_id": "vpc-1",
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["role-1", "unmanifested-hidden-role"],
            "cloudwatch_alarms": ["alarm-1"],
        }
        v1 = TeardownAbsenceVerifier(inventory=leak_inv, manifest=manifest)
        s1 = v1.verify_all()
        self.assertEqual(s1["status"], "UNVERIFIED")
        self.assertIn("inventory_unmanifested_resource:iam_roles:unmanifested-hidden-role", s1["manifest_errors"])

        # 2. Manifest omits essential resource types (e.g. partial allowlist like Extaseq fixture)
        partial_manifest = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-test-partial",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles"},
            "expected_types": ["dynamodb_tables", "iam_roles"],  # missing s3_buckets, cloudwatch_alarms, vpc_id
            "expected_counts": {"dynamodb_tables": 1, "iam_roles": 1},
            "expected_resources": {"dynamodb_tables": ["test-table"], "iam_roles": ["role-one"]},
            "provenance": {"source": "manual", "created_at_utc": "2026-11-10T00:00:00Z"},
        }
        partial_inv = {"dynamodb_tables": ["test-table"], "iam_roles": ["role-one"]}
        v2 = TeardownAbsenceVerifier(inventory=partial_inv, manifest=partial_manifest)
        s2 = v2.verify_all()
        self.assertEqual(s2["status"], "UNVERIFIED")
        self.assertIn("manifest_missing_essential_type:s3_buckets", s2["manifest_errors"])
        self.assertIn("manifest_missing_essential_type:cloudwatch_alarms", s2["manifest_errors"])
        self.assertIn("manifest_missing_essential_type:vpc_id", s2["manifest_errors"])

        # 3. Live mode STS caller identity mismatch
        matching_inv = {
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "vpc_id": "vpc-1",
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["role-1"],
            "cloudwatch_alarms": ["alarm-1"],
        }

        def mock_wrong_account_runner(cmd):
            if "get-caller-identity" in cmd:
                return 0, json.dumps({"Account": "999999999999", "Arn": "arn:aws:iam::999999999999:user/wrong"}), ""
            return 255, "", "ResourceNotFoundException"

        v3 = TeardownAbsenceVerifier(
            inventory=matching_inv,
            manifest=manifest,
            mode="live",
            cli_runner=mock_wrong_account_runner,
        )
        s3 = v3.verify_all()
        self.assertEqual(s3["status"], "UNVERIFIED")
        self.assertEqual(s3["error"], "CALLER_IDENTITY_VERIFICATION_FAILED")
        self.assertIn("caller_account_mismatch:cli_account_999999999999_manifest_123456789012", s3["reason"])

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

    def test_unsupported_resource_type_in_manifest_and_inventory_fails_closed(self):
        """
        [P1 Regression] Verifies that manifest or inventory declaring a resource type
        unsupported by native verifier checkers fails closed with UNVERIFIED (all_absent=False).
        """
        # 1. Unsupported type in manifest expected_types & expected_resources
        manifest_unsupported = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-test-unsupported",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles"},
            "expected_types": [
                "vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms",
                "unsupported_widget_type",
            ],
            "expected_counts": {
                "vpc_id": 1, "dynamodb_tables": 1, "s3_buckets": 1, "iam_roles": 1, "cloudwatch_alarms": 1,
                "unsupported_widget_type": 1,
            },
            "expected_resources": {
                "vpc_id": "vpc-1",
                "dynamodb_tables": ["tbl-1"],
                "s3_buckets": ["bkt-1"],
                "iam_roles": ["role-1"],
                "cloudwatch_alarms": ["alarm-1"],
                "unsupported_widget_type": ["widget-001"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }
        inv_unsupported = {
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "vpc_id": "vpc-1",
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["role-1"],
            "cloudwatch_alarms": ["alarm-1"],
            "unsupported_widget_type": ["widget-001"],
        }
        v1 = TeardownAbsenceVerifier(
            inventory=inv_unsupported,
            manifest=manifest_unsupported,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )
        s1 = v1.verify_all()
        self.assertFalse(s1["all_absent"])
        self.assertEqual(s1["status"], "UNVERIFIED")
        self.assertIn("unsupported_resource_type:unsupported_widget_type", s1["manifest_errors"])
        self.assertIn("unsupported_inventory_resource_type:unsupported_widget_type", s1["manifest_errors"])

        # 2. Unsupported type in standalone inventory (without manifest)
        inv_standalone_unsupported = {
            "dynamodb_tables": ["tbl-1"],
            "unsupported_custom_service": ["service-123"],
        }
        v2 = TeardownAbsenceVerifier(
            inventory=inv_standalone_unsupported,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )
        s2 = v2.verify_all()
        self.assertFalse(s2["all_absent"])
        self.assertEqual(s2["status"], "UNVERIFIED")
        self.assertEqual(s2["error"], "INCOMPLETE_INVENTORY_MISSING_EXPECTED_RESOURCES")
        self.assertIn("unsupported_inventory_resource_type:unsupported_custom_service", s2["missing_expected_types"])

    def test_unchecked_declared_resource_guard_fails_closed(self):
        """
        [P1 Regression] Verifies that if any resource declared in manifest or inventory
        is not queried by a native checker, verify_all fails closed with UNCHECKED_RESOURCES_DETECTED.
        """
        inventory = {
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
        }
        verifier = TeardownAbsenceVerifier(
            inventory=inventory,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )

        # Adversarially simulate a bug where s3_buckets loop is bypassed
        original_check_resource_absent = verifier.check_resource_absent

        def selective_runner(name, resource_type, args, errors):
            if resource_type == "s3_bucket":
                # Simulate caller skipping s3 check or not registering to results
                return {"resource": name, "type": resource_type, "status": "SKIPPED"}
            return original_check_resource_absent(name, resource_type, args, errors)

        # Inject an unchecked resource into expected_resources via manifest
        manifest = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-unchecked-test",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles"},
            "expected_types": ["vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms"],
            "expected_counts": {"vpc_id": 1, "dynamodb_tables": 1, "s3_buckets": 1, "iam_roles": 1, "cloudwatch_alarms": 1},
            "expected_resources": {
                "vpc_id": "vpc-1",
                "dynamodb_tables": ["tbl-1"],
                "s3_buckets": ["bkt-1"],
                "iam_roles": ["role-1"],
                "cloudwatch_alarms": ["alarm-1"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }
        full_inv = {
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "vpc_id": "vpc-1",
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["role-1"],
            "cloudwatch_alarms": ["alarm-1"],
        }
        v = TeardownAbsenceVerifier(
            inventory=full_inv,
            manifest=manifest,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )
        # Artificially remove one resource from the checked inventory
        v.inventory["s3_buckets"] = []  # S3 bucket won't be checked in verify_all loop!
        v.manifest_errors = []          # Clear manifest reconciliation error to specifically test verify_all guard

        summary = v.verify_all()
        self.assertFalse(summary["all_absent"])
        self.assertEqual(summary["status"], "UNVERIFIED")
        self.assertEqual(summary["error"], "UNCHECKED_RESOURCES_DETECTED")
        self.assertIn("s3_buckets:bkt-1", summary["unchecked_resources"])

    def test_duplicate_resource_ids_in_manifest_and_inventory_fails_closed(self):
        """
        [P2 Regression] Verifies that duplicate resource IDs in manifest or inventory
        (e.g. same role declared twice with expected_counts=2) fail closed with UNVERIFIED.
        """
        # 1. Manifest with duplicate IAM role IDs and expected_counts=2
        manifest_dupe_count2 = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-dupe-1",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles"},
            "expected_types": ["vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms"],
            "expected_counts": {
                "vpc_id": 1,
                "dynamodb_tables": 1,
                "s3_buckets": 1,
                "iam_roles": 2,  # Declaring 2 roles, but duplicating 'miles-role-1'!
                "cloudwatch_alarms": 1,
            },
            "expected_resources": {
                "vpc_id": "vpc-1",
                "dynamodb_tables": ["tbl-1"],
                "s3_buckets": ["bkt-1"],
                "iam_roles": ["miles-role-1", "miles-role-1"],  # Duplicate!
                "cloudwatch_alarms": ["alarm-1"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }
        inv1 = {
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "vpc_id": "vpc-1",
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["miles-role-1"],
            "cloudwatch_alarms": ["alarm-1"],
        }
        v1 = TeardownAbsenceVerifier(
            inventory=inv1,
            manifest=manifest_dupe_count2,
            cli_runner=lambda cmd: (255, "", "NoSuchEntity ResourceNotFoundException"),
        )
        s1 = v1.verify_all()
        self.assertFalse(s1["all_absent"])
        self.assertEqual(s1["status"], "UNVERIFIED")
        self.assertIn("manifest_duplicate_resource_ids:iam_roles:['miles-role-1']", s1["manifest_errors"])
        self.assertIn("manifest_count_mismatch:iam_roles:expected_2_got_1", s1["manifest_errors"])

        # 2. Manifest with duplicate IAM role IDs and expected_counts=1
        manifest_dupe_count1 = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-dupe-2",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles"},
            "expected_types": ["vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms"],
            "expected_counts": {
                "vpc_id": 1,
                "dynamodb_tables": 1,
                "s3_buckets": 1,
                "iam_roles": 1,
                "cloudwatch_alarms": 1,
            },
            "expected_resources": {
                "vpc_id": "vpc-1",
                "dynamodb_tables": ["tbl-1"],
                "s3_buckets": ["bkt-1"],
                "iam_roles": ["miles-role-1", "miles-role-1"],  # Duplicate!
                "cloudwatch_alarms": ["alarm-1"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }
        v2 = TeardownAbsenceVerifier(
            inventory=inv1,
            manifest=manifest_dupe_count1,
            cli_runner=lambda cmd: (255, "", "NoSuchEntity ResourceNotFoundException"),
        )
        s2 = v2.verify_all()
        self.assertFalse(s2["all_absent"])
        self.assertEqual(s2["status"], "UNVERIFIED")
        self.assertIn("manifest_duplicate_resource_ids:iam_roles:['miles-role-1']", s2["manifest_errors"])
        self.assertIn("manifest_non_unique_resource_ids:iam_roles:unique_1_total_2", s2["manifest_errors"])

        # 3. Inventory with duplicate IDs
        inv_dupe = {
            "dynamodb_tables": ["table-dup", "table-dup"],
        }
        v3 = TeardownAbsenceVerifier(
            inventory=inv_dupe,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )
        s3 = v3.verify_all()
        self.assertFalse(s3["all_absent"])
        self.assertEqual(s3["status"], "UNVERIFIED")
        self.assertEqual(s3["error"], "INCOMPLETE_INVENTORY_MISSING_EXPECTED_RESOURCES")
        self.assertIn("inventory_duplicate_resource_ids:dynamodb_tables:['table-dup']", s3["missing_expected_types"])

    def test_nat_gateway_and_elastic_ip_native_absence_and_leak_detection(self):
        """
        [P1 Regression] Verifies native absence and leak checking for NAT Gateways and Elastic IPs.
        - NAT Gateway: not found -> ABSENT; state deleted -> ABSENT; state deleting -> PENDING_DELETION; available -> STILL_PRESENT
        - Elastic IP: not found -> ABSENT; empty list -> ABSENT; allocated in list -> STILL_PRESENT
        """
        # 1. NAT Gateway absent via error code
        v_nat_err = TeardownAbsenceVerifier(
            inventory={},
            cli_runner=lambda cmd: (255, "", "NatGatewayNotFound: NatGateway nat-0123 not found"),
        )
        res_nat_err = v_nat_err.check_nat_gateway_absent("nat-0123")
        self.assertEqual(res_nat_err["status"], "ABSENT")
        self.assertEqual(res_nat_err["reason"], "NAT_GATEWAY_NOT_FOUND")

        # 2. NAT Gateway state == 'deleted' -> ABSENT
        v_nat_del = TeardownAbsenceVerifier(
            inventory={},
            cli_runner=lambda cmd: (0, json.dumps({"NatGateways": [{"NatGatewayId": "nat-0123", "State": "deleted"}]}), ""),
        )
        res_nat_del = v_nat_del.check_nat_gateway_absent("nat-0123")
        self.assertEqual(res_nat_del["status"], "ABSENT")
        self.assertEqual(res_nat_del["reason"], "NAT_GATEWAY_DELETED")

        # 3. NAT Gateway state == 'deleting' is not yet absent
        v_nat_deleting = TeardownAbsenceVerifier(
            inventory={},
            cli_runner=lambda cmd: (0, json.dumps({"NatGateways": [{"NatGatewayId": "nat-0123", "State": "deleting"}]}), ""),
        )
        res_nat_deleting = v_nat_deleting.check_nat_gateway_absent("nat-0123")
        self.assertEqual(res_nat_deleting["status"], "STILL_PRESENT")

        # 4. NAT Gateway state == 'available' -> STILL_PRESENT leak!
        v_nat_avail = TeardownAbsenceVerifier(
            inventory={},
            cli_runner=lambda cmd: (0, json.dumps({"NatGateways": [{"NatGatewayId": "nat-0123", "State": "available"}]}), ""),
        )
        res_nat_avail = v_nat_avail.check_nat_gateway_absent("nat-0123")
        self.assertEqual(res_nat_avail["status"], "STILL_PRESENT")
        self.assertEqual(res_nat_avail["reason"], "NAT_GATEWAY_STATE_AVAILABLE_LEAK")

        # 5. Elastic IP absent via error code
        v_eip_err = TeardownAbsenceVerifier(
            inventory={},
            cli_runner=lambda cmd: (255, "", "InvalidAllocationID.NotFound: The allocation ID 'eipalloc-0123' does not exist"),
        )
        res_eip_err = v_eip_err.check_elastic_ip_absent("eipalloc-0123")
        self.assertEqual(res_eip_err["status"], "ABSENT")
        self.assertEqual(res_eip_err["reason"], "ALLOCATION_ID_NOT_FOUND")

        # 6. Elastic IP empty list -> ABSENT
        v_eip_empty = TeardownAbsenceVerifier(
            inventory={},
            cli_runner=lambda cmd: (0, json.dumps({"Addresses": []}), ""),
        )
        res_eip_empty = v_eip_empty.check_elastic_ip_absent("eipalloc-0123")
        self.assertEqual(res_eip_empty["status"], "ABSENT")
        self.assertEqual(res_eip_empty["reason"], "ALLOCATION_ID_NOT_IN_LIST")

        # 7. Elastic IP present in Addresses list -> STILL_PRESENT leak!
        v_eip_leak = TeardownAbsenceVerifier(
            inventory={},
            cli_runner=lambda cmd: (0, json.dumps({"Addresses": [{"AllocationId": "eipalloc-0123", "PublicIp": "198.51.100.1"}]}), ""),
        )
        res_eip_leak = v_eip_leak.check_elastic_ip_absent("eipalloc-0123")
        self.assertEqual(res_eip_leak["status"], "STILL_PRESENT")
        self.assertEqual(res_eip_leak["reason"], "ELASTIC_IP_STILL_ALLOCATED_LEAK")

    def test_nat_gateway_and_elastic_ip_extraction_and_full_verification(self):
        """
        [P1 Regression] Verifies extraction of nat_gateway_ids and elastic_ip_allocation_ids
        from Terraform outputs, checks missing expected types when nat_count > 0, and
        verifies full clean teardown across all 24 resource types.
        """
        tf_outputs_with_nat = {
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                    "nat_count": 1,
                    "nat_gateway_ids": {"ap-southeast-1a": "nat-0123456789abcdef0"},
                    "elastic_ip_allocation_ids": {"ap-southeast-1a": "eipalloc-0123456789abcdef0"},
                    "public_subnets": {"ap-southeast-1a": "subnet-pub1"},
                    "private_subnets": {"ap-southeast-1a": "subnet-priv1"},
                    "security_groups": {"runtime": "sg-111"},
                    "gateway_endpoints": {"s3": "vpce-gw-s3"},
                    "interface_endpoints": {"ec2": "vpce-if-ec2"},
                }
            },
        }

        inv = TeardownAbsenceVerifier.extract_inventory_from_outputs(tf_outputs_with_nat)
        self.assertIn("nat-0123456789abcdef0", inv["nat_gateways"])
        self.assertIn("eipalloc-0123456789abcdef0", inv["elastic_ips"])

        # When nat_count > 0 but nat_gateway_ids is omitted, must flag expected_missing
        tf_outputs_nat_omitted = {
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                    "nat_count": 1,
                    # nat_gateway_ids and elastic_ip_allocation_ids omitted
                }
            }
        }
        inv_omitted = TeardownAbsenceVerifier.extract_inventory_from_outputs(tf_outputs_nat_omitted)
        self.assertIn("nat_gateways", inv_omitted["expected_missing"])
        self.assertIn("elastic_ips", inv_omitted["expected_missing"])

        # Comprehensive 24-resource verification including NAT Gateway and Elastic IP
        full_24_inventory = {
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
            "nat_gateways": ["nat-1"],
            "elastic_ips": ["eipalloc-1"],
        }

        def mock_clean_24_runner(cmd):
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
            if "describe-nat-gateways" in cmd:
                return 0, json.dumps({"NatGateways": [{"NatGatewayId": "nat-1", "State": "deleted"}]}), ""
            if "describe-addresses" in cmd:
                return 0, json.dumps({"Addresses": []}), ""
            return 255, "", "ResourceNotFoundException 404 Not Found LoadBalancerNotFound TargetGroupNotFound InvalidVpcID.NotFound InvalidSubnetID.NotFound InvalidGroup.NotFound InvalidVpcEndpointId.NotFound NoSuchEntity InvalidDocument InvalidKeyPair.NotFound ParameterNotFound"

        verifier24 = TeardownAbsenceVerifier(inventory=full_24_inventory, cli_runner=mock_clean_24_runner)
        summary24 = verifier24.verify_all()
        self.assertTrue(summary24["all_absent"])
        self.assertEqual(summary24["status"], "PASS")
        self.assertEqual(summary24["still_present_count"], 0)
        self.assertEqual(summary24["unexpected_error_count"], 0)
        self.assertEqual(summary24["total_checked"], 24)
        self.assertEqual(summary24["pending_deletion_count"], 2)
        self.assertEqual(summary24["absent_count"], 22)

    def test_nat_and_eip_cardinality_mismatch_fails_closed(self):
        """
        [P1 Regression] Re-review HEAD c74c6ed finding:
        Verifies that when declared nat_count=2, but outputs or manifest list only 1 NAT ID
        and 1 EIP ID (or vice versa), Gate E fails closed with UNVERIFIED (all_absent=False)
        and never produces a false PASS for State 0 teardown.
        """
        # 1. Extaseq reproduction scenario:
        # nat_count=2 declared, but outputs contain only 1 NAT ID and 1 EIP ID.
        tf_partial_nat_outputs = {
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                    "nat_count": 2,
                    "nat_gateway_ids": {"ap-southeast-1a": "nat-0123456789abcdef0"},
                    "elastic_ip_allocation_ids": {"ap-southeast-1a": "eipalloc-0123456789abcdef0"},
                    "public_subnets": {"ap-southeast-1a": "subnet-pub1"},
                    "private_subnets": {"ap-southeast-1a": "subnet-priv1"},
                    "security_groups": {"runtime": "sg-111"},
                    "gateway_endpoints": {"s3": "vpce-gw-s3"},
                    "interface_endpoints": {"ec2": "vpce-if-ec2"},
                }
            },
            "runtime_inventory": {
                "value": {
                    "iam_roles": {"worker": "arn:aws:iam::123456789012:role/worker-role"},
                    "data_bucket_name": "miles-dev-data-123456789012",
                    "expected_tables": ["miles-dev-records"],
                }
            },
            "alarm_contract": {
                "value": {"drift_alarm": "arn:aws:cloudwatch:ap-southeast-1:123456789012:alarm:miles-drift"}
            },
        }

        # Manifest constructed from that same partial inventory (declaring counts=1 and 1 ID each)
        manifest_from_partial_inv = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-partial-nat-test",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles", "environment": "dev"},
            "expected_types": [
                "vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms",
                "nat_gateways", "elastic_ips",
            ],
            "expected_counts": {
                "vpc_id": 1,
                "dynamodb_tables": 1,
                "s3_buckets": 1,
                "iam_roles": 1,
                "cloudwatch_alarms": 1,
                "nat_gateways": 1,  # Partial!
                "elastic_ips": 1,   # Partial!
            },
            "expected_resources": {
                "vpc_id": "vpc-0123456789abcdef0",
                "dynamodb_tables": ["miles-dev-records"],
                "s3_buckets": ["miles-dev-data-123456789012"],
                "iam_roles": ["arn:aws:iam::123456789012:role/worker-role"],
                "cloudwatch_alarms": ["miles-drift"],
                "nat_gateways": ["nat-0123456789abcdef0"],
                "elastic_ips": ["eipalloc-0123456789abcdef0"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }

        # Extract inventory from the partial outputs with manifest
        inv = TeardownAbsenceVerifier.extract_inventory_from_outputs(
            tf_partial_nat_outputs,
            manifest=manifest_from_partial_inv,
        )

        # Inventory must have recorded declared nat_count=2
        self.assertEqual(inv["nat_count"], 2)
        # expected_missing must flag nat_gateway and elastic_ip cardinality mismatches
        self.assertIn(
            "nat_gateway_cardinality_mismatch:declared_2_vs_extracted_1",
            inv["expected_missing"],
        )
        self.assertIn(
            "elastic_ip_cardinality_mismatch:declared_2_vs_extracted_1",
            inv["expected_missing"],
        )
        # Manifest reconciliation must also flag the mismatch against declared nat_count
        self.assertIn(
            "manifest_nat_count_mismatch:declared_2_vs_manifest_1",
            inv["expected_missing"],
        )
        self.assertIn(
            "manifest_elastic_ip_count_mismatch:declared_2_vs_manifest_1",
            inv["expected_missing"],
        )
        self.assertIn(
            "manifest_nat_resource_cardinality_mismatch:declared_2_vs_manifest_1",
            inv["expected_missing"],
        )
        self.assertIn(
            "manifest_elastic_ip_resource_cardinality_mismatch:declared_2_vs_manifest_1",
            inv["expected_missing"],
        )

        # TeardownAbsenceVerifier must fail closed with UNVERIFIED
        v1 = TeardownAbsenceVerifier(
            inventory=inv,
            manifest=manifest_from_partial_inv,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )
        res1 = v1.verify_all()
        self.assertFalse(res1["all_absent"])
        self.assertEqual(res1["status"], "UNVERIFIED")
        self.assertEqual(res1["error"], "INVALID_MANIFEST_OR_RECONCILIATION_FAILURE")
        self.assertEqual(res1["total_checked"], 0)

        # 2. Standalone partial inventory without manifest must also fail closed
        standalone_partial_inv = {
            "vpc_id": "vpc-0123456789abcdef0",
            "nat_count": 2,
            "nat_gateways": ["nat-0123456789abcdef0"],
            "elastic_ips": ["eipalloc-0123456789abcdef0"],
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["role-1"],
            "cloudwatch_alarms": ["alarm-1"],
        }
        v2 = TeardownAbsenceVerifier(
            inventory=standalone_partial_inv,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )
        res2 = v2.verify_all()
        self.assertFalse(res2["all_absent"])
        self.assertEqual(res2["status"], "UNVERIFIED")
        self.assertEqual(res2["error"], "INCOMPLETE_INVENTORY_MISSING_EXPECTED_RESOURCES")
        self.assertIn(
            "nat_gateway_cardinality_mismatch:declared_2_vs_extracted_1",
            res2["missing_expected_types"],
        )
        self.assertIn(
            "elastic_ip_cardinality_mismatch:declared_2_vs_extracted_1",
            res2["missing_expected_types"],
        )

        # 3. Contract violation: NAT Gateway and Elastic IP not 1:1 (e.g. 2 NATs but 1 EIP)
        inv_eip_missing = {
            "vpc_id": "vpc-0123456789abcdef0",
            "nat_count": 2,
            "nat_gateways": ["nat-1", "nat-2"],
            "elastic_ips": ["eipalloc-1"],
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["role-1"],
            "cloudwatch_alarms": ["alarm-1"],
        }
        v3 = TeardownAbsenceVerifier(
            inventory=inv_eip_missing,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )
        res3 = v3.verify_all()
        self.assertFalse(res3["all_absent"])
        self.assertEqual(res3["status"], "UNVERIFIED")
        self.assertIn(
            "elastic_ip_cardinality_mismatch:declared_2_vs_extracted_1",
            res3["missing_expected_types"],
        )

        # 4. Manifest ownership scope declaring nat_count=2 when manifest only declares 1
        manifest_scope_nat2 = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-scope-nat2",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles", "nat_count": 2},
            "expected_types": [
                "vpc_id", "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms",
                "nat_gateways", "elastic_ips",
            ],
            "expected_counts": {
                "vpc_id": 1, "dynamodb_tables": 1, "s3_buckets": 1, "iam_roles": 1, "cloudwatch_alarms": 1,
                "nat_gateways": 1, "elastic_ips": 1,
            },
            "expected_resources": {
                "vpc_id": "vpc-1",
                "dynamodb_tables": ["tbl-1"],
                "s3_buckets": ["bkt-1"],
                "iam_roles": ["role-1"],
                "cloudwatch_alarms": ["alarm-1"],
                "nat_gateways": ["nat-1"],
                "elastic_ips": ["eipalloc-1"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }
        inv_no_nat_count = {
            "vpc_id": "vpc-1",
            "nat_gateways": ["nat-1"],
            "elastic_ips": ["eipalloc-1"],
            "dynamodb_tables": ["tbl-1"],
            "s3_buckets": ["bkt-1"],
            "iam_roles": ["role-1"],
            "cloudwatch_alarms": ["alarm-1"],
        }
        v4 = TeardownAbsenceVerifier(
            inventory=inv_no_nat_count,
            manifest=manifest_scope_nat2,
            cli_runner=lambda cmd: (255, "", "ResourceNotFoundException"),
        )
        res4 = v4.verify_all()
        self.assertFalse(res4["all_absent"])
        self.assertEqual(res4["status"], "UNVERIFIED")
        self.assertIn("manifest_scope_nat_count_mismatch:scope_2_vs_manifest_1", res4["manifest_errors"])
        self.assertIn("manifest_scope_elastic_ip_count_mismatch:scope_2_vs_manifest_1", res4["manifest_errors"])

        # 5. Positive control: complete nat_count=2 with 2 NAT IDs and 2 EIP IDs PASSES
        complete_nat2_outputs = {
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                    "nat_count": 2,
                    "nat_gateway_ids": {
                        "ap-southeast-1a": "nat-01",
                        "ap-southeast-1b": "nat-02",
                    },
                    "elastic_ip_allocation_ids": {
                        "ap-southeast-1a": "eipalloc-01",
                        "ap-southeast-1b": "eipalloc-02",
                    },
                    "public_subnets": {"ap-southeast-1a": "subnet-pub1"},
                    "private_subnets": {"ap-southeast-1a": "subnet-priv1"},
                    "security_groups": {"runtime": "sg-111"},
                    "gateway_endpoints": {"s3": "vpce-gw-s3"},
                    "interface_endpoints": {"ec2": "vpce-if-ec2"},
                }
            },
            "runtime_inventory": {
                "value": {
                    "iam_roles": {"worker": "arn:aws:iam::123456789012:role/worker-role"},
                    "data_bucket_name": "miles-dev-data-123456789012",
                    "expected_tables": ["miles-dev-records"],
                }
            },
            "alarm_contract": {
                "value": {"drift_alarm": "arn:aws:cloudwatch:ap-southeast-1:123456789012:alarm:miles-drift"}
            },
        }
        manifest_complete = {
            "schema_version": "miles-teardown-manifest/v1",
            "run_id": "run-complete-nat2-test",
            "account_id": "123456789012",
            "region": "ap-southeast-1",
            "ownership_scope": {"project": "miles", "environment": "dev", "nat_count": 2},
            "expected_types": [
                "vpc_id", "subnets", "security_groups", "vpc_endpoints",
                "dynamodb_tables", "s3_buckets", "iam_roles", "cloudwatch_alarms",
                "nat_gateways", "elastic_ips",
            ],
            "expected_counts": {
                "vpc_id": 1,
                "subnets": 2,
                "security_groups": 1,
                "vpc_endpoints": 2,
                "dynamodb_tables": 1,
                "s3_buckets": 1,
                "iam_roles": 1,
                "cloudwatch_alarms": 1,
                "nat_gateways": 2,
                "elastic_ips": 2,
            },
            "expected_resources": {
                "vpc_id": "vpc-0123456789abcdef0",
                "subnets": ["subnet-pub1", "subnet-priv1"],
                "security_groups": ["sg-111"],
                "vpc_endpoints": ["vpce-gw-s3", "vpce-if-ec2"],
                "dynamodb_tables": ["miles-dev-records"],
                "s3_buckets": ["miles-dev-data-123456789012"],
                "iam_roles": ["worker-role"],
                "cloudwatch_alarms": ["arn:aws:cloudwatch:ap-southeast-1:123456789012:alarm:miles-drift"],
                "nat_gateways": ["nat-01", "nat-02"],
                "elastic_ips": ["eipalloc-01", "eipalloc-02"],
            },
            "provenance": {"source": "terraform", "created_at_utc": "2026-11-10T00:00:00Z"},
        }
        inv_complete = TeardownAbsenceVerifier.extract_inventory_from_outputs(
            complete_nat2_outputs,
            manifest=manifest_complete,
        )
        self.assertEqual(inv_complete["expected_missing"], [])

        def mock_clean_nat2_runner(cmd):
            if "describe-alarms" in cmd:
                return 0, json.dumps({"MetricAlarms": [], "CompositeAlarms": []}), ""
            if "describe-nat-gateways" in cmd:
                return 0, json.dumps({"NatGateways": [{"NatGatewayId": "nat-01", "State": "deleted"}, {"NatGatewayId": "nat-02", "State": "deleted"}]}), ""
            if "describe-addresses" in cmd:
                return 0, json.dumps({"Addresses": []}), ""
            return 255, "", "ResourceNotFoundException 404 Not Found NoSuchEntity InvalidVpcID.NotFound InvalidSubnetID.NotFound InvalidGroup.NotFound InvalidVpcEndpointId.NotFound"

        v5 = TeardownAbsenceVerifier(
            inventory=inv_complete,
            manifest=manifest_complete,
            cli_runner=mock_clean_nat2_runner,
        )
        res5 = v5.verify_all()
        self.assertTrue(res5["all_absent"])
        self.assertEqual(res5["status"], "PASS")
        self.assertEqual(res5["total_checked"], 14)
        self.assertEqual(res5["absent_count"], 14)



if __name__ == "__main__":
    unittest.main()
