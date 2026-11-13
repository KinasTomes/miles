"""Read-only verification of externally owned IAM and Terraform backend resources."""
import hashlib
import json
from fnmatch import fnmatchcase


def verify(aws, expected, state, private):
    account, region = expected["account_id"], expected["region"]
    actor = aws("sts", "get-caller-identity")
    role = aws("iam", "get-role", "--role-name", expected["role_arn"].rsplit("/", 1)[-1])["Role"]
    if actor.get("Account") != account or role.get("Arn") != expected["role_arn"] or not role.get("RoleId") or actor.get("UserId", "").split(":")[0] != role["RoleId"]:
        raise ValueError("Existing bootstrap role/account identity differs")

    def tags(items):
        found = {t["Key"]: t["Value"] for t in items}
        if any(found.get(k) != v for k, v in expected["tags"].items()):
            raise ValueError("Existing bootstrap ownership tags differ")

    tags(role.get("Tags", []))
    allows = [s for s in role.get("AssumeRolePolicyDocument", {}).get("Statement", []) if s.get("Effect") == "Allow"]
    if not allows or any(s.get("Principal") != {"AWS": expected["user_arn"]} or s.get("Action") not in ("sts:AssumeRole", ["sts:AssumeRole"]) or s.get("Condition", {}).get("Bool", {}).get("aws:MultiFactorAuthPresent") != "true" for s in allows):
        raise ValueError("Existing deployment role must trust only the configured MFA operator")
    boundary = aws("iam", "get-policy", "--policy-arn", expected["boundary_arn"])["Policy"]
    if boundary.get("Arn") != expected["boundary_arn"] or not boundary.get("DefaultVersionId"):
        raise ValueError("Existing runtime boundary differs")
    listed = aws("iam", "list-policy-tags", "--policy-arn", expected["boundary_arn"])
    if listed.get("IsTruncated") or listed.get("Marker"):
        raise ValueError("Incomplete boundary tag inventory")
    tags(listed.get("Tags", []))
    document = aws("iam", "get-policy-version", "--policy-arn", expected["boundary_arn"], "--version-id", boundary["DefaultVersionId"])["PolicyVersion"]["Document"]
    if not isinstance(document, dict) or not document.get("Statement"):
        raise ValueError("Missing runtime boundary document")
    bucket = expected["state_bucket"]
    args = ("--bucket", bucket, "--expected-bucket-owner", account)
    location = aws("s3api", "get-bucket-location", *args).get("LocationConstraint")
    if (location or "us-east-1").replace("EU", "eu-west-1") != region:
        raise ValueError("Existing backend Region differs")
    versioning = aws("s3api", "get-bucket-versioning", *args)
    public = aws("s3api", "get-public-access-block", *args).get("PublicAccessBlockConfiguration", {})
    encryption = aws("s3api", "get-bucket-encryption", *args).get("ServerSideEncryptionConfiguration", {}).get("Rules", [])
    if versioning.get("Status") != "Enabled" or not all(public.get(k) is True for k in ("BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets")) or not encryption or any(r.get("ApplyServerSideEncryptionByDefault", {}).get("SSEAlgorithm") != "AES256" for r in encryption):
        raise ValueError("Existing backend needs versioning, all public access blocks and AES256 encryption")
    tags(aws("s3api", "get-bucket-tagging", *args).get("TagSet", []))
    policy = json.loads(aws("s3api", "get-bucket-policy", *args)["Policy"])
    arn = "arn:aws:s3:::" + bucket
    tls = False
    for statement in policy.get("Statement", []):
        actions, resources = statement.get("Action", []), statement.get("Resource", [])
        actions = [actions] if isinstance(actions, str) else actions
        resources = [resources] if isinstance(resources, str) else resources
        tls = tls or (statement.get("Effect") == "Deny" and statement.get("Principal") in ("*", {"AWS": "*"})
                      and any(a in ("*", "s3:*") for a in actions)
                      and statement.get("Condition") == {"Bool": {"aws:SecureTransport": "false"}}
                      and all(any(fnmatchcase(resource, pattern) for pattern in resources) for resource in (arn, arn + "/*")))
    if not tls:
        raise ValueError("Existing backend must deny insecure transport")
    checks = [("s3:ListBucket", arn, True), ("s3:GetObject", arn + "/" + expected["state_key"], True),
              ("s3:PutObject", arn + "/" + expected["state_key"], True),
              ("s3:GetObject", arn + "/" + expected["state_key"] + ".tflock", True),
              ("s3:PutObject", arn + "/" + expected["state_key"] + ".tflock", True),
              ("s3:DeleteObject", arn + "/" + expected["state_key"] + ".tflock", True),
              ("s3:DeleteObject", arn + "/" + expected["state_key"], False),
              ("iam:PutRolePolicy", expected["role_arn"], False)]
    for action, resource, permitted in checks:
        request = {"PolicySourceArn": expected["role_arn"], "ActionNames": [action], "ResourceArns": [resource],
                   "ContextEntries": [{"ContextKeyName": "aws:RequestedRegion", "ContextKeyValues": [region], "ContextKeyType": "string"},
                                      {"ContextKeyName": "aws:PrincipalArn", "ContextKeyValues": [expected["role_arn"]], "ContextKeyType": "string"},
                                      {"ContextKeyName": "aws:SecureTransport", "ContextKeyValues": ["true"], "ContextKeyType": "boolean"}]}
        check_file = private / "bootstrap-permission-check.json"
        with check_file.open("w") as stream:
            check_file.chmod(0o600)
            json.dump(request, stream)
        result = aws("iam", "simulate-principal-policy", "--cli-input-json", "file://" + str(check_file))
        evaluations = result.get("EvaluationResults", [])
        if result.get("IsTruncated") or result.get("Marker") or len(evaluations) != 1 or evaluations[0].get("MissingContextValues") or any(r.get("MissingContextValues") for r in evaluations[0].get("ResourceSpecificResults", [])) or evaluations[0].get("EvalDecision") not in (("allowed",) if permitted else ("implicitDeny", "explicitDeny")):
            raise ValueError("Existing deploy role lacks required backend/lock permissions")
    # Inspect state locally; do not adopt another workload or overwrite its state.
    path = private / "bootstrap-backend-inspection.json"
    path.touch(mode=0o600, exist_ok=True)
    path.chmod(0o600)
    try:
        aws("s3api", "get-object", *args, "--key", expected["state_key"], "--range", "bytes=0-16777216", str(path))
    except RuntimeError as error:
        if getattr(error, "code", None) not in ("404", "NoSuchKey", "NotFound"):
            raise
        if state.get("backend_lineage"):
            raise ValueError("Previously bound backend state disappeared") from None
    else:
        if path.stat().st_size > 16777216:
            raise ValueError("Backend state exceeds inspection limit")
        remote = json.loads(path.read_text())
        if not isinstance(remote, dict) or remote.get("version") != 4 or not isinstance(remote.get("lineage"), str) or not remote["lineage"] or not isinstance(remote.get("resources", []), list) or any(not isinstance(r, dict) for r in remote.get("resources", [])):
            raise ValueError("Unsupported or incomplete existing backend state")
        if state.get("backend_lineage", remote["lineage"]) != remote["lineage"]:
            raise ValueError("Backend state lineage changed")
        if not state.get("foundation_apply_started") and any(r.get("mode") != "data" for r in remote.get("resources", [])):
            raise ValueError("Backend already contains a workload; recover its original private deployment artifacts")
        state["backend_lineage"] = remote["lineage"]
    finally:
        path.unlink(missing_ok=True)
    stable = {"expected": expected, "role_id": role["RoleId"], "trust": role["AssumeRolePolicyDocument"],
              "boundary_version": boundary["DefaultVersionId"], "boundary_document": document,
              "bucket_policy": policy, "versioning": versioning, "public_access": public, "encryption": encryption}
    return {"fingerprint": hashlib.sha256(json.dumps(stable, sort_keys=True).encode()).hexdigest(),
            "mode": "existing", "bootstrap_resources_managed": False}
