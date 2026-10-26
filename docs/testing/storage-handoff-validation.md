# Storage handoff acceptance

Task W03-HAI-03. Baseline main: `87d0769`. This document separates offline
acceptance from an actual AWS execution; a skipped test is not live evidence.

| Boundary | Reusable evidence | Backend |
|---|---|---|
| Metadata, CAS, quota, outcomes, auth and tombstones | `contracttest.RunMetadata` | Fake and Badger |
| Immutable PUT/retry/collision, exact ranges, generations, input failures, STANDARD sidecar | `contracttest.RunWarmPacks` | Fake via RunPacks, S3 SDK double, opt-in S3 dev |
| Controlled archive transitions/restore expiry and deletion | Remaining `RunPacks` cases; S3 adapter fault/class tests | Simulator only; not claimed as AWS archive acceptance |
| Write/prepare -> overwrite/commit/retry -> full GET/Range -> delete despite populated cache | `readpath.TestWriteCommitWarmReadHandoff` | Real writer, pack codec, Badger, S3 adapter and Warm reader; SDK service double |
| API error vocabulary | `s3.TestMetadataWriteError`, metadata contract bucket/auth/quota tests | Offline |

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
compiler. No live AWS acceptance was run for the offline results above.

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

Live status for this task: **not executed; environment unavailable**. Complete
checklist 1 live acceptance only after attaching a real execution record.
