# Integrated S3 Warm acceptance and controlled cleanup

W03-HAI-03 / PR #35. Final code verification on 27/10/2026 after integrating
`origin/main` at `00df615` (PR #32 and #34).

## Source and identity

Source commit: `fee31e07a2979d657a4a39490a22c9203605a747` (also recorded in
[source-commit.txt](source-commit.txt)). Later evidence-only commits do not change
the code exercised here.

* Bucket: `miles-hai03-s3-lab-demo-bucket`; region: `ap-southeast-1`.
* Dedicated personal account, ID ending `8569`; exact STS identity retained
  privately in `.cache/evidence/s3-final-review/runtime-identity.json`.
* Test identity verified before dispatch:
  `assumed-role/MilesHai03S3Test/hai03-s3-final-review`.
* AssumeRole session duration: 900 seconds. Browser-authenticated IAM user
  `miles-admin` obtained the temporary runtime session; no root/static access key
  was used for test execution. Test credentials were cleared from the child
  process environment in `finally`; this is not a claim of session revocation.
* [Runtime trust and inline permissions](runtime-policy.json): exact IAM user
  can assume role; role has bucket List/GetLocation, Get/Put on packs and
  packs-index prefixes, and GenerateDataKey/Decrypt on the exact CMK. The test
  role has no delete or provisioning permissions. Cleanup used the admin session.

Manual personal lab provisioning was authorized through the conversation.
No shared Terraform backend/state was initialized/reused; no Terraform apply,
EC2, NAT, ALB or DynamoDB provisioning was performed for these tests. The
runbooks' identity, explicit inventory, narrow runtime permissions and auditable
exact cleanup principles were followed; shared Terraform deployment guards and
the group's full runtime deployment were not exercised. Hung must review this
scope difference before accepting the environment evidence.

Committed AWS audit JSON replaces the account ID inside ARNs with
`PERSONAL_ACCOUNT_ID`. Credentials, MFA material and subscriber email addresses
are excluded. Exact object keys, version IDs and Go test events are retained.

## Real test result

Invocation with explicit bucket/region/CMK, temporary credentials and
`MILES_S3_DEV_ACCEPTANCE=1`:

```text
go test -count=1 -json -timeout=5m ./internal/storage/s3 -run '^TestS3DevWarmContract$'
```

[Raw events](go-test.jsonl): **PASS, no SKIP**, 19:31:54-19:31:56 UTC+07:00.

| Test | Result | Elapsed |
|---|---|---|
| immutable_pack_range_and_generation | PASS | 1.83s |
| body_errors_and_cancellation_leave_no_pack | PASS | 0.22s |
| TestS3DevWarmContract | PASS | 2.04s |

[Baseline versions](versions-before.json) exactly matched the prior three
audited versions. [After-run listing](versions-after.json) contained six versions:
exactly three more (21 bytes), one per new key; no retry-created extra version.
[Object HEAD audit](object-audit.json) confirms STANDARD, expected SSE-KMS CMK
and Bucket Key on all three new objects. The failed-body subtest produced no
versions. [Bucket audit](bucket-config.json) confirms Singapore, Versioning,
BucketOwnerEnforced, all public-access blocks, SSE-KMS and Bucket Key enabled.

## Controlled cleanup

After preserving evidence, the operator re-read full version inventory and
required exact equality with the six known key/version pairs. Cleanup aborted on
unexpected versions, delete markers or multipart uploads. No broad prefix delete,
bucket empty operation, Object Lock bypass, Terraform destroy or key deletion
was used.

[Reviewed target inventory](cleanup-plan.json) records six exact versions / 42
payload bytes from the two runs, at 12:32:54 UTC. Each DeleteObject request named
both its full key and VersionId. [Responses](cleanup-results.json) retain
requested/returned version IDs. Subsequent [version listing](versions-after-cleanup.json)
and [multipart listing](multipart-after-cleanup.json) show **zero versions,
zero delete markers, zero multipart uploads**. Original run evidence remains in
Git and private local copies; all six test object versions have been removed.

[Retained resources](retained-resources.json), checked 12:33:09 UTC:
bucket exists; CMK `3d9ce284-47e8-460a-be55-86916485bac8` is Enabled;
`MilesHai03S3Test` role remains. These are intentionally retained for further
development. This is audited artifact cleanup, **not full infrastructure teardown
or cost zero**. The enabled CMK can continue accruing its hourly prorated monthly
key charge; unrelated EC2/EBS/account resources were not audited or modified.

## Budget observation

[Read-only budget audit](budget-audit.json): monthly COST budget named
`My Zero-Spend Budget`, limit 1 USD; ACTUAL GREATER_THAN 0.01 USD notification
with one EMAIL subscriber. Addresses are omitted. This proves current configuration
only, not delivery, pre-provisioning coverage, a hard spending cap, or the final
bill after credits. No budget/notification was created or modified by this run.

## Validation and remaining acceptance

On the integrated source, Windows equivalents of every `make check` target passed:
gofmt, `go vet ./...`, `go test ./...`, all six Python unit tests, document-name
checks and both trimpath builds (miles/dsgctl). `make` is unavailable locally;
CI must run actual `make check` and Linux race checks on the pushed final head.
Local race remains unavailable without CGO/C compiler. Targeted DynamoDB tests,
including real-module and negative GSI/LSI/TTL/resource drift checks, passed.

An additional local infrastructure Python suite ran 12 tests: 10 passed; two
private-plan tests errored before assertions because Windows denied symlink
creation (WinError 1314) and its python3 app alias could not launch (WinError
1920). These unmodified infrastructure tests must pass in the Linux CI job;
the local invocation is not reported as a passing suite.

The physical Warm live test and controlled artifact cleanup have execution
evidence. The [27/10/2026 consolidated lead review](https://github.com/KinasTomes/miles/pull/35#pullrequestreview-5668488011)
found the W03 contract/handoff and evidence cross-check satisfactory, replacing
the earlier separate Chien/Phong/Hung sign-off gate. No individual signatures
are claimed. Final merge requires canonical checksum verification, green CI on
the final head and lead re-review/approval. See the
[handoff matrix](../../../contracts/storage-handoff.md).
No DynamoDB adapter, shared Terraform deployment, distributed fencing/sealing,
multipart runtime, archive restore or multi-node commit proof is claimed.

## Canonical checksum correction

The original manifests were calculated from Windows CRLF working files, while
Git stored normalized LF blobs. This caused 13/14 final-run and 5/6 initial-run
checksum failures on Linux; it did not change or invalidate the S3 results.
Both manifests are regenerated from raw staged Git blobs after final document
edits. Evidence files are pinned to LF in `.gitattributes`, and CI verifies both
manifests using `sha256sum -c` in its fresh Linux checkout. Captured logs/audit
results are preserved; no AWS rerun or reprovisioning is involved in this fix.
