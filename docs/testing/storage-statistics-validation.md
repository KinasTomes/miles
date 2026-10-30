# Storage statistics validation

W04-HAI-03 starts from main 92deb23, 2026-10-30. No AWS resources are provisioned.

## Allocation and physical inventory

Calculator tests verify overlapping bucket unique totals, equal distinct-bucket
payload allocation, self-repeat versus external sharing, verified overhead,
fractional/global residual policy and denominator-zero/incomplete-inventory nulls.
S3 inventory tests exercise native SDK version-list inputs, current/noncurrent
versions, marker-only pagination and budget rejection. Existing S3 PackStore
contracts continue to pass. Scope/methodology is frozen in storage-statistics.md.

## Endpoints and actual adapters

Three GET routes run through existing Admin bearer/user/role authorization and
error envelopes. Tests replace pending authorization fixture rows and cover the
global endpoint, one-time Unicode/percent/plus/slash encoding, malformed/duplicate
query rejection, no-store, null ratios and absent-provider unavailable behavior.

The shared observation suite uses actual committed metadata on Badger and native
DynamoDB Local. It verifies bounded pagination, admin discovery independent of S3
grants, head/manifest/canonical collection, global/bucket/layout values from one
report, immutable returned copies, no inventory scan on report reads, quota
unchanged by observation, failed refresh and stale-report rejection.

Executed in Linux (Go 1.27.2, required pinned DynamoDB Local, dummy credentials):

```text
go vet ./internal/stats ./internal/api/admin ./internal/app ./internal/metadata/...
PASS
go test ./internal/stats ./internal/api/admin ./internal/app -count=1 -timeout=4m
PASS stats 0.004s; admin 5.172s; app 7.475s.
go test ./internal/metadata/badger ./internal/metadata/dynamodb -run Observation -count=1 -timeout=3m
PASS Badger 0.121s; native DynamoDB 1.144s.
```

Local/benchmark runtime owns the background collector and joins it before closing
metadata. Global and bucket handlers read cached reports; layout additionally
rechecks bucket incarnation and current head revision. Collection remains an
approximate observation, not a distributed snapshot, integrity certification,
billing report or R1/GC permission. Missing metric producers return null, not
fabricated revival/cache/restored counters.
