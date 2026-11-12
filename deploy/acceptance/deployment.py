"""Three saved-plan stages using the repository's existing dev and acceptance modules."""

import datetime as dt
import hashlib
import json
import shutil

from deploy.acceptance.common import digest, now, private, require


def configuration_hash(r):
    h = hashlib.sha256()
    for f in sorted((r.file("source/infra/terraform/cloud")).rglob("*.tf")):
        h.update(str(f.relative_to(r.file("source"))).encode())
        h.update(f.read_bytes())
    return h.hexdigest()


def preflight(r):
    r.identity()
    from deploy.acceptance import iam

    iam.baseline(r)
    baseline = r.read("account-baseline.json")["resources"]
    require(
        "miles-dev-data-" + r.c["account_id"] + "-" + r.c["region"]
        not in baseline["buckets"],
        "Existing dev data bucket: do not adopt another installation",
    )
    require(
        not set(baseline["roles"])
        & {
            "miles-dev-gateway",
            "miles-dev-edge",
            "miles-dev-jobs",
            "miles-dev-acceptance-edge",
            "miles-dev-acceptance-client",
        },
        "Existing runtime roles: reconcile old environment before starting another",
    )
    require(
        "alias/miles-dev-runtime" not in baseline["kms_aliases"],
        "Existing runtime KMS alias: preserve and reconcile before starting another environment",
    )
    r.aws(
        "s3api",
        "head-bucket",
        "--bucket",
        r.c["backend_bucket"],
        "--expected-bucket-owner",
        r.c["account_id"],
    )
    ami = r.aws("ec2", "describe-images", "--image-ids", r.c["ami_id"])["Images"]
    require(
        len(ami) == 1
        and ami[0]["ImageOwnerAlias"] == "amazon"
        and ami[0]["Architecture"] == "arm64"
        and ami[0]["Name"].startswith("al2023-ami-2023.")
        and ami[0]["RootDeviceType"] == "ebs",
        "AMI is not a pinned Amazon AL2023 ARM64 EBS image",
    )
    r.write("ami-proof.json", ami[0])
    r.tf(
        "init",
        "-input=false",
        "-reconfigure",
        "-lockfile=readonly",
        "-backend-config=" + str(r.file("backend.hcl")),
        log="terraform-init.log",
    )
    state = json.loads(r.tf("show", "-json"))

    def managed(module):
        return [
            x for x in module.get("resources", []) if x.get("mode") == "managed"
        ] + [x for child in module.get("child_modules", []) for x in managed(child)]

    require(
        not managed(state.get("values", {}).get("root_module", {})),
        "This kit requires an empty disposable workload state; do not adopt or destroy an existing installation",
    )
    r.write("empty-state-proof.json", state, new=True)
    if not r.c.get("existing_autoscaling_role_arn"):
        # Existing SLR ownership is captured before activation; preserve it outside Terraform.
        roles = r.aws("iam", "list-roles", iam=True)["Roles"]
        roles = [
            x
            for x in roles
            if x["RoleName"] == "AWSServiceRoleForAutoScaling_miles-dev"
        ]
        if roles:
            variables = r.read("foundation.tfvars.json")
            variables["existing_autoscaling_role_arn"] = roles[0]["Arn"]
            r.write("foundation.tfvars.json", variables)
    r.event("preflight", "verified empty workload state and pinned AMI")


def plan(r, stage):
    require(stage in ("foundation", "hosts", "active"), "Unknown stage")
    if stage == "foundation":
        require(r.file("empty-state-proof.json").exists(), "Run preflight first")
        varfile = "foundation.tfvars.json"
    elif stage == "hosts":
        require(r.file("foundation-outputs.json").exists(), "Apply foundation first")
        variables = r.read("foundation.tfvars.json")
        variables.update(paused=False, enable_acceptance_nodes=True)
        r.write("hosts.tfvars.json", variables)
        varfile = "hosts.tfvars.json"
    else:
        require(
            r.file("bootstrap-confirmed.json").exists(),
            "Bootstrap must be confirmed before starting Cloud",
        )
        variables = r.read("hosts.tfvars.json")
        release = r.read("release.json")
        out = r.outputs("foundation")
        uploaded = r.read("release-upload.json")
        key = "releases/" + release["sha256"] + "/miles-linux-arm64.tar.gz"
        r.aws(
            "s3api",
            "get-object",
            "--bucket",
            out["runtime_inventory"]["data_bucket_name"],
            "--key",
            key,
            "--version-id",
            uploaded["VersionId"],
            "--expected-bucket-owner",
            r.c["account_id"],
            str(r.file("release-readback.tar.gz")),
        )
        require(
            digest(r.file("release-readback.tar.gz")) == release["sha256"],
            "Release key/version/hash mismatch",
        )
        variables.update(
            enable_asg=True,
            enable_alb=True,
            release_sha256=release["sha256"],
            release_version_id=uploaded["VersionId"],
            certificate_arn=r.read("certificate.json")["CertificateArn"],
        )
        r.write("active.tfvars.json", variables)
        varfile = "active.tfvars.json"
    tag = now().strftime("%Y%m%dT%H%M%S%f")
    prefix = "plans/" + stage + "/" + tag
    saved = r.file(prefix + ".tfplan")
    saved.parent.mkdir(parents=True, mode=0o700, exist_ok=True)
    r.tf(
        "plan",
        "-input=false",
        "-lock-timeout=60s",
        "-var-file=" + str(r.file(varfile)),
        "-out=" + str(saved),
        log=prefix + ".log",
    )
    data = json.loads(r.tf("show", "-json", saved))
    r.write(prefix + ".json", data)
    changes = [
        x
        for x in data.get("resource_changes", [])
        if x.get("mode", "managed") == "managed" and x["change"]["actions"] != ["no-op"]
    ]
    require(
        all("delete" not in x["change"]["actions"] for x in changes),
        "Creation stage cannot delete/replace resources",
    )
    for x in changes:
        after = x["change"].get("after") or {}
        if x["type"] == "aws_instance":
            require(
                not after.get("associate_public_ip_address"),
                "Test nodes must be private",
            )
        if x["type"] == "aws_lb":
            require(after.get("internal") is True, "ALB must be internal")
        if x["type"] == "aws_autoscaling_group":
            require(
                after["desired_capacity"] == 2 and after["max_size"] <= 3,
                "Unexpected fleet capacity",
            )
        require(
            x["type"] != "aws_nat_gateway", "Use existing private endpoint topology"
        )
    receipt = {
        "stage": stage,
        "plan": prefix + ".tfplan",
        "sha256": digest(saved),
        "config_sha256": configuration_hash(r),
        "vars": varfile,
        "vars_sha256": digest(r.file(varfile)),
        "changes": len(changes),
        "created_at": now().isoformat(),
    }
    r.write(stage + "-plan.json", receipt)
    # Deterministic IAM baseline is an artifact for review, never uploaded/attached automatically.
    executable = shutil.which("uvx")
    require(
        executable,
        "Install uv (uvx) for deterministic IAM plan analysis, then run plan again",
    )
    raw = r.command(
        [
            executable,
            "iam-policy-autopilot@0.4.0",
            "generate-policies",
            r.file(prefix + ".json"),
            "--account",
            r.c["account_id"],
            "--region",
            r.c["region"],
            "--pretty",
        ],
        timeout=600,
        log=prefix + "-autopilot.log",
    )
    private(r.file(prefix + "-autopilot.json"), raw.decode())
    print(json.dumps(receipt, indent=2))
    r.event(
        "plan-" + stage,
        "saved, checked no deletes; inspect plan and IAM/cost before apply",
    )


def apply(r, stage):
    require(
        r.read("iam-activation.json")["status"] == "active",
        "Activate reviewed IAM first",
    )
    receipt = r.read(stage + "-plan.json")
    require(
        digest(r.file(receipt["plan"])) == receipt["sha256"]
        and configuration_hash(r) == receipt["config_sha256"]
        and digest(r.file(receipt["vars"])) == receipt["vars_sha256"],
        "Plan/config changed since review",
    )
    require(
        now() - dt.datetime.fromisoformat(receipt["created_at"])
        < dt.timedelta(minutes=45),
        "Plan is stale; refresh/review",
    )
    require(
        not r.file(stage + "-outputs.json").exists(),
        "Stage receipt exists; reconcile instead of replaying apply",
    )
    if not r.file("paid-window.json").exists():
        start = now()
        r.write(
            "paid-window.json",
            {
                "started_at": start.isoformat(),
                "cleanup_by": (start + dt.timedelta(hours=r.c["hours"])).isoformat(),
                "max_usd": r.c["max_usd"],
                "hard_billing_cap": False,
            },
            new=True,
        )
    r.live_window()
    r.write(stage + "-apply-intent.json", receipt, new=True)
    r.tf(
        "apply",
        "-input=false",
        "-lock-timeout=60s",
        r.file(receipt["plan"]),
        log=stage + "-apply.log",
    )
    capture_applied(r, stage)


def capture_applied(r, stage):
    r.write(stage + "-outputs.json", json.loads(r.tf("output", "-json")))
    r.write(stage + "-state.json", json.loads(r.tf("show", "-json")))
    if stage == "foundation":
        inventory = r.outputs("foundation")["runtime_inventory"]
        version = r.aws(
            "s3api",
            "get-bucket-versioning",
            "--bucket",
            inventory["data_bucket_name"],
            "--expected-bucket-owner",
            r.c["account_id"],
        )
        require(version.get("Status") == "Enabled", "Data bucket is not versioned")
        r.write(
            "versioning.json",
            {
                "observed_at": now().isoformat(),
                "write_after": (now() + dt.timedelta(minutes=15)).isoformat(),
            },
        )
    if stage == "active":
        r.aws(
            "dynamodb",
            "update-table",
            "--table-name",
            "miles-dev-Chunks",
            "--on-demand-throughput",
            json.dumps({"MaxReadRequestUnits": 512, "MaxWriteRequestUnits": 128}),
        )
        caps = {}
        for table in r.outputs("active")["runtime_inventory"]["expected_tables"]:
            item = r.aws("dynamodb", "describe-table", "--table-name", table)["Table"]
            caps[table] = item["OnDemandThroughput"]
            require(
                caps[table]["MaxReadRequestUnits"]
                == (512 if table.endswith("-Chunks") else 128)
                and caps[table]["MaxWriteRequestUnits"] == 128,
                "Native caps differ from review",
            )
        r.write("caps.json", caps)
        target = r.outputs("active")["runtime_inventory"]["target_group_arn"]
        r.wait(
            "two healthy Cloud targets",
            lambda: (
                len(
                    [
                        x
                        for x in r.aws(
                            "elbv2",
                            "describe-target-health",
                            "--target-group-arn",
                            target,
                        )["TargetHealthDescriptions"]
                        if x["TargetHealth"]["State"] == "healthy"
                    ]
                )
                == 2
            ),
            900,
        )
    r.event("apply-" + stage, "saved plan applied; outputs retained")


def provision_material(r):
    r.identity()
    r.live_window()
    inventory = r.outputs("foundation")["runtime_inventory"]
    after = dt.datetime.fromisoformat(r.read("versioning.json")["write_after"])
    require(
        now() >= after, "Wait for recorded S3 versioning propagation before first write"
    )
    if not r.file("secret-provision.json").exists():
        metadata = r.aws(
            "secretsmanager",
            "describe-secret",
            "--secret-id",
            inventory["secret_metadata_arn"],
        )
        material = r.file("runtime-secret.json")
        import base64, secrets

        if not material.exists():
            require(
                not metadata.get("VersionIdsToStages"),
                "Secret already contains data; reconcile before overwrite",
            )
            r.write(
                "runtime-secret.json",
                {
                    "key_id": "acceptance-" + secrets.token_hex(8),
                    "seal_key": base64.b64encode(secrets.token_bytes(32)).decode(),
                    "cursor_key": base64.b64encode(secrets.token_bytes(32)).decode(),
                },
                new=True,
            )
        token = digest(material)
        # ClientRequestToken binds a retry to the exact same secret version, never a regenerated key.
        value = r.aws(
            "secretsmanager",
            "put-secret-value",
            "--secret-id",
            inventory["secret_metadata_arn"],
            "--client-request-token",
            token,
            "--secret-string",
            "file://" + str(material),
        )
        r.write("secret-provision.json", value)
    release = r.read("release.json")
    require(
        digest(r.file("release.tar.gz")) == release["sha256"], "Release archive changed"
    )
    if not r.file("release-upload.json").exists():
        r.write(
            "release-upload.json",
            r.aws(
                "s3api",
                "put-object",
                "--bucket",
                inventory["data_bucket_name"],
                "--key",
                "releases/" + release["sha256"] + "/miles-linux-arm64.tar.gz",
                "--body",
                str(r.file("release.tar.gz")),
                "--server-side-encryption",
                "aws:kms",
                "--ssekms-key-id",
                inventory["kms_key_arn"],
                "--expected-bucket-owner",
                r.c["account_id"],
            ),
        )
    require(
        r.read("release-upload.json").get("VersionId") not in (None, "null"),
        "Versioned release upload required",
    )
    if not r.file("certificate.json").exists():
        require(
            not r.file("certificate-intent.json").exists(),
            "Unknown ACM import outcome; reconcile tagged certificate before retry",
        )
        r.write(
            "certificate-intent.json", {"sha256": digest(r.file("cloud.pem"))}, new=True
        )
        r.write(
            "certificate.json",
            r.aws(
                "acm",
                "import-certificate",
                "--certificate",
                "fileb://" + str(r.file("cloud.pem")),
                "--private-key",
                "fileb://" + str(r.file("cloud-key.pem")),
                "--certificate-chain",
                "fileb://" + str(r.file("tls-ca.pem")),
                "--tags",
                "Key=Project,Value=Miles",
                "Key=Environment,Value=dev",
                "Key=AcceptanceRun,Value=" + r.id,
            ),
        )
    status = r.aws(
        "acm",
        "describe-certificate",
        "--certificate-arn",
        r.read("certificate.json")["CertificateArn"],
    )["Certificate"]["Status"]
    require(status == "ISSUED", "Imported certificate is not ISSUED")
    r.event(
        "material",
        "shared secret, immutable release version and private test ACM ready",
    )


def recover_apply(r, stage):
    r.identity()
    require(
        r.file(stage + "-apply-intent.json").exists()
        and not r.file(stage + "-outputs.json").exists(),
        "No unresolved apply intent",
    )
    receipt = r.read(stage + "-apply-intent.json")
    require(digest(r.file(receipt["plan"])) == receipt["sha256"], "Saved plan changed")
    from scripts.freeze_terraform_inventory import extract_resources

    state = json.loads(r.tf("show", "-json"))
    have = {x["address"] for x in extract_resources(state)}
    wanted = json.loads(r.tf("show", "-json", r.file(receipt["plan"])))
    expected = {
        x["address"]
        for x in wanted["resource_changes"]
        if x.get("mode", "managed") == "managed"
        and x["change"]["actions"] != ["delete"]
    }
    require(have == expected, "Apply is partial or unknown; do not mark it complete")
    capture_applied(r, stage)
    r.event("recover-apply", "complete state reconciled without replaying apply")
