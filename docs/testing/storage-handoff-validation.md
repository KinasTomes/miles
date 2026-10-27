# Storage handoff acceptance

Task W03-HAI-03. Baseline main: `87d0769`. This document separates offline
acceptance from an actual AWS execution; a skipped test is not live evidence.

## Approved design decisions

Lead decisions and final merge gate:
[PR #35](https://github.com/KinasTomes/miles/pull/35#issuecomment-6436239260).
W04 keeps exactly 20 base tables, no GSI/LSI/TTL, strong base-table reads and
conditional transactions; bounded Scan discovers candidates only. SystemConfig
operation/manifest-owner records are isolated from global-policy. Prepare/freeze/
verify/protect precedes a single atomic head/quota/ledger/outbox/outcome/owner
publication (plus upload state/receipt for Complete). Overflow fails closed;
publication cannot be split. Part kind-2 is rebased into a fresh kind-1 manifest;
head page_index is Binary little-endian u64, <=1024 entries / 8192 bytes.
These decisions are approved; implementation/fault proofs remain W04 obligations.

## Offline contract evidence

| Boundary | Reusable evidence | Backend |
|---|---|---|
| Metadata, CAS, quota, outcomes, auth and tombstones | `contracttest.RunMetadata` | Fake and Badger |
| Immutable PUT/retry/collision, exact ranges, generations, input failures, STANDARD sidecar | `contracttest.RunWarmPacks` | Fake via RunPacks, S3 SDK double, opt-in S3 dev |
| Controlled archive transitions/restore expiry and deletion | Remaining `RunPacks` cases; S3 adapter fault/class tests | Simulator only; not claimed as AWS archive acceptance |
| Write/prepare -> overwrite/commit/retry -> full GET/Range -> delete despite populated cache | `readpath.TestWriteCommitWarmReadHandoff` | Real writer, pack codec, Badger, S3 adapter and Warm reader; SDK service double |
| API error vocabulary | `s3.TestMetadataWriteError`, metadata contract bucket/auth/quota tests | Offline |
| Part staging namespace, immutable retry, writer fence, wrong kind and reopen | Shared part contract; Badger `TestPartPagesSurviveReopenAndRejectWrongKind` | Fake/Badger |
| Physical schema alignment, domain/generation identities, auxiliary namespaces | DynamoDB schema tests | Offline; no DynamoDB Store |
| Native sizes, full-page/head-index bounds, oversized metadata/envelope/receipt, duplicate targets and aggregate transactions | DynamoDB item/limits/wire tests, manifest index tests | Native SDK attributes and SDK serializer seam; no AWS call |

The Warm core intentionally performs no storage-class changes or deletes. Raw
byte PackStore tests check the physical port; the writer/read handoff additionally
checks real pack/index/chunk CRC and decoding. WarmGate/test writer protection is
one-process, with no concurrent maintenance, not a durable R1 lease proof.

## Reproduce offline

```sh
go test ./internal/storage/... ./internal/metadata/... ./internal/engine/... ./internal/manifest ./internal/readpath ./internal/multipart ./internal/api/s3
go test -v ./internal/storage/s3 -run '^TestS3DevWarmContract$'
```

The second command must report SKIP unless explicitly enabled. Full `make check`
and CI's Linux race job remain required. Local Windows race needs CGO and a C
compiler. The offline results above are separate from the personal-account live
run recorded below.

## Opt-in S3 dev

The environment in W03-HUNG-01 was torn down; see
[AWS readiness](aws-dev-readiness.md). Provision/resume a reviewed disposable dev
environment separately. This test never provisions infrastructure or changes IAM.

Supply an approved temporary runtime session as `AWS_ACCESS_KEY_ID`,
`AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`, plus `AWS_REGION` and
`MILES_S3_DEV_BUCKET`. Optionally set `MILES_S3_DEV_KMS_KEY` to the existing CMK;
otherwise the bucket's default encryption is used. Never put credentials in Git,
logs, command arguments or evidence. Then enable the runner:

```sh
MILES_S3_DEV_ACCEPTANCE=1 go test -count=1 -v -timeout=5m ./internal/storage/s3 -run '^TestS3DevWarmContract$'
```

PowerShell: set `$env:MILES_S3_DEV_ACCEPTANCE='1'` for that invocation and unset
it afterwards. No default bucket, region, long-lived credential or anonymous
fallback is accepted. SDK calls have a 30-second HTTP timeout.

Every subtest allocates a random pack identity; logs retain its ID/generation and
the literal `other` generation used to test isolation. Objects use the normal
`packs/` and `packs-index/` layout. They remain STANDARD, including sidecars.
The runner retains objects/versions for operator-audited cleanup; it does not
request extra DeleteObjectVersion permissions or delete a shared prefix.
Record commit, region, runtime role, test JSON/output and exact created keys in
private evidence. Check encryption/ownership from the approved environment's
inventory. Archive restore, KMS denial, crash/fault scheduling and multi-node
publication require separate tests and are not implied by a Warm pass.

Live physical-port status: **PASS on 27/10/2026**, using a dedicated personal
S3/KMS lab in `ap-southeast-1`, temporary `MilesHai03S3Test` role credentials,
and source commit `f4a13f2a8c04ad12651056bc158a046602266879`. Both subtests passed
(no SKIP). AWS version/HEAD audit confirms exactly three new versions (21 bytes),
STANDARD and SSE-KMS with the expected CMK. Bucket ownership is
BucketOwnerEnforced, public access blocked, Versioning and Bucket Key enabled.
See [execution log and AWS audits](evidence/w03-hai-03-s3-live-20261027/acceptance-record.md).
This run used manually provisioned personal resources, not the shared Terraform
environment. Artifacts/key remain retained; audited cleanup, infrastructure/IAM/
budget consumer review and remaining PR #35 gates are still open. Live PASS does
not by itself complete task acceptance or permit merge.

## W03 checklist evidence and remaining gates

1. Acceptance suite and write/read integration run offline; S3 SDK double passes
   the same Warm core as fake PackStore. The opt-in live runner is prepared and
   skips by default. Actual personal-account S3 Warm evidence is linked above;
   controlled cleanup and consumer/runtime acceptance remain outstanding.
2. Schema/keys, zero-index baseline, record namespaces, consistency, conditions,
   bounded preparation and SDK API seam are prepared in dynamodb-adapter.md and
   Go helpers; table inventory alignment is tested. Consumer review is required
   before production mutation/registry/closure enablement.
3. PartPageStore, prepared-head binding, strict descriptor/index codecs and native
   size/transaction guards are implemented with tests. Multipart orchestration,
   full completion/R2 codecs and a networked DynamoDB MetadataStore remain W4.

Branch validation performed on Windows on 26/10/2026: `go test ./...`,
`go vet ./...`, `go build ./...`, all 6 Python unit tests, doc-name checks,
gofmt and `git diff --check` passed. An initial restricted run of the existing
readpath HTTP test was denied loopback access; the full run with loopback allowed
passed. CI configuration covers Linux race checks; no CI run is claimed for this
branch, and local Windows race is unavailable without CGO/C compiler. A checklist
implementation commit does not assert its live/consumer enablement gates passed.
