"""Render reviewed scopes; activate/restore only recorded IAM changes."""

import datetime as dt
import json
import string

from deploy.acceptance.common import ROOT, digest, load, now, require

MANAGED = {
    "runtime-roles": "MilesDevDeployRuntimeRoles",
    "compute": "MilesDevDeployCompute",
    "data": "MilesDevDeployData",
    "services": "MilesDevDeployServices",
    "runtime-boundary": "MilesDevRuntimeBoundary",
}


def baseline(r):
    r.identity(iam=True)
    require(
        not r.file("iam-activation.json").exists(), "Baseline must precede activation"
    )
    for script, out in [
        ("capture_account_inventory.py", "account-baseline.json"),
        ("capture_volume_baseline.py", "volume-baseline.json"),
    ]:
        if r.file(out).exists():
            continue
        args = [
            "python3",
            ROOT / "scripts" / script,
            "--profile",
            r.c["iam_profile"],
            "--region",
            r.c["region"],
            "--account-id",
            r.c["account_id"],
            "--run-id",
            r.id,
            "--output",
            r.file(out),
        ]
        if script.startswith("capture_account"):
            args += ["--iam-role", r.c["deploy_role_arn"].rsplit("/", 1)[1]]
        r.command(args, env=r.environment(), timeout=600, log=out + ".capture.log")


def review(r):
    baseline(r)
    require(
        r.file("foundation-plan.json").exists(),
        "Plan foundation (including Autopilot baseline) before IAM review",
    )
    require(
        not r.file("iam-activation.json").exists(),
        "IAM already activated; preserve review",
    )
    expiry = (
        (now() + dt.timedelta(hours=max(6, r.c["hours"] + 2)))
        .isoformat(timespec="seconds")
        .replace("+00:00", "Z")
    )
    values = {
        "ACCOUNT_ID": r.c["account_id"],
        "REGION": r.c["region"],
        "AMI_ID": r.c["ami_id"],
        "DEPLOY_ROLE_ARN": r.c["deploy_role_arn"],
        "RUNTIME_BOUNDARY_ARN": r.c["runtime_boundary_arn"],
        "RUNTIME_SECRET_NAME": r.read("names.json")["secret"],
        "AUDIT_ROLE_NAME": r.read("names.json")["auditor"],
        "IAM_EXPIRY": expiry,
    }
    receipts = {}
    for src in sorted((ROOT / "deploy/acceptance/policies").glob("*.tmpl")):
        name = src.name.removesuffix(".json.tmpl")
        document = json.loads(string.Template(src.read_text()).substitute(values))
        path = r.write("iam/" + name + ".json", document, compact=True)
        if name == "auditor-trust":
            continue
        validation = r.aws(
            "accessanalyzer",
            "validate-policy",
            "--policy-type",
            "IDENTITY_POLICY",
            "--policy-document",
            "file://" + str(path),
            iam=True,
        )
        r.write("iam/" + name + "-validation.json", validation)
        require(
            not validation["findings"],
            "IAM validation findings; review private receipt before activation",
        )
        receipts[name] = digest(path)
    for key, name in MANAGED.items():
        arn = (
            r.c["runtime_boundary_arn"]
            if key == "runtime-boundary"
            else (r.c["runtime_boundary_arn"].rsplit("/", 1)[0] + "/" + name)
        )
        p = r.aws("iam", "get-policy", "--policy-arn", arn, iam=True)["Policy"]
        document = r.aws(
            "iam",
            "get-policy-version",
            "--policy-arn",
            arn,
            "--version-id",
            p["DefaultVersionId"],
            iam=True,
        )["PolicyVersion"]["Document"]
        versions = r.aws("iam", "list-policy-versions", "--policy-arn", arn, iam=True)[
            "Versions"
        ]
        require(
            len(versions) < 5,
            "No free managed-policy version; preserve existing versions",
        )
        path = r.file("iam/baseline-" + key + ".json")
        value = {
            "arn": arn,
            "default": p["DefaultVersionId"],
            "document": document,
            "versions": versions,
        }
        if path.exists():
            require(load(path) == value, "Baseline policy changed since review")
        else:
            r.write("iam/baseline-" + key + ".json", value, new=True)
    # Simulator uses all reviewed deploy scopes, not a new manually broadened policy.
    documents = [
        json.dumps(r.read("iam/" + n + ".json"), separators=(",", ":"))
        for n in [*MANAGED.keys(), "deploy"]
        if n != "runtime-boundary"
    ]
    bucket = "arn:aws:s3:::miles-dev-data-" + r.c["account_id"] + "-" + r.c["region"]
    table = (
        "arn:aws:dynamodb:"
        + r.c["region"]
        + ":"
        + r.c["account_id"]
        + ":table/miles-dev-Outbox"
    )
    probes = [
        ("s3:ListBucketVersions", bucket),
        ("dynamodb:Scan", table),
        ("dynamodb:DescribeTable", table),
    ]
    context = [
        {"ContextKeyName": k, "ContextKeyType": t, "ContextKeyValues": [v]}
        for k, t, v in [
            ("aws:CurrentTime", "date", now().isoformat()),
            ("aws:RequestedRegion", "string", r.c["region"]),
            ("aws:ResourceTag/Project", "string", "Miles"),
            ("aws:ResourceTag/Environment", "string", "dev"),
        ]
    ]
    for action, arn in probes:
        result = r.aws(
            "iam",
            "simulate-custom-policy",
            "--policy-input-list",
            *documents,
            "--action-names",
            action,
            "--resource-arns",
            arn,
            "--context-entries",
            json.dumps(context),
            iam=True,
        )
        r.write("iam/simulate-" + action.split(":")[1] + ".json", result)
        require(
            all(x["EvalDecision"] == "allowed" for x in result["EvaluationResults"]),
            "IAM simulation failed",
        )
    r.write(
        "iam-review.json",
        {
            "expires_at": expiry,
            "prepared_at": now().isoformat(),
            "hashes": receipts,
            "source": r.meta["source"],
            "scope": "reviewed acceptance policies; no IAM changed",
        },
    )
    from deploy.acceptance.pricing import review as cost_review

    cost_review(r)
    r.event(
        "iam-review",
        "validation and simulations passed; review policy files and plan before activate",
    )


def activate(r):
    r.identity(iam=True)
    reviewed = r.read("iam-review.json")
    from decimal import Decimal

    cost = r.read("cost-review.json")
    require(
        now() - dt.datetime.fromisoformat(cost["at"]) < dt.timedelta(minutes=45)
        and Decimal(cost["estimate_usd"]) <= Decimal(str(r.c["max_usd"]))
        and cost["envelope_usd"] == r.c["max_usd"]
        and cost["hours"] == r.c["hours"],
        "Cost review stale, over budget or configuration changed",
    )
    require(r.file("cost-review.json").exists(), "Cost review missing")
    require(
        dt.datetime.fromisoformat(reviewed["expires_at"].replace("Z", "+00:00")) - now()
        > dt.timedelta(hours=r.c["hours"] + 0.5),
        "IAM review is stale; refresh before activation",
    )
    for name, sha in reviewed["hashes"].items():
        require(
            digest(r.file("iam/" + name + ".json")) == sha, "Reviewed policy changed"
        )
    names = r.read("names.json")
    role = r.c["deploy_role_arn"].rsplit("/", 1)[1]
    state = (
        r.read("iam-activation.json")
        if r.file("iam-activation.json").exists()
        else {"status": "activating", "managed": {}, "inline": names["inline"]}
    )
    require(
        state["status"] in ("activating", "active"),
        "Do not activate after restoration/cleanup",
    )
    r.write("iam-activation.json", state)
    for key in MANAGED:
        base = r.read("iam/baseline-" + key + ".json")
        arn = base["arn"]
        current = r.aws("iam", "get-policy", "--policy-arn", arn, iam=True)["Policy"][
            "DefaultVersionId"
        ]
        doc = r.aws(
            "iam",
            "get-policy-version",
            "--policy-arn",
            arn,
            "--version-id",
            current,
            iam=True,
        )["PolicyVersion"]["Document"]
        proposal = r.read("iam/" + key + ".json")
        if key in state["managed"]:
            require(
                current == state["managed"][key] and doc == proposal,
                "Concurrent managed policy change",
            )
            continue
        if doc == proposal and current != base["default"]:
            # An acknowledged-by-AWS response may have been lost after our recorded intent.
            require(state.get("pending") == key, "Unowned matching policy change")
            version = current
        else:
            require(
                current == base["default"] and doc == base["document"],
                "Managed baseline changed",
            )
            state["pending"] = key
            r.write("iam-activation.json", state)
            version = r.aws(
                "iam",
                "create-policy-version",
                "--policy-arn",
                arn,
                "--policy-document",
                "file://" + str(r.file("iam/" + key + ".json")),
                "--set-as-default",
                iam=True,
            )["PolicyVersion"]["VersionId"]
        state["managed"][key] = version
        state.pop("pending", None)
        r.write("iam-activation.json", state)
    existing = r.aws("iam", "list-role-policies", "--role-name", role, iam=True)[
        "PolicyNames"
    ]
    if names["inline"] in existing:
        actual = r.aws(
            "iam",
            "get-role-policy",
            "--role-name",
            role,
            "--policy-name",
            names["inline"],
            iam=True,
        )["PolicyDocument"]
        require(actual == r.read("iam/deploy.json"), "Unowned inline policy")
    else:
        r.aws(
            "iam",
            "put-role-policy",
            "--role-name",
            role,
            "--policy-name",
            names["inline"],
            "--policy-document",
            "file://" + str(r.file("iam/deploy.json")),
            iam=True,
        )
    state["status"] = "active"
    r.write("iam-activation.json", state)
    create_auditor(r)
    baseline_roles = r.read("account-baseline.json")["resources"]["roles"]
    for service, name in [
        ("rolesanywhere.amazonaws.com", "AWSServiceRoleForRolesAnywhere"),
        ("elasticloadbalancing.amazonaws.com", "AWSServiceRoleForElasticLoadBalancing"),
    ]:
        if r.file("iam/" + name + ".json").exists():
            continue
        roles = r.aws("iam", "list-roles", iam=True)["Roles"]
        found = [x for x in roles if x["RoleName"] == name]
        require(
            name not in baseline_roles or bool(found),
            "Baseline service role disappeared; reconcile concurrent account changes",
        )
        created = not found
        if not found:
            found = [
                r.aws(
                    "iam",
                    "create-service-linked-role",
                    "--aws-service-name",
                    service,
                    iam=True,
                )["Role"]
            ]
        r.write("iam/" + name + ".json", {"created": created, "role": found[0]})
    r.event("iam-activate", "temporary scopes activated; workload must use deploy role")


def create_auditor(r):
    name = r.read("names.json")["auditor"]
    require(
        name not in r.read("account-baseline.json")["resources"]["roles"],
        "Auditor existed in baseline",
    )
    roles = r.aws("iam", "list-roles", iam=True)["Roles"]
    found = [x for x in roles if x["RoleName"] == name]
    if found:
        require(
            r.file("iam/auditor-intent.json").exists(), "Auditor name already in use"
        )
        role = found[0]
        require(
            role["AssumeRolePolicyDocument"] == r.read("iam/auditor-trust.json"),
            "Auditor trust changed",
        )
    else:
        r.write("iam/auditor-intent.json", {"name": name, "at": now().isoformat()})
        role = r.aws(
            "iam",
            "create-role",
            "--role-name",
            name,
            "--path",
            "/miles/acceptance/",
            "--assume-role-policy-document",
            "file://" + str(r.file("iam/auditor-trust.json")),
            "--max-session-duration",
            "3600",
            "--tags",
            "Key=Project,Value=Miles",
            "Key=Environment,Value=dev",
            "Key=AcceptanceRun,Value=" + r.id,
            iam=True,
        )["Role"]
    r.write("iam/auditor-role.json", role)
    r.aws(
        "iam",
        "put-role-policy",
        "--role-name",
        name,
        "--policy-name",
        "read-only-final-inventory",
        "--policy-document",
        "file://" + str(r.file("iam/auditor-policy.json")),
        iam=True,
    )


def restore(r):
    r.identity(iam=True)
    state = r.read("iam-activation.json")
    state["status"] = "restoring"
    r.write("iam-activation.json", state)
    for key, version in reversed(list(state["managed"].items())):
        base = r.read("iam/baseline-" + key + ".json")
        arn = base["arn"]
        current = r.aws("iam", "get-policy", "--policy-arn", arn, iam=True)["Policy"][
            "DefaultVersionId"
        ]
        require(
            current in (version, base["default"]),
            "Concurrent policy change: preserve and reconcile",
        )
        if current == version:
            doc = r.aws(
                "iam",
                "get-policy-version",
                "--policy-arn",
                arn,
                "--version-id",
                version,
                iam=True,
            )["PolicyVersion"]["Document"]
            require(
                doc == r.read("iam/" + key + ".json"),
                "Policy content changed; do not overwrite",
            )
            r.aws(
                "iam",
                "set-default-policy-version",
                "--policy-arn",
                arn,
                "--version-id",
                base["default"],
                iam=True,
            )
        versions = r.aws("iam", "list-policy-versions", "--policy-arn", arn, iam=True)[
            "Versions"
        ]
        if any(x["VersionId"] == version for x in versions):
            r.aws(
                "iam",
                "delete-policy-version",
                "--policy-arn",
                arn,
                "--version-id",
                version,
                iam=True,
            )
    role = r.c["deploy_role_arn"].rsplit("/", 1)[1]
    name = state["inline"]
    policies = r.aws("iam", "list-role-policies", "--role-name", role, iam=True)[
        "PolicyNames"
    ]
    if name in policies:
        require(
            r.aws(
                "iam",
                "get-role-policy",
                "--role-name",
                role,
                "--policy-name",
                name,
                iam=True,
            )["PolicyDocument"]
            == r.read("iam/deploy.json"),
            "Inline changed",
        )
        r.aws(
            "iam",
            "delete-role-policy",
            "--role-name",
            role,
            "--policy-name",
            name,
            iam=True,
        )
    state["status"] = "restored"
    r.write("iam-activation.json", state)
    r.event("iam-restore", "managed defaults and deploy inline restored")
