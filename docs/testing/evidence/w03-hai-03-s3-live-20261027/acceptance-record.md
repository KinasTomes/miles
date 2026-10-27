# W03-HAI-03 personal-account S3 Warm evidence

Executed 2026-10-27, 18:11:01-18:11:03 UTC+07:00 against commit
`f4a13f2a8c04ad12651056bc158a046602266879` on
`codex/w03-hai-03-storage-handoff-dynamodb` (PR #35).

Historical snapshot: the retention/cleanup status below describes the first run.
The [integrated rerun and subsequent cleanup](../s3-warm-final-20261027/acceptance-record.md)
supersedes it: all three versions from this run and three from the rerun were
audited and removed. Bucket/CMK/role remain intentionally retained.

## Runtime and provenance

* Dedicated personal-account bucket: `miles-hai03-s3-lab-demo-bucket`.
* Region: `ap-southeast-1`; account ID ends in `8569`.
* Runtime: `assumed-role/MilesHai03S3Test/hai03-s3-live`, verified by STS before
  dispatch. AssumeRole duration: 900 seconds. Temporary credentials were held
  only in process memory/environment and cleared in `finally` after the test.
* Administrative CLI login: IAM user `miles-admin`, browser/MFA authentication.
  No root or long-lived access key was used by the runner.
* KMS alias: `miles-hai-s3-lab`; key ID:
  `3d9ce284-47e8-460a-be55-86916485bac8` (Enabled, symmetric encrypt/decrypt).
* AWS CLI `2.38.6`; Go toolchain `go1.27.2` cached in this checkout.
* This lab was provisioned manually through Console with the user's approval.
  No Terraform init/plan/apply, shared backend/state reuse, or compute/network
  provisioning was performed for this run. This is not evidence that the shared
  Terraform deployment/IAM/budget guards have been exercised in this account.

The private original evidence is retained under
`.cache/evidence/w03-hai-03-s3-live-20261027T111051Z/`, including STS identity.
In the committed AWS audit JSON files, the account ID inside KMS ARNs alone is
replaced with `PERSONAL_ACCOUNT_ID`. Object keys, version IDs and test output
are unchanged. No credentials, authorization URL or MFA material is included.

## Invocation and result

The runner received explicit temporary `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, `AWS_REGION`,
`MILES_S3_DEV_BUCKET`, `MILES_S3_DEV_KMS_KEY` and
`MILES_S3_DEV_ACCEPTANCE=1` in its process environment. Values of credentials
were not logged. Command:

```text
go test -count=1 -json -timeout=5m ./internal/storage/s3 -run '^TestS3DevWarmContract$'
```

[Raw Go test events](go-test.jsonl) record PASS, not SKIP:

| Test | Result | Elapsed |
|---|---|---|
| immutable_pack_range_and_generation | PASS | 1.94s |
| body_errors_and_cancellation_leave_no_pack | PASS | 0.23s |
| TestS3DevWarmContract | PASS | 2.18s |

The first case verifies immutable pack/sidecar PUT, identical retries,
conflicting-content rejection, exact range bytes, invalid ranges, missing
generation and distinct generation isolation. The second verifies invalid body
lengths, broken streams and cancellation leave no pack.

## Independent AWS audit

[Bucket configuration](bucket-config.json), captured after the run, confirms:
Singapore location; Versioning Enabled; all four Public Access Block settings
true; BucketOwnerEnforced; SSE-KMS with the expected key; Bucket Key enabled.

[Before](versions-before.json) and [after](versions-after.json) full bucket
version listings show an initially empty bucket and exactly three new versions,
with no delete markers. [HEAD audit](object-audit.json) preserves each exact
key/version, length, storage class and encryption response. The total payload is
21 bytes (8 + 8 + 5); each key has one version despite retries/collision tests.
The failed-body case created no versions. All three HEADs report SSE-KMS,
the expected CMK and Bucket Key enabled; omitted S3 StorageClass is interpreted
as STANDARD. The audit records this normalized class explicitly.

## Retention and remaining gates

Objects/versions, bucket, CMK and test role are intentionally retained for review.
No DeleteObject/DeleteObjectVersion, bucket deletion or KMS deletion was performed.
The runtime role has only bucket List/GetLocation, pack/index Get/Put and exact
CMK GenerateDataKey/Decrypt permissions. The administrative CLI login remains
available; clearing the runner environment is not revocation of that login or
of an already issued STS session. There is no automatic test rerun configured.

**Cleanup/pause/teardown acceptance remains open.** Retaining three objects and
clearing environment variables is not an audited cloud teardown. Before cleanup,
review these exact versions and confirm no new/unrelated data or dependent key
users exist; remove only approved test versions, then audit remaining versions,
delete markers and resource state. Review bucket/key/role cleanup separately;
retain the evidence outside the bucket before scheduling key deletion. The CMK
remains enabled and can accrue charges. Existing unrelated EC2/EBS resources and
the account's current budget configuration were not audited by this run.

PR #35 still needs the requested approved-baseline docs and no-GSI/LSI/TTL
regression guard, integration with current main, consumer sign-off (Chien, Phong,
Hung), controlled cleanup evidence and CI on the resulting head. Personal S3
Warm PASS proves the physical port only: it does not establish DynamoDB adapter,
shared-runtime infrastructure compliance, archive restore or multi-node commit.
