#!/usr/bin/env python3
"""Render private Terraform/backend/IAM inputs without AWS calls or applying changes."""
import argparse
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re
import shutil
import tempfile


CLOUD = Path(__file__).resolve().parents[1]
REPO = CLOUD.parents[2]


def require(pattern, value, name):
    if not isinstance(value, str) or not re.fullmatch(pattern, value):
        raise ValueError("Invalid " + name)
    return value


def render(configuration):
    if not isinstance(configuration, dict) or not isinstance(configuration.get("operator"), dict):
        raise ValueError("Installation and operator must be objects")
    values = dict(configuration)
    operator = values.pop("operator")
    deployment = values.pop("deployment", {})
    if not isinstance(deployment, dict) or deployment.keys() - {"max_cost_usd", "max_active_minutes"}:
        raise ValueError("Unknown deployment field")
    for key, value in deployment.items():
        if type(value) not in (int, float) or not math.isfinite(value) or value <= 0:
            raise ValueError("Deployment limits must be positive numbers")
    known_variables = set(re.findall(r'variable "([a-z0-9_]+)"', (CLOUD / "environments/dev/variables.tf").read_text()))
    if values.keys() - known_variables or operator.keys() - {"user_arn", "mfa_arn", "deploy_role_arn", "terraform_profile", "state_key", "audit_expires_at", "cleanup_expires_at", "utc_reference", "login_profile", "admin_profile"}:
        raise ValueError("Unknown installation/operator field")
    account = require(r"[0-9]{12}", values["account_id"], "account_id")
    region = require(r"(af|ap|ca|eu|il|me|mx|sa|us)-[a-z]+-[0-9]+", values.get("region", "ap-southeast-1"), "region")
    name = require(r"[a-z][a-z0-9-]{1,22}[a-z0-9]", values.get("installation_name", "miles-dev"), "installation_name")
    environment = require(r"[a-z][a-z0-9-]{0,19}", values.get("environment", "dev"), "environment")
    owner = values["owner"]
    if not isinstance(owner, str) or not owner.strip() or len(owner) > 128:
        raise ValueError("Invalid owner")
    path = require(r"/([A-Za-z0-9_+=,.@-]+/)*", values.get("runtime_iam_path", "/miles/dev/"), "runtime_iam_path")
    if len(path) > 128:
        raise ValueError("Invalid runtime_iam_path")
    boundary = require(r"arn:aws:iam::" + account + r":policy/[A-Za-z0-9_+=,.@/-]+", values["runtime_permissions_boundary_arn"], "runtime_permissions_boundary_arn")
    user = require(r"arn:aws:iam::" + account + r":user/[A-Za-z0-9_+=,.@/-]+", operator["user_arn"], "operator.user_arn")
    mfa = require(r"arn:aws:iam::" + account + r":mfa/[A-Za-z0-9_+=,.@/-]+", operator["mfa_arn"], "operator.mfa_arn")
    deploy = require(r"arn:aws:iam::" + account + r":role/[A-Za-z0-9_+=,.@/-]+", operator["deploy_role_arn"], "operator.deploy_role_arn")
    runtime_names = {name + "-" + suffix for suffix in ("gateway", "jobs", "edge", "edge-bootstrap")}
    if deploy.rsplit("/", 1)[-1] in runtime_names:
        raise ValueError("Deploy role name must be separate from runtime roles")
    profile = require(r"[A-Za-z0-9_-]+", operator["terraform_profile"], "operator.terraform_profile")
    for key in ("login_profile", "admin_profile"):
        if key in operator:
            require(r"[A-Za-z0-9_-]+", operator[key], "operator." + key)
    state_key = require(r"[A-Za-z0-9_./-]+", operator.get("state_key", "cloud/" + environment + "/terraform.tfstate"), "operator.state_key")
    if any(part in ("", ".", "..") for part in state_key.split("/")):
        raise ValueError("Invalid operator.state_key")
    ami = require(r"ami-[0-9a-f]{17}", values["ami_id"], "ami_id")
    instance_type = require(r"[a-z0-9-]+\.[a-z0-9]+", values.get("instance_type", "t4g.small"), "instance_type")
    fleet_type = values.get("fleet_instance_type")
    if fleet_type is not None:
        require(r"[a-z0-9-]+\.[a-z0-9]+", fleet_type, "fleet_instance_type")
    secret = values.get("runtime_secret_name") or name + "/runtime-secrets"
    require(r"[A-Za-z0-9/_+=.@-]+", secret, "runtime_secret_name")
    common_name = require(r"[A-Za-z0-9][A-Za-z0-9_.-]{0,63}", values.get("edge_certificate_common_name", "miles-edge"), "edge_certificate_common_name")
    if values.get("deploy_role_arn") is not None:
        raise ValueError("Use the active deploy login session and deploy_role_arn=null")
    # Persist defaults so Terraform, IAM and backend cannot resolve them differently.
    values.update(region=region, installation_name=name, environment=environment,
                  runtime_iam_path=path, instance_type=instance_type,
                  edge_certificate_common_name=common_name, deploy_role_arn=None)
    state_bucket = name + "-tfstate-" + account + "-" + region
    if len(state_bucket) > 63 or len(name + "-data-" + account + "-" + region) > 63:
        raise ValueError("Installation/Region combination exceeds S3 bucket name limits")
    replacements = {
        "ACCOUNT_ID": account, "REGION": region, "INSTALLATION_NAME": name,
        "ENVIRONMENT": environment, "RUNTIME_IAM_PATH": path, "BOUNDARY_ARN": boundary,
        "OPERATOR_USER_ARN": user, "OPERATOR_MFA_ARN": mfa, "DEPLOY_ROLE_ARN": deploy,
        "STATE_BUCKET": state_bucket, "STATE_KEY": state_key, "AMI_ID": ami,
        "INSTANCE_TYPE": instance_type, "RUNTIME_SECRET_NAME": secret,
        "EDGE_INSTANCE_TYPE": require(r"[a-z0-9-]+\.[a-z0-9]+", values.get("edge_instance_type", "t4g.small"), "edge_instance_type"),
    }
    expires = operator.get("audit_expires_at")
    if expires is not None:
        require(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", expires, "operator.audit_expires_at")
        if datetime.fromisoformat(expires.replace("Z", "+00:00")) <= datetime.now(timezone.utc):
            raise ValueError("Audit grant must expire in the future")
        replacements["EXPIRES_AT"] = expires
    cleanup_expires = operator.get("cleanup_expires_at")
    if cleanup_expires is not None:
        require(r"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z", cleanup_expires, "cleanup_expires_at")
        remaining = datetime.fromisoformat(cleanup_expires.replace("Z", "+00:00")) - datetime.now(timezone.utc)
        if not 0 < remaining.total_seconds() <= 4 * 3600:
            raise ValueError("Cleanup grant needs an explicit window of at most four hours")
        replacements["CLEANUP_EXPIRES_AT"] = cleanup_expires
    tokens = re.compile(r"\{\{([A-Z_]+)\}\}")
    output = {
        "installation.tfvars.json": values,
        "bootstrap-state.tfvars.json": {key: values[key] for key in ("account_id", "region", "installation_name", "environment", "owner")},
    }
    for template in sorted((CLOUD / "bootstrap/operator").glob("*.json.tmpl")):
        if template.name == "teardown-audit.json.tmpl" and expires is None:
            continue
        if template.name == "deploy-cleanup.json.tmpl" and cleanup_expires is None:
            continue
        # One replacement pass keeps user-supplied values out of template evaluation.
        policy = json.loads(tokens.sub(lambda match: replacements[match.group(1)], template.read_text()))
        output["iam/" + template.name.removesuffix(".tmpl")] = policy
    # The fleet can launch only its selected type, independently of single-node compute.
    if fleet_type is not None:
        for statement in output["iam/deploy-compute.json"]["Statement"]:
            condition = statement.get("Condition", {}).get("StringEquals", {})
            if "ec2:InstanceType" in condition:
                condition["ec2:InstanceType"] = sorted({instance_type, fleet_type})
    output["backend.hcl"] = (
        f'bucket = "{state_bucket}"\nkey = "{state_key}"\nregion = "{region}"\n'
        f'encrypt = true\nuse_lockfile = true\nprofile = "{profile}"\n'
    )
    # Shard complete deploy permissions within default IAM attachment/document quotas.
    documents = {}
    statements = []
    for file, document in output.items():
        if file.startswith("iam/deploy-"):
            for statement in document["Statement"]:
                candidate = {"Version": "2012-10-17", "Statement": statements + [statement]}
                if len(json.dumps({"Version":"2012-10-17", "Statement":[statement]}, separators=(",", ":"))) > 6000:
                    raise ValueError("A deploy statement exceeds the managed policy document limit")
                if len(json.dumps(candidate, separators=(",", ":"))) > 6000:
                    documents[f"{len(documents) + 1:02d}"] = json.dumps({"Version": "2012-10-17", "Statement": statements})
                    statements = []
                statements.append(statement)
    if statements:
        documents[f"{len(documents) + 1:02d}"] = json.dumps({"Version": "2012-10-17", "Statement": statements})
    if len(documents) > 9:
        raise ValueError("Deploy policies exceed the default role attachment quota")
    output["operator.tfvars.json"] = {
        **output["bootstrap-state.tfvars.json"], "operator_user_arn": user,
        "deploy_role_arn": deploy, "boundary_arn": boundary,
        "boundary_document": json.dumps(output["iam/runtime-boundary.json"]),
        "user_document": json.dumps(output["iam/user-policy.json"]),
        "trust_document": json.dumps(output["iam/trust-policy.json"]),
        "deploy_documents": documents,
    }
    return output


def write(configuration, output):
    destination = output.absolute()
    if destination.resolve().is_relative_to(REPO) or destination.exists() or destination.is_symlink():
        raise ValueError("Output must be a new directory outside the repository")
    documents = render(configuration)
    # Build the complete private set first. Failed rendering never leaves partial output.
    temporary = Path(tempfile.mkdtemp(prefix=".miles-installation-", dir=destination.parent))
    try:
        for name, document in documents.items():
            file = temporary / name
            file.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            data = document if isinstance(document, str) else json.dumps(document, indent=2) + "\n"
            with open(file, "x", encoding="utf-8", opener=lambda path, flags: os.open(path, flags, 0o600)) as stream:
                stream.write(data)
        # mkdir reserves the destination without replacing another process's output.
        destination.mkdir(mode=0o700)
        # Sibling directories share a filesystem, so publication is one atomic rename.
        temporary.replace(destination)
    finally:
        if temporary.exists():
            shutil.rmtree(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--config", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    arguments = parser.parse_args()
    try:
        write(json.loads(arguments.config.read_text()), arguments.out)
    except (ValueError, KeyError, TypeError, OSError):
        # Configuration and filesystem errors may contain private inputs.
        raise SystemExit("Render failed. Check installation fields and use a new private output directory.") from None
    print("Rendered private Terraform/backend/IAM inputs. Review before use. No AWS calls made.")


if __name__ == "__main__":
    main()
