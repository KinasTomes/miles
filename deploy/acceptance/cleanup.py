"""Freeze ownership, retain failed-run inputs, delete synthetic resources and audit."""

import base64
import datetime as dt
import hashlib
import json
import shutil
import sys
import time

from deploy.acceptance.common import ROOT, digest, now, private, require

sys.path.insert(0, str(ROOT))
from scripts.freeze_terraform_inventory import augment_inventory, extract_resources
from scripts.prepare_teardown_manifest import build_manifest
from scripts.verify_teardown_absence import TeardownAbsenceVerifier
from deploy.acceptance import iam


def freeze(r):
    r.identity()
    if r.file("direct-create-intent.json").exists():
        require(r.file("direct-created.json").exists(), "Direct bucket create unresolved; reconcile ownership before freeze")
        r.write("frozen/direct-comparator.json", r.read("direct-created.json"), new=True)
    require(
        not r.file("frozen/manifest.json").exists(),
        "Frozen ownership already recorded; do not replace it",
    )
    state = r.tf("show", "-json")
    outputs = json.loads(r.tf("output", "-json"))
    inventory = TeardownAbsenceVerifier.extract_inventory_from_outputs(outputs)
    inventory = augment_inventory(extract_resources(json.loads(state)), inventory)
    if r.file("certificate.json").exists():
        inventory["acm_certificates"] = [r.read("certificate.json")["CertificateArn"]]
    for name in [
        "AWSServiceRoleForElasticLoadBalancing",
        "AWSServiceRoleForRolesAnywhere",
    ]:
        if (
            r.file("iam/" + name + ".json").exists()
            and r.read("iam/" + name + ".json")["created"]
        ):
            inventory.setdefault("iam_roles", []).append(name)
    auditor = r.read("names.json")["auditor"]
    inventory.setdefault("iam_roles", []).append(auditor)
    require(
        inventory.get("vpc_id"),
        "No owned VPC in state: reconcile partial apply with account baseline before cleanup",
    )
    decoded = {k: v["value"] for k, v in outputs.items()}
    nodes = decoded.get("acceptance_nodes", {})
    args = [
        "python3",
        ROOT / "scripts/capture_runtime_inventory.py",
        "--profile",
        "acceptance-audit",
        "--region",
        r.c["region"],
        "--account-id",
        r.c["account_id"],
        "--run-id",
        r.id,
        "--vpc-id",
        inventory["vpc_id"],
        "--volume-baseline",
        r.file("volume-baseline.json"),
        "--volume-baseline-sha256",
        digest(r.file("volume-baseline.json")),
        "--output",
        r.file("frozen/runtime.json"),
    ]
    r.file("frozen").mkdir(mode=0o700, exist_ok=True)
    for node in nodes.values():
        args += ["--instance", node["instance_id"]]
    for name in inventory.get("asg_names", []):
        args += ["--asg", name]
    r.command(
        args, env=r.environment(True), timeout=600, log="frozen/runtime-capture.log"
    )
    runtime = r.file("frozen/runtime.json").read_bytes()
    account = r.file("account-baseline.json").read_bytes()
    pending = ["kms_key=" + x for x in inventory.get("kms_keys", [])] + [
        "secret=" + x for x in inventory.get("secrets", [])
    ]
    manifest = build_manifest(
        state,
        hashlib.sha256(state).hexdigest(),
        inventory,
        r.id,
        r.c["account_id"],
        r.c["region"],
        runtime,
        hashlib.sha256(runtime).hexdigest(),
        account,
        hashlib.sha256(account).hexdigest(),
        pending,
        r.c["deploy_role_arn"].rsplit("/", 1)[1],
        auditor,
    )
    inventory.update(manifest["expected_resources"])
    private(r.file("frozen/state.json"), state.decode())
    r.write("frozen/outputs.json", outputs)
    r.write("frozen/inventory.json", inventory)
    r.write("frozen/manifest.json", manifest, new=True)
    r.event(
        "freeze",
        "owned state/native IDs and approved key/secret pending deletions retained",
    )


def cleanup_policy(r):
    r.identity(iam=True)
    i = r.read("frozen/inventory.json")
    c = r.c
    expiry = r.read("iam-review.json")["expires_at"]
    require(
        dt.datetime.fromisoformat(expiry.replace("Z", "+00:00"))
        > now() + dt.timedelta(minutes=50),
        "Cleanup IAM expiry too close; review a cleanup-only extension",
    )
    statements = []

    def add(actions, resources, global_service=False):
        if not resources:
            return
        condition = {"DateLessThan": {"aws:CurrentTime": expiry}}
        if not global_service:
            condition["StringEquals"] = {"aws:RequestedRegion": c["region"]}
        statements.append(
            {
                "Effect": "Allow",
                "Action": actions,
                "Resource": sorted(set(resources)),
                "Condition": condition,
            }
        )

    arn = "arn:aws:ec2:" + c["region"] + ":" + c["account_id"] + ":"
    net = [arn + "vpc/" + i["vpc_id"]]
    for key, kind in [
        ("subnets", "subnet"),
        ("route_tables", "route-table"),
        ("security_groups", "security-group"),
        ("internet_gateways", "internet-gateway"),
    ]:
        net += [arn + kind + "/" + x for x in i.get(key, [])]
    add(
        [
            "ec2:DeleteSubnet",
            "ec2:DeleteVpc",
            "ec2:DeleteRouteTable",
            "ec2:DisassociateRouteTable",
            "ec2:DetachInternetGateway",
            "ec2:DeleteInternetGateway",
            "ec2:DeleteSecurityGroup",
        ],
        net,
    )
    add(
        ["dynamodb:DeleteTable", "dynamodb:DescribeTable", "dynamodb:UpdateTable"],
        [
            "arn:aws:dynamodb:" + c["region"] + ":" + c["account_id"] + ":table/" + x
            for x in i.get("dynamodb_tables", [])
        ],
    )
    add(
        ["s3:DeleteBucket", "s3:DeleteBucketPolicy"],
        ["arn:aws:s3:::" + x for x in i.get("s3_buckets", [])],
        True,
    )
    add(
        ["ssm:DeleteParameter"],
        [
            x
            if x.startswith("arn:")
            else "arn:aws:ssm:"
            + c["region"]
            + ":"
            + c["account_id"]
            + ":parameter/"
            + x.lstrip("/")
            for x in i.get("ssm_parameters", [])
        ],
    )
    add(
        ["logs:DeleteLogGroup"],
        [
            "arn:aws:logs:"
            + c["region"]
            + ":"
            + c["account_id"]
            + ":log-group:"
            + x
            + ":*"
            for x in i.get("cloudwatch_log_groups", [])
        ],
    )
    add(
        ["kms:ScheduleKeyDeletion", "kms:DeleteAlias"],
        [
            "arn:aws:kms:" + c["region"] + ":" + c["account_id"] + ":" + x
            for x in i.get("kms_aliases", [])
        ]
        + i.get("kms_keys", []),
    )
    add(["secretsmanager:DeleteSecret"], i.get("secrets", []))
    add(["acm:DeleteCertificate"], i.get("acm_certificates", []))
    roles = r.aws("iam", "list-roles", iam=True)["Roles"]
    names = set(i.get("iam_roles", []))
    add(
        ["iam:ListInstanceProfilesForRole"],
        [x["Arn"] for x in roles if x["RoleName"] in names],
        True,
    )
    policy = {"Version": "2012-10-17", "Statement": statements}
    require(
        len(json.dumps(policy, separators=(",", ":"))) <= 6144,
        "Cleanup scope exceeds one managed policy",
    )
    path = r.write("iam/cleanup.json", policy)
    validation = r.aws(
        "accessanalyzer",
        "validate-policy",
        "--policy-type",
        "IDENTITY_POLICY",
        "--policy-document",
        "file://" + str(path),
        iam=True,
    )
    r.write("iam/cleanup-validation.json", validation)
    require(not validation["findings"], "Cleanup validation findings")
    r.write(
        "cleanup-review.json",
        {
            "sha256": digest(path),
            "expires_at": expiry,
            "manifest_sha256": digest(r.file("frozen/manifest.json")),
        },
    )
    r.event("cleanup-review", "exact owned resource cleanup policy ready; not attached")


def grant_cleanup(r):
    r.identity(iam=True)
    review = r.read("cleanup-review.json")
    require(
        digest(r.file("iam/cleanup.json")) == review["sha256"]
        and digest(r.file("frozen/manifest.json")) == review["manifest_sha256"],
        "Cleanup review changed",
    )
    require(
        dt.datetime.fromisoformat(review["expires_at"].replace("Z", "+00:00"))
        > now() + dt.timedelta(minutes=40),
        "Cleanup grant expired/too close",
    )
    if not r.file("iam/cleanup-created.json").exists():
        require(
            not r.file("iam/cleanup-intent.json").exists(),
            "Unknown CreatePolicy outcome; reconcile before retry",
        )
        r.write("iam/cleanup-intent.json", {"at": now().isoformat()}, new=True)
        made = r.aws(
            "iam",
            "create-policy",
            "--policy-name",
            "MilesCleanup-" + r.id,
            "--path",
            "/miles/acceptance/",
            "--policy-document",
            "file://" + str(r.file("iam/cleanup.json")),
            iam=True,
        )["Policy"]
        r.write("iam/cleanup-created.json", made)
    arn = r.read("iam/cleanup-created.json")["Arn"]
    r.aws(
        "iam",
        "attach-role-policy",
        "--role-name",
        r.c["deploy_role_arn"].rsplit("/", 1)[1],
        "--policy-arn",
        arn,
        iam=True,
    )
    r.event("cleanup-grant", "exact cleanup scope attached until recorded expiry")


def snapshot(r):
    r.identity()
    i = r.read("frozen/inventory.json")
    directory = "snapshots/" + now().strftime("%Y%m%dT%H%M%S%f")
    r.file(directory).mkdir(parents=True, mode=0o700)
    rows = {}
    for table in i.get("dynamodb_tables", []):
        value = r.aws("dynamodb", "scan", "--table-name", table, "--consistent-read")
        r.write(directory + "/" + table + ".json", value)
        rows[table] = value.get("Items", [])
    # Non-atomic read-only snapshots verify retained source bytes, not authoritative S3 receipt closure.
    payloads = {
        x["config_id"]["S"].removeprefix("storage/payload/"): x["body"]["B"]
        for x in rows.get("miles-dev-SystemConfig", [])
        if x.get("config_id", {}).get("S", "").startswith("storage/payload/")
        and "body" in x
    }
    pending = [
        x for x in rows.get("miles-dev-Outbox", []) if "export_progress" not in x
    ]
    missing = mismatch = 0
    for event in pending:
        pointer = event["payload_pointer"]["S"]
        expected = event["payload_sha256"]["S"]
        if pointer not in payloads:
            missing += 1
            continue
        body = base64.b64decode(payloads[pointer], validate=True)
        if hashlib.sha256(body).hexdigest() != expected:
            mismatch += 1
    proof = {
        "pending_events": len(pending),
        "missing_payloads": missing,
        "sha_mismatch": mismatch,
        "non_atomic": True,
        "proves_s3_receipts": False,
    }
    r.write(directory + "/payload-proof.json", proof)
    for bucket in i.get("s3_buckets", []):
        r.write(
            directory + "/" + bucket + "-versions.json",
            r.aws(
                "s3api",
                "list-object-versions",
                "--bucket",
                bucket,
                "--expected-bucket-owner",
                r.c["account_id"],
            ),
        )
        r.write(
            directory + "/" + bucket + "-uploads.json",
            r.aws(
                "s3api",
                "list-multipart-uploads",
                "--bucket",
                bucket,
                "--expected-bucket-owner",
                r.c["account_id"],
            ),
        )
    r.write(
        "latest-snapshot.json",
        {"directory": directory, "at": now().isoformat(), "payload_proof": proof},
    )
    r.event("snapshot", directory)
    return directory


def unprotect(r):
    r.identity()
    require(
        r.file("iam/cleanup-created.json").exists(),
        "Activate the exact owned cleanup grant first",
    )
    for table in r.read("frozen/inventory.json").get("dynamodb_tables", []):
        item = r.aws("dynamodb", "describe-table", "--table-name", table)["Table"]
        if item.get("DeletionProtectionEnabled"):
            r.aws(
                "dynamodb",
                "update-table",
                "--table-name",
                table,
                "--no-deletion-protection-enabled",
            )
    r.write("unprotected.json", {"at": now().isoformat()})
    r.event(
        "unprotect", "only owned table deletion protections disabled; data retained"
    )


def validate_destroy(plan, frozen):
    """A retry may delete only surviving identities from the original frozen graph."""
    original = {x["address"]: x for x in extract_resources(frozen)}
    prior = plan.get("prior_state", {"values": {"root_module": {}}})
    remaining = {x["address"]: x for x in extract_resources(prior, allow_empty=True)}
    require(
        all(original.get(address) == entry for address, entry in remaining.items()),
        "Destroy state contains a new or changed resource identity",
    )
    changes = [
        x
        for x in plan.get("resource_changes", [])
        if x.get("mode", "managed") == "managed" and x["change"]["actions"] != ["no-op"]
    ]
    require(
        {x["address"] for x in changes} == set(remaining)
        and len(changes) == len(remaining)
        and all(
            x["change"]["actions"] == ["delete"]
            and x.get("type") == remaining[x["address"]]["type"]
            and x["change"].get("before", {}).get("id") == remaining[x["address"]]["id"]
            for x in changes
        ),
        "Destroy plan differs from surviving frozen managed graph",
    )


def plan(r):
    r.identity()
    require(
        r.file("unprotected.json").exists(),
        "Unprotect owned tables before refreshing destroy plan",
    )
    target = r.file("cleanup-source")
    if not target.exists():
        shutil.copytree(
            r.file("source/infra/terraform/cloud"),
            target,
            ignore=shutil.ignore_patterns(".terraform", "*.tfstate*"),
        )
        changes = 0
        for f in target.rglob("*.tf"):
            if "bootstrap" in f.parts:
                continue
            before = f.read_text()
            after = before.replace("prevent_destroy = true", "prevent_destroy = false")
            changes += before.count("prevent_destroy = true")
            if before != after:
                private(f, after)
        require(
            changes == 4,
            "Protection layout changed; review cleanup code before planning",
        )
    stack = target / "environments/dev"
    r.tf(
        "init",
        "-input=false",
        "-reconfigure",
        "-lockfile=readonly",
        "-backend-config=" + str(r.file("backend.hcl")),
        stack=stack,
        log="cleanup-init.log",
    )
    # -destroy never restores ASG desired capacity or creates replacement nodes.
    r.tf(
        "plan",
        "-destroy",
        "-input=false",
        "-lock-timeout=60s",
        "-var-file="
        + str(
            r.file(
                "active.tfvars.json"
                if r.file("active.tfvars.json").exists()
                else (
                    "hosts.tfvars.json"
                    if r.file("hosts.tfvars.json").exists()
                    else "foundation.tfvars.json"
                )
            )
        ),
        "-out=" + str(r.file("destroy.tfplan")),
        stack=stack,
        log="destroy-plan.log",
    )
    value = json.loads(r.tf("show", "-json", r.file("destroy.tfplan"), stack=stack))
    r.write("destroy-plan.json", value)
    validate_destroy(value, r.read("frozen/state.json"))
    r.write(
        "destroy-reviewed.json",
        {
            "plan_sha256": digest(r.file("destroy.tfplan")),
            "manifest_sha256": digest(r.file("frozen/manifest.json")),
        },
    )
    r.event("cleanup-plan", "exact delete-only managed graph; inspect before cleanup")


def execute(r):
    r.identity()
    if r.file("destroy-complete.json").exists():
        r.event("cleanup", "already destroyed; finish IAM restore and native audit")
        return
    review = r.read("destroy-reviewed.json")
    i = r.read("frozen/inventory.json")
    require(
        digest(r.file("destroy.tfplan")) == review["plan_sha256"]
        and digest(r.file("frozen/manifest.json")) == review["manifest_sha256"],
        "Destroy evidence changed",
    )
    require(
        r.file("iam/cleanup-created.json").exists(),
        "Activate reviewed cleanup grant first",
    )
    if not r.file("cleanup-intent.json").exists():
        r.write("cleanup-intent.json", {"at": now().isoformat()}, new=True)
    if r.file("data-purged.json").exists():
        require(
            r.read("data-purged.json")["manifest_sha256"] == review["manifest_sha256"],
            "Purged data proof differs from frozen ownership",
        )
    else:
        # Freeze records every root/instance, including superseded nodes. Scaling cannot replace drained nodes.
        for name in i.get("asg_names", []):
            r.aws(
                "autoscaling",
                "update-auto-scaling-group",
                "--auto-scaling-group-name",
                name,
                "--min-size",
                "0",
                "--desired-capacity",
                "0",
            )
        r.ssh("edge", "sudo", "-n", "systemctl", "stop", "--no-block", "miles.service")

        def quiescent():
            for name in i.get("asg_names", []):
                groups = r.aws(
                    "autoscaling",
                    "describe-auto-scaling-groups",
                    "--auto-scaling-group-names",
                    name,
                )["AutoScalingGroups"]
                if groups and groups[0]["Instances"]:
                    return False
            return (
                r.ssh(
                    "edge",
                    "sudo",
                    "-n",
                    "systemctl",
                    "show",
                    "miles.service",
                    "-p",
                    "MainPID",
                    "--value",
                ).strip()
                == b"0"
            )

        r.wait("natural Cloud drain and Edge stopped before purge", quiescent, 2700)
        # A failed/interrupted SSH trial must not continue writing during purge.
        check = r.ssh(
            "client",
            "bash",
            "-c",
            "if pgrep -x warmbench >/dev/null; then echo running; else echo stopped; fi",
        ).strip()
        require(
            check == b"stopped",
            "Client still writing; stop/drain workload and retain outcome first",
        )
        r.write("runtime-quiescent.json", {"at": now().isoformat()})
        from deploy.acceptance.workload import collect

        collect(r)
        directory = snapshot(r)
        proof = r.read("latest-snapshot.json")["payload_proof"]
        require(
            proof["missing_payloads"] == 0 and proof["sha_mismatch"] == 0,
            "Pending source evidence incomplete: retain data and reconcile before purge",
        )
        for bucket in i.get("s3_buckets", []):
            versions = r.read(directory + "/" + bucket + "-versions.json")
            uploads = r.read(directory + "/" + bucket + "-uploads.json")
            for upload in uploads.get("Uploads", []):
                r.aws(
                    "s3api",
                    "abort-multipart-upload",
                    "--bucket",
                    bucket,
                    "--key",
                    upload["Key"],
                    "--upload-id",
                    upload["UploadId"],
                    "--expected-bucket-owner",
                    r.c["account_id"],
                )
            objects = [
                {"Key": x["Key"], "VersionId": x["VersionId"]}
                for kind in ["Versions", "DeleteMarkers"]
                for x in versions.get(kind, [])
            ]
            require(
                len(objects) < 100000, "Synthetic version count exceeded review; stop"
            )
            for n in range(0, len(objects), 1000):
                path = r.write(
                    "purge/request-" + str(n) + ".json",
                    {"Objects": objects[n : n + 1000], "Quiet": True},
                )
                result = r.aws(
                    "s3api",
                    "delete-objects",
                    "--bucket",
                    bucket,
                    "--delete",
                    "file://" + str(path),
                    "--expected-bucket-owner",
                    r.c["account_id"],
                )
                r.write("purge/result-" + str(n) + ".json", result)
                require(
                    not result.get("Errors"),
                    "Some version deletions failed; retain errors",
                )
            remaining = r.aws(
                "s3api",
                "list-object-versions",
                "--bucket",
                bucket,
                "--expected-bucket-owner",
                r.c["account_id"],
            )
            require(
                not remaining.get("Versions") and not remaining.get("DeleteMarkers"),
                "Bucket not empty after purge",
            )
        r.write(
            "data-purged.json",
            {"at": now().isoformat(), "manifest_sha256": review["manifest_sha256"]},
        )
    r.tf(
        "apply",
        "-input=false",
        "-lock-timeout=60s",
        r.file("destroy.tfplan"),
        stack=r.file("cleanup-source/environments/dev"),
        log="destroy-apply.log",
    )
    existing = {
        x["CertificateArn"]
        for x in r.aws("acm", "list-certificates", iam=True)["CertificateSummaryList"]
    }
    for arn in i.get("acm_certificates", []):
        if arn in existing:
            r.aws("acm", "delete-certificate", "--certificate-arn", arn)
    r.write("destroy-complete.json", {"at": now().isoformat()})
    r.event(
        "cleanup",
        "managed graph and synthetic data destroyed; IAM/audit still required",
    )


def finish(r):
    r.identity(iam=True)
    if r.file("direct-create-intent.json").exists():
        require(r.file("direct-absence.json").exists() and r.read("direct-absence.json").get("absent") is True,
                "Teardown/reconcile direct comparator before restoring deploy IAM")
    require(
        r.file("destroy-complete.json").exists(),
        "Destroy workload before restoring IAM",
    )
    iam.restore(r)
    role = r.c["deploy_role_arn"].rsplit("/", 1)[1]
    if (
        r.file("iam/cleanup-created.json").exists()
        and not r.file("iam/cleanup-removed.json").exists()
    ):
        policy = r.read("iam/cleanup-created.json")
        arn = policy["Arn"]
        listed = r.aws("iam", "list-policies", "--scope", "Local", iam=True)["Policies"]
        found = [p for p in listed if p["Arn"] == arn]
        if found:
            require(
                found[0]["PolicyId"] == policy["PolicyId"],
                "Cleanup policy identity changed",
            )
            attached = r.aws(
                "iam", "list-attached-role-policies", "--role-name", role, iam=True
            )["AttachedPolicies"]
            if any(p["PolicyArn"] == arn for p in attached):
                r.aws(
                    "iam",
                    "detach-role-policy",
                    "--role-name",
                    role,
                    "--policy-arn",
                    arn,
                    iam=True,
                )
            r.aws("iam", "delete-policy", "--policy-arn", arn, iam=True)
        r.write("iam/cleanup-removed.json", {"at": now().isoformat()})
    auditor = r.read("iam/auditor-role.json")
    existing = r.aws("iam", "list-roles", iam=True)["Roles"]
    found = [x for x in existing if x["RoleName"] == auditor["RoleName"]]
    if found:
        require(
            found[0]["RoleId"] == auditor["RoleId"]
            and found[0]["AssumeRolePolicyDocument"]
            == r.read("iam/auditor-trust.json"),
            "Auditor identity/trust changed",
        )
        policies = r.aws(
            "iam", "list-role-policies", "--role-name", auditor["RoleName"], iam=True
        )["PolicyNames"]
        require(
            set(policies) <= {"read-only-final-inventory"},
            "Auditor has unowned policies; preserve and reconcile",
        )
        if policies:
            r.aws(
                "iam",
                "delete-role-policy",
                "--role-name",
                auditor["RoleName"],
                "--policy-name",
                "read-only-final-inventory",
                iam=True,
            )
        r.aws("iam", "delete-role", "--role-name", auditor["RoleName"], iam=True)
    r.write("iam/auditor-removed.json", {"at": now().isoformat()})
    for name in [
        "AWSServiceRoleForElasticLoadBalancing",
        "AWSServiceRoleForRolesAnywhere",
    ]:
        proof = r.read("iam/" + name + ".json")
        if not proof["created"]:
            continue

        def deleted():
            roles = r.aws("iam", "list-roles", iam=True)["Roles"]
            found = [x for x in roles if x["RoleName"] == name]
            if not found:
                return True
            require(
                found[0]["RoleId"] == proof["role"]["RoleId"],
                "SLR identity changed; preserve",
            )
            task_file = "iam/" + name + "-task.json"
            old = r.read(task_file) if r.file(task_file).exists() else None
            status = None
            if old:
                try:
                    status = r.aws(
                        "iam",
                        "get-service-linked-role-deletion-status",
                        "--deletion-task-id",
                        old["id"],
                        iam=True,
                    )
                except Exception:
                    status = None
            if not old or not status or status.get("Status") == "FAILED":
                task = r.aws(
                    "iam", "delete-service-linked-role", "--role-name", name, iam=True
                )["DeletionTaskId"]
                r.write(task_file, {"id": task})
            else:
                task = old["id"]
            end = time.monotonic() + 120
            while time.monotonic() < end:
                try:
                    status = r.aws(
                        "iam",
                        "get-service-linked-role-deletion-status",
                        "--deletion-task-id",
                        task,
                        iam=True,
                    )
                except Exception:
                    time.sleep(5)
                    continue
                r.write(
                    "iam/slr-" + name + "-" + now().strftime("%H%M%S%f") + ".json",
                    status,
                )
                if status["Status"] == "SUCCEEDED":
                    return True
                if status["Status"] == "FAILED":
                    return False
                time.sleep(10)
            return False

        r.wait("run-created service role " + name, deleted, 1800)
    r.write("iam-finished.json", {"at": now().isoformat()})
    audit(r)


def audit(r):
    r.identity(iam=True)
    if r.file("direct-create-intent.json").exists():
        from deploy.acceptance import direct
        require(r.file("direct-absence.json").exists(), "Direct comparator absence not recorded")
        buckets = r.aws("s3api", "list-buckets", iam=True)
        require(direct.name(r) not in {x["Name"] for x in buckets.get("Buckets", [])}, "Residual direct comparator bucket")
    require(r.file("iam-finished.json").exists(), "Finish IAM restore/removal first")
    state = r.file("frozen/state.json").read_bytes()
    runtime = r.file("frozen/runtime.json").read_bytes()
    verifier = TeardownAbsenceVerifier(
        inventory=r.read("frozen/inventory.json"),
        manifest=r.file("frozen/manifest.json"),
        mode="live",
        profile=r.c["iam_profile"],
        region=r.c["region"],
        final_audit=True,
        state_json=state,
        state_sha256=hashlib.sha256(state).hexdigest(),
        runtime_json=runtime,
        runtime_sha256=hashlib.sha256(runtime).hexdigest(),
        volume_baseline_json=r.file("volume-baseline.json").read_bytes(),
        account_baseline_json=r.file("account-baseline.json").read_bytes(),
    )
    # Reuse the verifier's native checks with a sanitized CLI environment.
    import subprocess

    def native(args):
        require(
            len(args) > 1
            and args[1].startswith(("get-", "list-", "describe-", "head-")),
            "Final audit must stay read-only",
        )
        import time

        for attempt in range(3):
            try:
                p = subprocess.run(
                    [
                        "aws",
                        "--profile",
                        r.c["iam_profile"],
                        "--region",
                        r.c["region"],
                        "--no-cli-pager",
                        *args,
                    ],
                    env=r.environment(),
                    capture_output=True,
                    text=True,
                    timeout=120,
                )
                if p.returncode == 0:
                    return p.returncode, p.stdout, p.stderr
                if attempt < 2 and (
                    "Throttling" in p.stderr
                    or "Rate exceeded" in p.stderr
                    or "RequestLimitExceeded" in p.stderr
                ):
                    time.sleep(2 * (attempt + 1))
                    continue
                return p.returncode, p.stdout, p.stderr
            except subprocess.TimeoutExpired:
                if attempt < 2:
                    time.sleep(2 * (attempt + 1))
                    continue
                return -1, "", "native audit command timed out"

    verifier._runner = native
    verifier.export_summary(r.file("final-absence-summary.json"))
    require(
        r.read("final-absence-summary.json").get("accepted") is True,
        "Final native audit failed/unverified; inspect private ledger",
    )
    r.event("audit", "strict final audit accepted; operator may logout")
