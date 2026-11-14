"""A standalone direct-S3 comparator lifecycle. Never adopt an existing bucket."""

import hashlib
import json

from deploy.acceptance.common import now, require
from deploy.acceptance import approvals


def name(r):
    # The exact ARN is rendered into IAM review; no wildcard bucket permissions.
    suffix = hashlib.sha256(r.id.encode()).hexdigest()[:12]
    return f"miles-direct-{r.c['account_id']}-{suffix}"


def plan_value(r):
    return {"run_id": r.id, "source": r.meta["source"], "bucket": name(r),
           "account": r.c["account_id"], "region": r.c["region"],
           "tags": {"Project": "Miles", "Environment": "dev", "AcceptanceRun": r.id},
           "encryption": "aws:kms", "kms_key": "same-key-as-runtime-inventory",
           "versioning": "Enabled", "public_access_block": True,
           "unknown_create": "retain intent; reconcile exact bucket; never auto-adopt/retry"}


def plan(r):
    out = plan_value(r)
    r.write("direct-plan.json", out)
    return out


def owned(r, *, verify_configuration=True):
    receipt = r.read("direct-created.json")
    require(receipt["run_id"] == r.id and receipt["bucket"] == name(r), "Wrong direct bucket receipt")
    r.aws("s3api", "head-bucket", "--bucket", name(r), "--expected-bucket-owner", r.c["account_id"])
    tags = r.aws("s3api", "get-bucket-tagging", "--bucket", name(r), "--expected-bucket-owner", r.c["account_id"])
    actual = {t["Key"]: t["Value"] for t in tags.get("TagSet", [])}
    require(actual == receipt["tags"], "Direct bucket ownership changed; refuse use/delete")
    if verify_configuration:
        args = ["--bucket", name(r), "--expected-bucket-owner", r.c["account_id"]]
        versioning = r.aws("s3api", "get-bucket-versioning", *args)
        encryption = r.aws("s3api", "get-bucket-encryption", *args)
        access = r.aws("s3api", "get-public-access-block", *args)
        require(versioning.get("Status") == "Enabled", "Direct versioning drift")
        rules = encryption["ServerSideEncryptionConfiguration"]["Rules"]
        require(len(rules) == 1 and rules[0]["ApplyServerSideEncryptionByDefault"] == {
            "SSEAlgorithm": "aws:kms", "KMSMasterKeyID": receipt["kms_key"]}, "Direct encryption mismatch")
        require(all(access["PublicAccessBlockConfiguration"].get(k) is True for k in
                ("BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets")), "Direct public access block drift")
    return receipt


def provision(r):
    approvals.check(r, "direct-provision")
    r.identity(); r.live_window(needed=30)
    p = r.read("direct-plan.json")
    require(p == plan_value(r), "Direct plan changed")
    require(not r.file("direct-create-intent.json").exists(), "Create already dispatched; reconcile unknown outcome before any retry")
    # list-buckets is the already-reviewed account baseline read. No ambiguous
    # 403/404 HeadBucket result is upgraded to absence.
    listed = r.aws("s3api", "list-buckets", audit=True)
    require(name(r) not in {b["Name"] for b in listed.get("Buckets", [])}, "Direct bucket already exists; do not adopt")
    kms = r.outputs("active")["runtime_inventory"]["kms_key_arn"]
    r.write("direct-create-intent.json", {**p, "at": now().isoformat(), "kms_key": kms}, new=True)
    args = ["s3api", "create-bucket", "--bucket", name(r), "--object-ownership", "BucketOwnerEnforced"]
    if r.c["region"] != "us-east-1":
        args += ["--create-bucket-configuration", json.dumps({"LocationConstraint": r.c["region"]})]
    # A lost ACK leaves an intent only; no automatic retries or cleanup adoption.
    r.aws(*args)
    r.write("direct-create-ack.json", {"run_id": r.id, "bucket": name(r)}, new=True)
    bucket_args = ["--bucket", name(r), "--expected-bucket-owner", r.c["account_id"]]
    r.aws("s3api", "put-bucket-tagging", *bucket_args, "--tagging",
          json.dumps({"TagSet": [{"Key": k, "Value": v} for k, v in p["tags"].items()]}))
    r.aws("s3api", "put-public-access-block", *bucket_args, "--public-access-block-configuration",
          json.dumps({k: True for k in ("BlockPublicAcls", "IgnorePublicAcls", "BlockPublicPolicy", "RestrictPublicBuckets")}))
    r.aws("s3api", "put-bucket-versioning", *bucket_args, "--versioning-configuration", "Status=Enabled")
    r.aws("s3api", "put-bucket-encryption", *bucket_args, "--server-side-encryption-configuration",
          json.dumps({"Rules": [{"ApplyServerSideEncryptionByDefault": {"SSEAlgorithm": "aws:kms", "KMSMasterKeyID": kms}}]}))
    policy = {"Version": "2012-10-17", "Statement": [{"Effect": "Deny", "Principal": "*",
        "Action": "s3:*", "Resource": ["arn:aws:s3:::"+name(r), "arn:aws:s3:::"+name(r)+"/*"],
        "Condition": {"Bool": {"aws:SecureTransport": "false"}}}]}
    r.aws("s3api", "put-bucket-policy", *bucket_args, "--policy", json.dumps(policy))
    r.write("direct-created.json", {**p, "kms_key": kms, "created_at": now().isoformat()}, new=True)
    owned(r)


def contents(r):
    args = ["--bucket", name(r), "--expected-bucket-owner", r.c["account_id"]]
    versions = r.aws("s3api", "list-object-versions", *args)
    uploads = r.aws("s3api", "list-multipart-uploads", *args)
    require(not uploads.get("Uploads"), "Unsettled direct multipart upload; reconcile it before deleting")
    return sorted([{"Key": x["Key"], "VersionId": x["VersionId"]}
                   for k in ("Versions", "DeleteMarkers") for x in versions.get(k, [])],
                  key=lambda x: (x["Key"], x["VersionId"]))


def cleanup_plan(r):
    owned(r, verify_configuration=False)
    require(r.file("stop-proof.json").exists(), "Stop/quiesce workload before direct cleanup review")
    processes = json.loads(r.ssh("client", "python3.12", "/home/ec2-user/acceptance/client.py", "quiescence"))
    require(processes == {"active": []}, "Client still has benchmark/AWS processes; reconcile before cleanup")
    r.write("direct-cleanup-plan.json", {"run_id": r.id, "bucket": name(r),
        "source": r.meta["source"], "versions": contents(r), "at": now().isoformat()}, new=True)


def cleanup(r):
    approvals.check(r, "direct-cleanup")
    r.identity(); owned(r, verify_configuration=False)
    processes = json.loads(r.ssh("client", "python3.12", "/home/ec2-user/acceptance/client.py", "quiescence"))
    require(processes == {"active": []}, "Client became active after cleanup review")
    reviewed = r.read("direct-cleanup-plan.json")
    require(reviewed["versions"] == contents(r), "Direct versions changed since cleanup review")
    require(not r.file("direct-delete-intent.json").exists(), "Cleanup dispatched; reconcile without replay")
    r.write("direct-delete-intent.json", reviewed, new=True)
    versions = reviewed["versions"]
    for start in range(0, len(versions), 1000):
        out = r.aws("s3api", "delete-objects", "--bucket", name(r), "--expected-bucket-owner", r.c["account_id"],
                    "--delete", json.dumps({"Objects": versions[start:start+1000], "Quiet": False}))
        r.write(f"direct-cleanup/delete-{start}.json", out, new=True)
        require(not out.get("Errors"), "Partial version deletion; reconcile exact failed versions")
    require(contents(r) == [], "Direct bucket not empty after reviewed deletion")
    r.aws("s3api", "delete-bucket", "--bucket", name(r), "--expected-bucket-owner", r.c["account_id"])
    # Account listing distinguishes native absence from AccessDenied on HEAD.
    out = r.aws("s3api", "list-buckets", audit=True)
    require(name(r) not in {x["Name"] for x in out.get("Buckets", [])}, "Direct bucket absence not verified")
    r.write("direct-absence.json", {"bucket": name(r), "run_id": r.id, "absent": True, "at": now().isoformat()}, new=True)
