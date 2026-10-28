# Local runtime and CLI validation

Task: W03-HUNG-03 — CLI nền, health thật và tích hợp MVP.

Validated on Linux arm64, 28/10/2026, from main
`aa219407850dd6178e3e8906e10599daf2a4e1c3` (includes PRs #32–35).
Runtime implementation: `b532e04`; CLI transport: `a1663bd`.
Tools: Go 1.27.2, Python 3.12.3, AWS CLI 2.38.1, Docker Engine 29.9.1.

## Checklist and proof

| Task requirement | Implementation and validation |
|---|---|
| Bootstrap/config to dev backend | Explicit local mode wires persistent Badger, SDK S3 PackStore against literal loopback, and bounded LRU. A one-time non-admin dev identity is persisted to a private credential file. Unit and process tests reopen the same metadata and retain credential/bucket identity. No live AWS credential chain is loaded. |
| Real dependency health and controlled shutdown | Cached bounded probe reads Badger, HEADs the physical local bucket and checks cache. Process smoke removes the physical bucket: ready503, health200; it never auto-recreates the bucket. SIGTERM waits for admitted work, shuts both listeners, then closes Badger. Timeout reports incomplete and does not close storage under live work. |
| Admin CLI transport | Read commands send bearer only to Admin paths. Tests check private regular token files, HTTPS/loopback boundary, no redirect, bounded JSON response, timeout, error redaction, auth failure, and unsupported exit behavior. The test Admin server checks the bearer; no production Admin route is claimed. |
| Hard deadline and independent admission | S3 request context and socket read/write share one hard deadline. A stalled body read is interrupted. Cancellation keeps admission until handler quiescence. Draining rejects a signed request with S3 XML503. Dependency readiness is never an admission boolean: the process test still serves a metadata-only HEAD while the physical S3 bucket is absent. |
| Integration with merged MVP modules | Real binary + AWS CLI exercise SigV4 CreateBucket/HeadBucket/ListBuckets on Badger. A backend credential is rejected and object GET returns NotImplemented. Existing whole-repo contract tests remain green. Object HTTP handlers remain W03-CHIEN-02/03. |

## Reproduce

```sh
make check
go test -race ./internal/app ./internal/config ./internal/observability ./internal/adminclient ./internal/secretfile ./cmd/miles ./cmd/dsgctl
make test-local
```

All commands passed. `make test-local` also retains the existing isolated
DynamoDB Local transaction/S3 conditional-write checks and Edge/dev,
Cloud/dev, Cloud/runtime health/shutdown smoke. It runs Edge/local twice
with the same data directory, then removes the temporary Compose project,
processes, files and test buckets. It uses dummy credentials and validated
loopback endpoints, never the operator AWS profile.

## Scope and downstream handoff

- `cmd/miles/s3.go` owns the HTTP composition. API consumers add real object
  handlers here; they inherit the process gate and immutable context deadline,
  and must retain their authorization, live dependency and R1 checks. A reader
  is available in `internal/readpath`; this task does not duplicate its logic or
  add HTTP object handlers on behalf of W03-CHIEN-02/03.
- W04-CHIEN-01 extends `cmd/dsgctl`/`internal/adminclient` with actual identity,
  token/grant/policy routes. W04-HUNG-02 supplies job/node/cache implementations.
  Unsupported/missing endpoints and offline commands return errors today.
- W04-HUNG-01 consumes health/drain for deployment. Local mode is loopback-only
  and explicitly labels simulated clock readiness. Runtime remains unready
  until real clock/backend authority is configured; this is not production
  Cloud/Edge deployment, TLS, ALB/ASG deregistration or distributed R1/R2 proof.
- No live AWS apply or paid resources were needed. Warm object upload/download,
  multipart, full P0 `cp/ls/rm`, networked DynamoDB metadata, and durable
  pack/outbox flushing are not claimed by these results.
