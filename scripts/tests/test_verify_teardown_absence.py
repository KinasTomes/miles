"""
Unit tests for Gate E AWS Teardown Absence Verifier (scripts/verify_teardown_absence.py).
"""

import json
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

    def test_extract_inventory_from_outputs(self):
        """Verifies parsing of nested Terraform output format into flat inventory dict."""
        sample_outputs = {
            "runtime_inventory": {
                "value": {
                    "expected_tables": ["miles-dev-records", "miles-dev-leases"],
                    "data_bucket_name": "miles-dev-data-12345",
                    "kms_key_arn": "arn:aws:kms:ap-southeast-1:123456789012:key/test-key-id",
                    "secret_metadata_arn": "arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:test-secret",
                }
            },
            "network_inventory": {
                "value": {
                    "vpc_id": "vpc-0123456789abcdef0",
                    "alb_name": "miles-dev-alb",
                }
            },
            "edge_provider": {
                "value": {
                    "role_name": "miles-edge-role",
                    "trust_anchor_arn": "arn:aws:rolesanywhere:ap-southeast-1:123456789012:trust-anchor/abc",
                }
            },
            "data_bucket_name": {
                "value": "miles-dev-data-12345"  # duplicate should be de-duped
            },
        }

        inv = TeardownAbsenceVerifier.extract_inventory_from_outputs(sample_outputs)
        self.assertEqual(inv["dynamodb_tables"], ["miles-dev-records", "miles-dev-leases"])
        self.assertEqual(inv["s3_buckets"], ["miles-dev-data-12345"])
        self.assertEqual(inv["vpc_id"], "vpc-0123456789abcdef0")
        self.assertEqual(inv["alb_names"], ["miles-dev-alb"])
        self.assertEqual(inv["iam_roles"], ["miles-edge-role"])
        self.assertEqual(inv["kms_keys"], ["arn:aws:kms:ap-southeast-1:123456789012:key/test-key-id"])
        self.assertEqual(inv["secrets"], ["arn:aws:secretsmanager:ap-southeast-1:123456789012:secret:test-secret"])
        self.assertEqual(inv["trust_anchors"], ["arn:aws:rolesanywhere:ap-southeast-1:123456789012:trust-anchor/abc"])

    def test_check_resource_absent_statuses(self):
        """Verifies ABSENT, STILL_PRESENT, and UNEXPECTED_ERROR classification."""
        # 1. Resource is absent (command fails with expected not-found error)
        def mock_absent_runner(cmd):
            return 255, "", "An error occurred (ResourceNotFoundException) when calling DescribeTable: Table not found"

        verifier = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_absent_runner)
        res = verifier.check_resource_absent("my-table", "dynamodb_table", ["dynamodb", "describe-table"], EXPECTED_ABSENCE_ERRORS["dynamodb_table"])
        self.assertEqual(res["status"], "ABSENT")
        self.assertEqual(res["reason"], "NOT_FOUND_MATCHED")

        # 2. Resource still exists (command succeeds with exit code 0 -> LEAK)
        def mock_present_runner(cmd):
            return 0, '{"Table": {"TableName": "my-table"}}', ""

        verifier_present = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_present_runner)
        res_present = verifier_present.check_resource_absent("my-table", "dynamodb_table", ["dynamodb", "describe-table"], EXPECTED_ABSENCE_ERRORS["dynamodb_table"])
        self.assertEqual(res_present["status"], "STILL_PRESENT")
        self.assertEqual(res_present["reason"], "RESOURCE_EXISTS_LEAK")

        # 3. Unexpected error (e.g. AccessDenied)
        def mock_error_runner(cmd):
            return 255, "", "An error occurred (AccessDeniedException): User is not authorized"

        verifier_err = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_error_runner)
        res_err = verifier_err.check_resource_absent("my-table", "dynamodb_table", ["dynamodb", "describe-table"], EXPECTED_ABSENCE_ERRORS["dynamodb_table"])
        self.assertEqual(res_err["status"], "UNEXPECTED_ERROR")
        self.assertIn("exit_code_255", res_err["reason"])

    def test_check_kms_key_pending_deletion(self):
        """Verifies KMS key PendingDeletion detection vs still present vs absent."""
        # Pending deletion
        def mock_kms_pending(cmd):
            return 0, json.dumps({"State": "PendingDeletion", "DeletionDate": "2026-12-10T00:00:00Z"}), ""

        v_pending = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_kms_pending)
        r_pending = v_pending.check_kms_key_pending_deletion("arn:aws:kms:key/123")
        self.assertEqual(r_pending["status"], "PENDING_DELETION")
        self.assertEqual(r_pending["deletion_date"], "2026-12-10T00:00:00Z")

        # Active (Enabled -> STILL_PRESENT leak)
        def mock_kms_enabled(cmd):
            return 0, json.dumps({"State": "Enabled", "DeletionDate": None}), ""

        v_enabled = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_kms_enabled)
        r_enabled = v_enabled.check_kms_key_pending_deletion("arn:aws:kms:key/123")
        self.assertEqual(r_enabled["status"], "STILL_PRESENT")

        # Deleted / NotFound
        def mock_kms_notfound(cmd):
            return 255, "", "NotFoundException: Key does not exist"

        v_notfound = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_kms_notfound)
        r_notfound = v_notfound.check_kms_key_pending_deletion("arn:aws:kms:key/123")
        self.assertEqual(r_notfound["status"], "ABSENT")

    def test_check_secret_pending_deletion(self):
        """Verifies Secrets Manager DeletedDate detection vs active vs absent."""
        # Pending deletion
        def mock_sec_pending(cmd):
            return 0, json.dumps({"DeletedDate": "2026-12-10T00:00:00Z"}), ""

        v_pending = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_sec_pending)
        r_pending = v_pending.check_secret_pending_deletion("arn:aws:secretsmanager:secret/123")
        self.assertEqual(r_pending["status"], "PENDING_DELETION")
        self.assertEqual(r_pending["deletion_date"], "2026-12-10T00:00:00Z")

        # Active (No DeletedDate -> STILL_PRESENT leak)
        def mock_sec_active(cmd):
            return 0, json.dumps({"DeletedDate": None}), ""

        v_active = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_sec_active)
        r_active = v_active.check_secret_pending_deletion("arn:aws:secretsmanager:secret/123")
        self.assertEqual(r_active["status"], "STILL_PRESENT")

        # NotFound
        def mock_sec_notfound(cmd):
            return 255, "", "ResourceNotFoundException: Secret not found"

        v_notfound = TeardownAbsenceVerifier(inventory={}, cli_runner=mock_sec_notfound)
        r_notfound = v_notfound.check_secret_pending_deletion("arn:aws:secretsmanager:secret/123")
        self.assertEqual(r_notfound["status"], "ABSENT")

    def test_verify_all_clean_teardown(self):
        """Verifies verify_all returns all_absent=True when all resources are absent/pending."""
        inventory = {
            "dynamodb_tables": ["table1"],
            "s3_buckets": ["bucket1"],
            "vpc_id": "vpc-123",
            "alb_names": ["alb1"],
            "iam_roles": ["role1"],
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
            # All others return expected not found
            return 255, "", "ResourceNotFoundException 404 LoadBalancerNotFound InvalidVpcID.NotFound NoSuchEntity InvalidDocument InvalidKeyPair.NotFound"

        verifier = TeardownAbsenceVerifier(inventory=inventory, cli_runner=mock_clean_runner)
        summary = verifier.verify_all()
        self.assertTrue(summary["all_absent"])
        self.assertEqual(summary["still_present_count"], 0)
        self.assertEqual(summary["unexpected_error_count"], 0)
        self.assertEqual(summary["total_checked"], 11)
        self.assertEqual(summary["pending_deletion_count"], 2)
        self.assertEqual(summary["absent_count"], 9)

    def test_verify_all_detected_leak(self):
        """Verifies verify_all flags leak when at least one resource is STILL_PRESENT."""
        inventory = {
            "dynamodb_tables": ["table1"],
            "s3_buckets": ["bucket_leaked"],
        }

        def mock_leak_runner(cmd):
            if "head-bucket" in cmd:
                return 0, "", ""  # Bucket still exists!
            return 255, "", "ResourceNotFoundException"

        verifier = TeardownAbsenceVerifier(inventory=inventory, cli_runner=mock_leak_runner)
        summary = verifier.verify_all()
        self.assertFalse(summary["all_absent"])
        self.assertEqual(summary["still_present_count"], 1)
        self.assertIn("bucket_leaked", summary["still_present"])

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
            self.assertEqual(ledger_data["total_checked"], 4)
        finally:
            sys.argv = old_argv


if __name__ == "__main__":
    unittest.main()
