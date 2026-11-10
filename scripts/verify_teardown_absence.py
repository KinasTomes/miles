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
- Fail-closed: Empty or unverified inventory evaluates to UNVERIFIED / FAIL.
- CloudWatch describe-alarms returning 0 with empty lists is correctly classified as ABSENT.
- Dynamic extraction covers all owned resources (VPCs, subnets, SGs, endpoints, ALB/TGs,
  roles, instance profiles, trust anchors, profiles, compute instances, volumes, ASGs,
  SSM parameters, KMS keys, secrets, tables, buckets, and alarms).
- Emits structured Gate E ledger artifact with 0600 file permissions and explicit mode.
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
    "vpc_endpoint": ["InvalidVpcEndpointId.NotFound"],
    "instance": ["InvalidInstanceID.NotFound"],
    "volume": ["InvalidVolume.NotFound"],
    "asg": ["AutoScalingGroupNotFound"],
    "trust_anchor": ["ResourceNotFoundException"],
    "rolesanywhere_profile": ["ResourceNotFoundException"],
    "ssm_parameter": ["ParameterNotFound"],
}


def _unwrap_val(d: Any) -> Any:
    if isinstance(d, dict) and "value" in d:
        return d["value"]
    return d


def _extract_id_list(val: Any) -> List[str]:
    res: List[str] = []
    if isinstance(val, dict):
        for v in val.values():
            if v is not None and str(v).strip():
                res.append(str(v).strip())
    elif isinstance(val, list):
        for v in val:
            if v is not None and str(v).strip():
                res.append(str(v).strip())
    elif val is not None and isinstance(val, str) and val.strip():
        res.append(val.strip())
    return res


def _name_from_arn(arn_or_name: str) -> str:
    s = str(arn_or_name).strip()
    if "/" in s:
        return s.split("/")[-1]
    if ":" in s:
        return s.split(":")[-1]
    return s


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
        mode: str = "live",
        cli_runner: Optional[Callable[[List[str]], Tuple[int, str, str]]] = None,
    ):
        self.inventory = inventory
        self.profile = profile
        self.region = region
        self.account_id = account_id or inventory.get("account_id")
        self.mode = mode
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
        mode: str = "live",
        cli_runner: Optional[Callable[[List[str]], Tuple[int, str, str]]] = None,
    ) -> "TeardownAbsenceVerifier":
        """
        Parses active-outputs.json or `terraform output -json` and extracts
        all owned resource identifiers dynamically.
        """
        data = json.loads(Path(outputs_json_path).read_text(encoding="utf-8"))
        inv = cls.extract_inventory_from_outputs(data)
        return cls(inventory=inv, profile=profile, region=region, mode=mode, cli_runner=cli_runner)

    @staticmethod
    def extract_inventory_from_outputs(data: Dict[str, Any]) -> Dict[str, Any]:
        """Extracts owned resource identifiers from nested Terraform output structure."""
        inv: Dict[str, Any] = {
            "dynamodb_tables": [],
            "s3_buckets": [],
            "vpc_id": None,
            "subnets": [],
            "security_groups": [],
            "vpc_endpoints": [],
            "alb_names": [],
            "target_groups": [],
            "iam_roles": [],
            "instance_profiles": [],
            "trust_anchors": [],
            "rolesanywhere_profiles": [],
            "instances": [],
            "volumes": [],
            "asg_names": [],
            "acm_certificates": [],
            "ssm_documents": [],
            "ssm_parameters": [],
            "key_pairs": [],
            "cloudwatch_alarms": [],
            "kms_keys": [],
            "secrets": [],
        }

        def _dedup_add(key: str, items: List[str]) -> None:
            for item in items:
                if item and item not in inv[key]:
                    inv[key].append(item)

        # 1. network_inventory
        net = _unwrap_val(data.get("network_inventory", {}))
        if isinstance(net, dict):
            if net.get("vpc_id"):
                inv["vpc_id"] = str(net["vpc_id"]).strip()
            _dedup_add("subnets", _extract_id_list(net.get("public_subnets")))
            _dedup_add("subnets", _extract_id_list(net.get("private_subnets")))
            _dedup_add("security_groups", _extract_id_list(net.get("security_groups")))
            _dedup_add("vpc_endpoints", _extract_id_list(net.get("gateway_endpoints")))
            _dedup_add("vpc_endpoints", _extract_id_list(net.get("interface_endpoints")))
            if net.get("alb_name"):
                _dedup_add("alb_names", [str(net["alb_name"]).strip()])

        # 2. runtime_inventory
        rt = _unwrap_val(data.get("runtime_inventory", {}))
        if isinstance(rt, dict):
            # Tables
            tables = rt.get("expected_tables", [])
            if isinstance(tables, list):
                _dedup_add("dynamodb_tables", [str(t).strip() for t in tables])
            # Buckets
            if rt.get("data_bucket_name"):
                _dedup_add("s3_buckets", [str(rt["data_bucket_name"]).strip()])
            # KMS Keys
            if rt.get("kms_key_arn"):
                _dedup_add("kms_keys", [str(rt["kms_key_arn"]).strip()])
            # Secrets
            if rt.get("secret_metadata_arn"):
                _dedup_add("secrets", [str(rt["secret_metadata_arn"]).strip()])
            # SSM Parameter
            if rt.get("bootstrap_parameter_arn"):
                param = str(rt["bootstrap_parameter_arn"]).strip()
                _dedup_add("ssm_parameters", [param])
            # Target Groups
            if rt.get("target_group_arn"):
                _dedup_add("target_groups", [str(rt["target_group_arn"]).strip()])
            # IAM Roles
            roles_val = rt.get("iam_roles")
            if roles_val:
                role_names = [_name_from_arn(r) for r in _extract_id_list(roles_val)]
                _dedup_add("iam_roles", role_names)
            # Instance Profiles
            inst_profiles = rt.get("instance_profiles")
            if inst_profiles:
                p_names = [_name_from_arn(p) for p in _extract_id_list(inst_profiles)]
                _dedup_add("instance_profiles", p_names)
            # Compute inventory
            compute = rt.get("compute")
            if isinstance(compute, dict):
                if compute.get("instance_id"):
                    _dedup_add("instances", [str(compute["instance_id"]).strip()])
                if compute.get("data_volume_id"):
                    _dedup_add("volumes", [str(compute["data_volume_id"]).strip()])
                if compute.get("session_document"):
                    _dedup_add("ssm_documents", [str(compute["session_document"]).strip()])

        # 3. edge_provider
        edge = _unwrap_val(data.get("edge_provider", {}))
        if isinstance(edge, dict):
            if edge.get("role_name"):
                _dedup_add("iam_roles", [str(edge["role_name"]).strip()])
            elif edge.get("role_arn"):
                _dedup_add("iam_roles", [_name_from_arn(edge["role_arn"])])
            if edge.get("trust_anchor_id"):
                _dedup_add("trust_anchors", [str(edge["trust_anchor_id"]).strip()])
            elif edge.get("trust_anchor_arn"):
                _dedup_add("trust_anchors", [_name_from_arn(edge["trust_anchor_arn"])])
            if edge.get("profile_id"):
                _dedup_add("rolesanywhere_profiles", [str(edge["profile_id"]).strip()])
            elif edge.get("profile_arn"):
                _dedup_add("rolesanywhere_profiles", [_name_from_arn(edge["profile_arn"])])

        # 4. fleet_contract
        fleet = _unwrap_val(data.get("fleet_contract", {}))
        if isinstance(fleet, dict) and fleet.get("asg_name"):
            _dedup_add("asg_names", [str(fleet["asg_name"]).strip()])

        # 5. alb_contract
        alb_c = _unwrap_val(data.get("alb_contract", {}))
        if isinstance(alb_c, dict):
            if alb_c.get("target_group_arn"):
                _dedup_add("target_groups", [str(alb_c["target_group_arn"]).strip()])

        # 6. alarm_contract
        alarm_c = _unwrap_val(data.get("alarm_contract", {}))
        if isinstance(alarm_c, dict):
            _dedup_add("cloudwatch_alarms", list(alarm_c.keys()))

        # 7. Direct top-level outputs
        if data.get("data_bucket_name"):
            b = _unwrap_val(data["data_bucket_name"])
            if b:
                _dedup_add("s3_buckets", [str(b).strip()])
        if data.get("dynamodb_tables"):
            _dedup_add("dynamodb_tables", _extract_id_list(_unwrap_val(data["dynamodb_tables"])))
        if data.get("cloudwatch_alarms"):
            _dedup_add("cloudwatch_alarms", _extract_id_list(_unwrap_val(data["cloudwatch_alarms"])))
        if data.get("acm_certificates"):
            _dedup_add("acm_certificates", _extract_id_list(_unwrap_val(data["acm_certificates"])))
        if data.get("key_pairs"):
            _dedup_add("key_pairs", _extract_id_list(_unwrap_val(data["key_pairs"])))
        if data.get("ssm_documents"):
            _dedup_add("ssm_documents", _extract_id_list(_unwrap_val(data["ssm_documents"])))

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

    def check_cloudwatch_alarm_absent(self, alarm_name: str) -> Dict[str, Any]:
        """
        Verifies that an alarm is absent.
        aws cloudwatch describe-alarms returns returncode 0 with empty lists
        {'MetricAlarms': [], 'CompositeAlarms': []} when the alarm does not exist.
        """
        rc, stdout, stderr = self._runner([
            "cloudwatch", "describe-alarms",
            "--alarm-names", alarm_name,
            "--output", "json",
        ])
        if rc != 0:
            combined_err = f"{stdout} {stderr}"
            if any(s.lower() in combined_err.lower() for s in EXPECTED_ABSENCE_ERRORS["cloudwatch_alarm"]):
                return {
                    "resource": alarm_name,
                    "type": "cloudwatch_alarm",
                    "status": "ABSENT",
                    "reason": "NOT_FOUND_MATCHED",
                    "details": stderr.strip() or stdout.strip(),
                }
            return {
                "resource": alarm_name,
                "type": "cloudwatch_alarm",
                "status": "UNEXPECTED_ERROR",
                "reason": f"exit_code_{rc}",
                "details": stderr.strip() or stdout.strip(),
            }

        try:
            data = json.loads(stdout)
            metric_alarms = data.get("MetricAlarms", [])
            composite_alarms = data.get("CompositeAlarms", [])
            matching = [
                a for a in (metric_alarms + composite_alarms)
                if a.get("AlarmName") == alarm_name
            ]
            if not matching:
                return {
                    "resource": alarm_name,
                    "type": "cloudwatch_alarm",
                    "status": "ABSENT",
                    "reason": "EMPTY_OR_UNMATCHED_ALARMS_LIST",
                }
            return {
                "resource": alarm_name,
                "type": "cloudwatch_alarm",
                "status": "STILL_PRESENT",
                "reason": "RESOURCE_EXISTS_LEAK",
                "details": f"Found {len(matching)} alarm(s)",
            }
        except Exception as e:
            return {
                "resource": alarm_name,
                "type": "cloudwatch_alarm",
                "status": "UNEXPECTED_ERROR",
                "reason": f"json_decode_error: {e}",
            }

    def check_asg_absent(self, asg_name: str) -> Dict[str, Any]:
        """
        Verifies that an Auto Scaling Group is absent.
        describe-auto-scaling-groups returns returncode 0 and empty AutoScalingGroups list
        when absent.
        """
        rc, stdout, stderr = self._runner([
            "autoscaling", "describe-auto-scaling-groups",
            "--auto-scaling-group-names", asg_name,
            "--output", "json",
        ])
        if rc != 0:
            combined_err = f"{stdout} {stderr}"
            if any(s.lower() in combined_err.lower() for s in EXPECTED_ABSENCE_ERRORS["asg"]):
                return {
                    "resource": asg_name,
                    "type": "asg",
                    "status": "ABSENT",
                    "reason": "NOT_FOUND_MATCHED",
                    "details": stderr.strip() or stdout.strip(),
                }
            return {
                "resource": asg_name,
                "type": "asg",
                "status": "UNEXPECTED_ERROR",
                "reason": f"exit_code_{rc}",
                "details": stderr.strip() or stdout.strip(),
            }

        try:
            data = json.loads(stdout)
            asgs = data.get("AutoScalingGroups", [])
            matching = [g for g in asgs if g.get("AutoScalingGroupName") == asg_name]
            if not matching:
                return {
                    "resource": asg_name,
                    "type": "asg",
                    "status": "ABSENT",
                    "reason": "EMPTY_OR_UNMATCHED_ASG_LIST",
                }
            return {
                "resource": asg_name,
                "type": "asg",
                "status": "STILL_PRESENT",
                "reason": "RESOURCE_EXISTS_LEAK",
                "details": f"ASG {asg_name} still exists",
            }
        except Exception as e:
            return {
                "resource": asg_name,
                "type": "asg",
                "status": "UNEXPECTED_ERROR",
                "reason": f"json_decode_error: {e}",
            }

    def check_instance_absent(self, instance_id: str) -> Dict[str, Any]:
        """
        Verifies that an EC2 instance is terminated or deleted.
        """
        rc, stdout, stderr = self._runner([
            "ec2", "describe-instances",
            "--instance-ids", instance_id,
            "--output", "json",
        ])
        if rc != 0:
            combined_err = f"{stdout} {stderr}"
            if any(s.lower() in combined_err.lower() for s in EXPECTED_ABSENCE_ERRORS["instance"]):
                return {
                    "resource": instance_id,
                    "type": "instance",
                    "status": "ABSENT",
                    "reason": "NOT_FOUND_MATCHED",
                }
            return {
                "resource": instance_id,
                "type": "instance",
                "status": "UNEXPECTED_ERROR",
                "reason": f"exit_code_{rc}",
                "details": stderr.strip() or stdout.strip(),
            }

        try:
            data = json.loads(stdout)
            reservations = data.get("Reservations", [])
            instances = [
                inst for r in reservations
                for inst in r.get("Instances", [])
                if inst.get("InstanceId") == instance_id
            ]
            if not instances:
                return {
                    "resource": instance_id,
                    "type": "instance",
                    "status": "ABSENT",
                    "reason": "INSTANCE_NOT_IN_RESERVATIONS",
                }
            state = instances[0].get("State", {}).get("Name")
            if state == "terminated":
                return {
                    "resource": instance_id,
                    "type": "instance",
                    "status": "ABSENT",
                    "reason": "INSTANCE_TERMINATED",
                }
            return {
                "resource": instance_id,
                "type": "instance",
                "status": "STILL_PRESENT",
                "reason": f"INSTANCE_STATE_{state.upper()}_LEAK",
            }
        except Exception as e:
            return {
                "resource": instance_id,
                "type": "instance",
                "status": "UNEXPECTED_ERROR",
                "reason": f"json_decode_error: {e}",
            }

    def check_volume_absent(self, volume_id: str) -> Dict[str, Any]:
        """
        Verifies that an EBS volume is absent or deleted.
        """
        rc, stdout, stderr = self._runner([
            "ec2", "describe-volumes",
            "--volume-ids", volume_id,
            "--output", "json",
        ])
        if rc != 0:
            combined_err = f"{stdout} {stderr}"
            if any(s.lower() in combined_err.lower() for s in EXPECTED_ABSENCE_ERRORS["volume"]):
                return {
                    "resource": volume_id,
                    "type": "volume",
                    "status": "ABSENT",
                    "reason": "NOT_FOUND_MATCHED",
                }
            return {
                "resource": volume_id,
                "type": "volume",
                "status": "UNEXPECTED_ERROR",
                "reason": f"exit_code_{rc}",
                "details": stderr.strip() or stdout.strip(),
            }

        try:
            data = json.loads(stdout)
            vols = [v for v in data.get("Volumes", []) if v.get("VolumeId") == volume_id]
            if not vols:
                return {
                    "resource": volume_id,
                    "type": "volume",
                    "status": "ABSENT",
                    "reason": "VOLUME_NOT_FOUND",
                }
            state = vols[0].get("State")
            if state in ("deleted", "deleting"):
                return {
                    "resource": volume_id,
                    "type": "volume",
                    "status": "ABSENT",
                    "reason": f"VOLUME_{state.upper()}",
                }
            return {
                "resource": volume_id,
                "type": "volume",
                "status": "STILL_PRESENT",
                "reason": f"VOLUME_STATE_{state.upper()}_LEAK",
            }
        except Exception as e:
            return {
                "resource": volume_id,
                "type": "volume",
                "status": "UNEXPECTED_ERROR",
                "reason": f"json_decode_error: {e}",
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
        Fails closed on empty or unverified inventory.
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

        # 4. Subnets
        for subnet in self.inventory.get("subnets", []):
            results[f"subnet_{subnet}"] = self.check_resource_absent(
                subnet,
                "subnet",
                ["ec2", "describe-subnets", "--subnet-ids", subnet],
                EXPECTED_ABSENCE_ERRORS["subnet"],
            )

        # 5. Security Groups
        for sg in self.inventory.get("security_groups", []):
            results[f"sg_{sg}"] = self.check_resource_absent(
                sg,
                "security_group",
                ["ec2", "describe-security-groups", "--group-ids", sg],
                EXPECTED_ABSENCE_ERRORS["security_group"],
            )

        # 6. VPC Endpoints
        for vpce in self.inventory.get("vpc_endpoints", []):
            results[f"vpce_{vpce}"] = self.check_resource_absent(
                vpce,
                "vpc_endpoint",
                ["ec2", "describe-vpc-endpoints", "--vpc-endpoint-ids", vpce],
                EXPECTED_ABSENCE_ERRORS["vpc_endpoint"],
            )

        # 7. ALB
        for alb in self.inventory.get("alb_names", []):
            results[f"alb_{alb}"] = self.check_resource_absent(
                alb,
                "alb",
                ["elbv2", "describe-load-balancers", "--names", alb],
                EXPECTED_ABSENCE_ERRORS["alb"],
            )

        # 8. Target Groups
        for tg in self.inventory.get("target_groups", []):
            results[f"tg_{tg}"] = self.check_resource_absent(
                tg,
                "target_group",
                ["elbv2", "describe-target-groups", "--target-group-arns", tg],
                EXPECTED_ABSENCE_ERRORS["target_group"],
            )

        # 9. IAM Roles
        for role in self.inventory.get("iam_roles", []):
            results[f"iam_{role}"] = self.check_resource_absent(
                role,
                "iam_role",
                ["iam", "get-role", "--role-name", role],
                EXPECTED_ABSENCE_ERRORS["iam_role"],
            )

        # 10. Instance Profiles
        for prof in self.inventory.get("instance_profiles", []):
            results[f"prof_{prof}"] = self.check_resource_absent(
                prof,
                "iam_instance_profile",
                ["iam", "get-instance-profile", "--instance-profile-name", prof],
                EXPECTED_ABSENCE_ERRORS["iam_instance_profile"],
            )

        # 11. Trust Anchors
        for anchor in self.inventory.get("trust_anchors", []):
            results[f"anchor_{anchor}"] = self.check_resource_absent(
                anchor,
                "trust_anchor",
                ["rolesanywhere", "get-trust-anchor", "--trust-anchor-id", anchor],
                EXPECTED_ABSENCE_ERRORS["trust_anchor"],
            )

        # 12. Roles Anywhere Profiles
        for rap in self.inventory.get("rolesanywhere_profiles", []):
            results[f"rap_{rap}"] = self.check_resource_absent(
                rap,
                "rolesanywhere_profile",
                ["rolesanywhere", "get-profile", "--profile-id", rap],
                EXPECTED_ABSENCE_ERRORS["rolesanywhere_profile"],
            )

        # 13. EC2 Instances
        for inst in self.inventory.get("instances", []):
            results[f"inst_{inst}"] = self.check_instance_absent(inst)

        # 14. EBS Volumes
        for vol in self.inventory.get("volumes", []):
            results[f"vol_{vol}"] = self.check_volume_absent(vol)

        # 15. Auto Scaling Groups
        for asg in self.inventory.get("asg_names", []):
            results[f"asg_{asg}"] = self.check_asg_absent(asg)

        # 16. SSM Parameters
        for param in self.inventory.get("ssm_parameters", []):
            results[f"param_{param}"] = self.check_resource_absent(
                param,
                "ssm_parameter",
                ["ssm", "get-parameter", "--name", param],
                EXPECTED_ABSENCE_ERRORS["ssm_parameter"],
            )

        # 17. ACM Certificates
        for cert in self.inventory.get("acm_certificates", []):
            results[f"acm_{cert}"] = self.check_resource_absent(
                cert,
                "acm_certificate",
                ["acm", "describe-certificate", "--certificate-arn", cert],
                EXPECTED_ABSENCE_ERRORS["acm_certificate"],
            )

        # 18. SSM Documents
        for doc in self.inventory.get("ssm_documents", []):
            results[f"ssm_{doc}"] = self.check_resource_absent(
                doc,
                "ssm_document",
                ["ssm", "describe-document", "--name", doc],
                EXPECTED_ABSENCE_ERRORS["ssm_document"],
            )

        # 19. Key Pairs
        for kp in self.inventory.get("key_pairs", []):
            results[f"kp_{kp}"] = self.check_resource_absent(
                kp,
                "key_pair",
                ["ec2", "describe-key-pairs", "--key-names", kp],
                EXPECTED_ABSENCE_ERRORS["key_pair"],
            )

        # 20. CloudWatch Alarms
        for alarm in self.inventory.get("cloudwatch_alarms", []):
            results[f"alarm_{alarm}"] = self.check_cloudwatch_alarm_absent(alarm)

        # 21. KMS Keys (PendingDeletion check)
        for kms_key in self.inventory.get("kms_keys", []):
            results[f"kms_{kms_key}"] = self.check_kms_key_pending_deletion(kms_key)

        # 22. Secrets Manager Secrets (DeletedDate check)
        for secret in self.inventory.get("secrets", []):
            results[f"secret_{secret}"] = self.check_secret_pending_deletion(secret)

        total = len(results)

        # Fail-closed guard: empty or unverified inventory evaluates to FAIL
        if total == 0:
            return {
                "timestamp_utc": _iso_utc_now(),
                "mode": self.mode,
                "status": "UNVERIFIED",
                "all_absent": False,
                "error": "EMPTY_OR_UNVERIFIED_INVENTORY",
                "total_checked": 0,
                "absent_count": 0,
                "pending_deletion_count": 0,
                "still_present_count": 0,
                "unexpected_error_count": 0,
                "still_present": [],
                "unexpected_errors": [],
                "resources": {},
            }

        absent_count = sum(1 for r in results.values() if r["status"] == "ABSENT")
        pending_del_count = sum(1 for r in results.values() if r["status"] == "PENDING_DELETION")
        still_present = [r["resource"] for r in results.values() if r["status"] == "STILL_PRESENT"]
        unexpected_errors = [r for r in results.values() if r["status"] == "UNEXPECTED_ERROR"]

        all_absent = (len(still_present) == 0 and len(unexpected_errors) == 0)

        summary = {
            "timestamp_utc": _iso_utc_now(),
            "mode": self.mode,
            "status": "PASS" if all_absent else "FAIL",
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
        """Runs verification and writes structured JSON ledger to output_path with 0600 permissions."""
        p = Path(output_path)
        p.parent.mkdir(parents=True, exist_ok=True)
        summary = self.verify_all()
        payload = json.dumps(summary, indent=2) + "\n"

        fd = os.open(str(p), os.O_CREAT | os.O_WRONLY | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as f:
            f.write(payload)
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
    mode = "live"
    if args.dry_run:
        mode = "dry-run"

        def mock_dry_runner(cmd):
            # In dry-run mode, simulate clean absence for all checks
            if "describe-key" in cmd:
                return 0, json.dumps({"State": "PendingDeletion", "DeletionDate": "2026-12-08T00:00:00Z"}), ""
            if "describe-secret" in cmd:
                return 0, json.dumps({"DeletedDate": "2026-11-10T05:00:00Z"}), ""
            if "describe-alarms" in cmd:
                return 0, json.dumps({"MetricAlarms": [], "CompositeAlarms": []}), ""
            if "describe-auto-scaling-groups" in cmd:
                return 0, json.dumps({"AutoScalingGroups": []}), ""
            if "describe-instances" in cmd:
                return 255, "", "InvalidInstanceID.NotFound: The instance ID does not exist"
            if "describe-volumes" in cmd:
                return 255, "", "InvalidVolume.NotFound: The volume does not exist"
            return 255, "", "ResourceNotFoundException 404 Not Found NoSuchBucket LoadBalancerNotFound TargetGroupNotFound InvalidVpcID.NotFound InvalidSubnetID.NotFound InvalidGroup.NotFound InvalidVpcEndpointId.NotFound NoSuchEntity InvalidDocument InvalidKeyPair.NotFound ParameterNotFound"

        cli_runner = mock_dry_runner

    if args.outputs_json:
        verifier = TeardownAbsenceVerifier.from_terraform_outputs(
            Path(args.outputs_json),
            profile=args.profile,
            region=args.region,
            mode=mode,
            cli_runner=cli_runner,
        )
    elif args.inventory_json:
        inv = json.loads(Path(args.inventory_json).read_text(encoding="utf-8"))
        verifier = TeardownAbsenceVerifier(
            inventory=inv,
            profile=args.profile,
            region=args.region,
            mode=mode,
            cli_runner=cli_runner,
        )
    else:
        print("Error: must provide --outputs-json or --inventory-json", file=sys.stderr)
        return 2

    out_p = Path(args.output_ledger)
    verifier.export_summary(out_p)
    print(f"Teardown absence ledger written to: {out_p}")

    summary = json.loads(out_p.read_text(encoding="utf-8"))
    if summary.get("status") == "UNVERIFIED":
        print(f"FAIL: Teardown verification status is UNVERIFIED ({summary.get('error')}).", file=sys.stderr)
        return 1
    if summary["all_absent"]:
        print(f"PASS: All {summary['total_checked']} resources verified absent ({summary['absent_count']} absent, {summary['pending_deletion_count']} pending deletion). Mode: {summary['mode']}.")
        return 0
    else:
        print(f"FAIL: {summary['still_present_count']} resources still present, {summary['unexpected_error_count']} unexpected errors.", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
