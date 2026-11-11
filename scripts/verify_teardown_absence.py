"""
Native AWS service teardown absence verifier (Gate E).

Performs dynamic, service-native absence checks on all owned resources
identified in Terraform outputs or an inventory manifest.

Distinguishes among:
- ABSENT: Resource confirmed deleted / not found (expected native exception).
- STILL_PRESENT: Resource is still active / exists (LEAK - Gate E failure).
- PENDING_DELETION: Resource is scheduled for deletion (e.g. KMS, Secrets Manager).
- UNEXPECTED_ERROR: Permission denied, network failure, or unknown error.

Strict Guardrails:
- Pluggable runner architecture enables 100% offline verification in tests.
- Rejects reliance on historical 2026-11-08 static proofs; dynamically checks actual inventory.
- Emits structured Gate E ledger artifact: gate-e-absence-summary.json.
"""

import argparse
import datetime
import json
import os
from pathlib import Path
import subprocess
import sys
from typing import Any, Callable, Dict, List, Optional, Tuple


def _iso_utc_now() -> str:
    return datetime.datetime.now(datetime.timezone.utc).isoformat()


EXPECTED_ABSENCE_ERRORS = {
    "dynamodb_table": ["ResourceNotFoundException"],
    "s3_bucket": ["(404)", "404", "Not Found", "NoSuchBucket"],
    "vpc": ["InvalidVpcID.NotFound"],
    "subnet": ["InvalidSubnetID.NotFound"],
    "security_group": ["InvalidGroup.NotFound"],
    "alb": ["LoadBalancerNotFound"],
    "target_group": ["TargetGroupNotFound"],
    "iam_role": ["NoSuchEntity"],
    "iam_instance_profile": ["NoSuchEntity"],
    "acm_certificate": ["ResourceNotFoundException"],
    "ssm_document": ["InvalidDocument"],
    "key_pair": ["InvalidKeyPair.NotFound"],
    "cloudwatch_alarm": ["ResourceNotFound", "ValidationError"],
}


class TeardownAbsenceVerifier:
    """
    Evaluates teardown completeness against owned AWS resources.
    Can be run against live AWS CLI or with an offline mock runner.
    """

    def __init__(
        self,
        inventory: Dict[str, Any],
        profile: Optional[str] = None,
        region: str = "ap-southeast-1",
        account_id: Optional[str] = None,
        cli_runner: Optional[Callable[[List[str]], Tuple[int, str, str]]] = None,
    ):
        self.inventory = inventory
        self.profile = profile
        self.region = region
        self.account_id = account_id or inventory.get("account_id")
        self._runner = cli_runner or self._default_aws_runner

    def _default_aws_runner(self, args: List[str]) -> Tuple[int, str, str]:
        cmd = ["aws", "--region", self.region, "--no-cli-pager"]
        if self.profile:
            cmd.extend(["--profile", self.profile])
        cmd.extend(args)
        try:
            res = subprocess.run(cmd, capture_output=True, text=True, timeout=40)
            return res.returncode, res.stdout, res.stderr
        except Exception as e:
            return -1, "", str(e)

    @classmethod
    def from_terraform_outputs(
        cls,
        outputs_json_path: Path,
        profile: Optional[str] = None,
        region: str = "ap-southeast-1",
        cli_runner: Optional[Callable[[List[str]], Tuple[int, str, str]]] = None,
    ) -> "TeardownAbsenceVerifier":
        """
        Parses active-outputs.json or `terraform output -json` and extracts
        all owned resource identifiers dynamically.
        """
        data = json.loads(Path(outputs_json_path).read_text(encoding="utf-8"))
        inv = cls.extract_inventory_from_outputs(data)
        return cls(inventory=inv, profile=profile, region=region, cli_runner=cli_runner)

    @staticmethod
    def extract_inventory_from_outputs(data: Dict[str, Any]) -> Dict[str, Any]:
        """Extracts owned resource identifiers from nested Terraform output structure."""
        inv: Dict[str, Any] = {
            "dynamodb_tables": [],
            "s3_buckets": [],
            "vpc_id": None,
            "alb_names": [],
            "iam_roles": [],
            "acm_certificates": [],
            "ssm_documents": [],
            "key_pairs": [],
            "cloudwatch_alarms": [],
            "kms_keys": [],
            "secrets": [],
            "trust_anchors": [],
            "asg_names": [],
        }

        # Extract runtime_inventory
        rt = data.get("runtime_inventory", {}).get("value", {}) if isinstance(data.get("runtime_inventory"), dict) else data.get("runtime_inventory", {})
        if isinstance(rt, dict):
            for t in rt.get("expected_tables", []):
                if t not in inv["dynamodb_tables"]:
                    inv["dynamodb_tables"].append(t)
            if rt.get("data_bucket_name"):
                inv["s3_buckets"].append(rt["data_bucket_name"])
            if rt.get("kms_key_arn"):
                inv["kms_keys"].append(rt["kms_key_arn"])
            if rt.get("secret_metadata_arn"):
                inv["secrets"].append(rt["secret_metadata_arn"])

        # Extract network_inventory
        net = data.get("network_inventory", {}).get("value", {}) if isinstance(data.get("network_inventory"), dict) else data.get("network_inventory", {})
        if isinstance(net, dict):
            if net.get("vpc_id"):
                inv["vpc_id"] = net["vpc_id"]
            if net.get("alb_name"):
                inv["alb_names"].append(net["alb_name"])

        # Extract edge_provider
        edge = data.get("edge_provider", {}).get("value", {}) if isinstance(data.get("edge_provider"), dict) else data.get("edge_provider", {})
        if isinstance(edge, dict):
            if edge.get("role_name"):
                inv["iam_roles"].append(edge["role_name"])
            if edge.get("trust_anchor_arn"):
                inv["trust_anchors"].append(edge["trust_anchor_arn"])

        # Direct buckets if present
        if data.get("data_bucket_name", {}).get("value"):
            b = data["data_bucket_name"]["value"]
            if b not in inv["s3_buckets"]:
                inv["s3_buckets"].append(b)

        return inv

    def check_resource_absent(
        self,
        name: str,
        resource_type: str,
        aws_args: List[str],
        notfound_substrings: List[str],
    ) -> Dict[str, Any]:
        """
        Executes an AWS CLI command to verify that a resource is absent.
        Returns a structured dictionary with absence status and reason.
        """
        rc, stdout, stderr = self._runner(aws_args)

        if rc != 0:
            combined_err = f"{stdout} {stderr}"
            if any(s.lower() in combined_err.lower() for s in notfound_substrings):
                return {
                    "resource": name,
                    "type": resource_type,
                    "status": "ABSENT",
                    "reason": "NOT_FOUND_MATCHED",
                    "details": stderr.strip() or stdout.strip(),
                }
            return {
                "resource": name,
                "type": resource_type,
                "status": "UNEXPECTED_ERROR",
                "reason": f"exit_code_{rc}",
                "details": stderr.strip() or stdout.strip(),
            }

        # Exit code 0 means resource was found and still exists!
        return {
            "resource": name,
            "type": resource_type,
            "status": "STILL_PRESENT",
            "reason": "RESOURCE_EXISTS_LEAK",
            "details": stdout.strip(),
        }

    def check_kms_key_pending_deletion(self, key_arn_or_id: str) -> Dict[str, Any]:
        """Verifies that an owned KMS key is scheduled for deletion."""
        rc, stdout, stderr = self._runner([
            "kms", "describe-key",
            "--key-id", key_arn_or_id,
            "--query", "KeyMetadata.{State:KeyState,DeletionDate:DeletionDate}",
            "--output", "json",
        ])
        if rc != 0:
            if "NotFoundException" in stderr:
                return {"resource": key_arn_or_id, "type": "kms_key", "status": "ABSENT", "reason": "NotFoundException"}
            return {"resource": key_arn_or_id, "type": "kms_key", "status": "UNEXPECTED_ERROR", "reason": stderr.strip()}

        try:
            meta = json.loads(stdout)
            state = meta.get("State")
            del_date = meta.get("DeletionDate")
            if state == "PendingDeletion" and del_date:
                return {
                    "resource": key_arn_or_id,
                    "type": "kms_key",
                    "status": "PENDING_DELETION",
                    "reason": f"PendingDeletion at {del_date}",
                    "deletion_date": del_date,
                }
            return {
                "resource": key_arn_or_id,
                "type": "kms_key",
                "status": "STILL_PRESENT",
                "reason": f"KMS Key State is {state!r}",
            }
        except Exception as e:
            return {"resource": key_arn_or_id, "type": "kms_key", "status": "UNEXPECTED_ERROR", "reason": str(e)}

    def check_secret_pending_deletion(self, secret_arn_or_name: str) -> Dict[str, Any]:
        """Verifies that an owned Secrets Manager secret is scheduled for deletion."""
        rc, stdout, stderr = self._runner([
            "secretsmanager", "describe-secret",
            "--secret-id", secret_arn_or_name,
            "--query", "{DeletedDate:DeletedDate}",
            "--output", "json",
        ])
        if rc != 0:
            if "ResourceNotFoundException" in stderr:
                return {"resource": secret_arn_or_name, "type": "secret", "status": "ABSENT", "reason": "ResourceNotFoundException"}
            return {"resource": secret_arn_or_name, "type": "secret", "status": "UNEXPECTED_ERROR", "reason": stderr.strip()}

        try:
            meta = json.loads(stdout)
            del_date = meta.get("DeletedDate")
            if del_date:
                return {
                    "resource": secret_arn_or_name,
                    "type": "secret",
                    "status": "PENDING_DELETION",
                    "reason": f"DeletedDate set to {del_date}",
                    "deletion_date": del_date,
                }
            return {
                "resource": secret_arn_or_name,
                "type": "secret",
                "status": "STILL_PRESENT",
                "reason": "Secret is active without DeletedDate",
            }
        except Exception as e:
            return {"resource": secret_arn_or_name, "type": "secret", "status": "UNEXPECTED_ERROR", "reason": str(e)}

    def verify_all(self) -> Dict[str, Any]:
        """
        Executes absence verification across all registered resources in inventory.
        Produces structured absence ledger.
        """
        results: Dict[str, Dict[str, Any]] = {}

        # 1. DynamoDB Tables
        for table in self.inventory.get("dynamodb_tables", []):
            results[f"ddb_{table}"] = self.check_resource_absent(
                table,
                "dynamodb_table",
                ["dynamodb", "describe-table", "--table-name", table],
                EXPECTED_ABSENCE_ERRORS["dynamodb_table"],
            )

        # 2. S3 Buckets
        for bucket in self.inventory.get("s3_buckets", []):
            args = ["s3api", "head-bucket", "--bucket", bucket]
            if self.account_id:
                args.extend(["--expected-bucket-owner", self.account_id])
            results[f"s3_{bucket}"] = self.check_resource_absent(
                bucket,
                "s3_bucket",
                args,
                EXPECTED_ABSENCE_ERRORS["s3_bucket"],
            )

        # 3. VPC
        vpc_id = self.inventory.get("vpc_id")
        if vpc_id:
            results[f"vpc_{vpc_id}"] = self.check_resource_absent(
                vpc_id,
                "vpc",
                ["ec2", "describe-vpcs", "--vpc-ids", vpc_id],
                EXPECTED_ABSENCE_ERRORS["vpc"],
            )

        # 4. ALB
        for alb in self.inventory.get("alb_names", []):
            results[f"alb_{alb}"] = self.check_resource_absent(
                alb,
                "alb",
                ["elbv2", "describe-load-balancers", "--names", alb],
                EXPECTED_ABSENCE_ERRORS["alb"],
            )

        # 5. IAM Roles
        for role in self.inventory.get("iam_roles", []):
            results[f"iam_{role}"] = self.check_resource_absent(
                role,
                "iam_role",
                ["iam", "get-role", "--role-name", role],
                EXPECTED_ABSENCE_ERRORS["iam_role"],
            )

        # 6. ACM Certificates
        for cert in self.inventory.get("acm_certificates", []):
            results[f"acm_{cert}"] = self.check_resource_absent(
                cert,
                "acm_certificate",
                ["acm", "describe-certificate", "--certificate-arn", cert],
                EXPECTED_ABSENCE_ERRORS["acm_certificate"],
            )

        # 7. SSM Documents
        for doc in self.inventory.get("ssm_documents", []):
            results[f"ssm_{doc}"] = self.check_resource_absent(
                doc,
                "ssm_document",
                ["ssm", "describe-document", "--name", doc],
                EXPECTED_ABSENCE_ERRORS["ssm_document"],
            )

        # 8. Key Pairs
        for kp in self.inventory.get("key_pairs", []):
            results[f"kp_{kp}"] = self.check_resource_absent(
                kp,
                "key_pair",
                ["ec2", "describe-key-pairs", "--key-names", kp],
                EXPECTED_ABSENCE_ERRORS["key_pair"],
            )

        # 9. CloudWatch Alarms
        for alarm in self.inventory.get("cloudwatch_alarms", []):
            results[f"alarm_{alarm}"] = self.check_resource_absent(
                alarm,
                "cloudwatch_alarm",
                ["cloudwatch", "describe-alarms", "--alarm-names", alarm],
                EXPECTED_ABSENCE_ERRORS["cloudwatch_alarm"],
            )

        # 10. KMS Keys (PendingDeletion check)
        for kms_key in self.inventory.get("kms_keys", []):
            results[f"kms_{kms_key}"] = self.check_kms_key_pending_deletion(kms_key)

        # 11. Secrets Manager Secrets (DeletedDate check)
        for secret in self.inventory.get("secrets", []):
            results[f"secret_{secret}"] = self.check_secret_pending_deletion(secret)

        # Compute summary stats
        total = len(results)
        absent_count = sum(1 for r in results.values() if r["status"] == "ABSENT")
        pending_del_count = sum(1 for r in results.values() if r["status"] == "PENDING_DELETION")
        still_present = [r["resource"] for r in results.values() if r["status"] == "STILL_PRESENT"]
        unexpected_errors = [r for r in results.values() if r["status"] == "UNEXPECTED_ERROR"]

        all_absent = (len(still_present) == 0 and len(unexpected_errors) == 0)

        summary = {
            "timestamp_utc": _iso_utc_now(),
            "all_absent": all_absent,
            "total_checked": total,
            "absent_count": absent_count,
            "pending_deletion_count": pending_del_count,
            "still_present_count": len(still_present),
            "unexpected_error_count": len(unexpected_errors),
            "still_present": still_present,
            "unexpected_errors": unexpected_errors,
            "resources": results,
        }
        return summary

    def export_summary(self, output_path: Path) -> Path:
        """Runs verification and writes structured JSON ledger to output_path."""
        p = Path(output_path)
        p.parent.mkdir(parents=True, exist_ok=True)
        summary = self.verify_all()
        p.write_text(json.dumps(summary, indent=2), encoding="utf-8")
        return p


def main():
    parser = argparse.ArgumentParser(description="Verify AWS teardown absence (Gate E).")
    parser.add_argument("--outputs-json", help="Path to active-outputs.json or terraform outputs.")
    parser.add_argument("--inventory-json", help="Direct inventory JSON file path.")
    parser.add_argument("--output-ledger", default="gate-e-absence-summary.json", help="Output path for summary.")
    parser.add_argument("--profile", default=None, help="AWS CLI profile.")
    parser.add_argument("--region", default="ap-southeast-1", help="AWS Region.")
    parser.add_argument("--dry-run", action="store_true", help="Dry run mode (returns mock absent for testing).")

    args = parser.parse_args()

    cli_runner = None
    if args.dry_run:
        def mock_dry_runner(cmd):
            # In dry-run mode, simulate clean absence for all checks
            if "describe-key" in cmd:
                return 0, json.dumps({"State": "PendingDeletion", "DeletionDate": "2026-12-08T00:00:00Z"}), ""
            if "describe-secret" in cmd:
                return 0, json.dumps({"DeletedDate": "2026-11-10T05:00:00Z"}), ""
            return 255, "", "ResourceNotFoundException 404 Not Found NoSuchBucket LoadBalancerNotFound InvalidVpcID.NotFound NoSuchEntity InvalidDocument InvalidKeyPair.NotFound"
        cli_runner = mock_dry_runner

    if args.outputs_json:
        verifier = TeardownAbsenceVerifier.from_terraform_outputs(
            Path(args.outputs_json),
            profile=args.profile,
            region=args.region,
            cli_runner=cli_runner,
        )
    elif args.inventory_json:
        inv = json.loads(Path(args.inventory_json).read_text(encoding="utf-8"))
        verifier = TeardownAbsenceVerifier(
            inventory=inv,
            profile=args.profile,
            region=args.region,
            cli_runner=cli_runner,
        )
    else:
        print("Error: must provide --outputs-json or --inventory-json", file=sys.stderr)
        return 2

    out_p = Path(args.output_ledger)
    verifier.export_summary(out_p)
    print(f"Teardown absence ledger written to: {out_p}")

    summary = json.loads(out_p.read_text(encoding="utf-8"))
    if summary["all_absent"]:
        print(f"PASS: All {summary['total_checked']} resources verified absent ({summary['absent_count']} absent, {summary['pending_deletion_count']} pending deletion).")
        return 0
    else:
        print(f"FAIL: {summary['still_present_count']} resources still present, {summary['unexpected_error_count']} unexpected errors.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
