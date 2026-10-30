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

## Property, integrity and concurrency

An independent reference oracle exercises 60 deterministic seeds: overlapping
bucket references, repeated occurrences, the same hash across two domains,
raw/zstd stored lengths, empty objects, retained noncurrent versions, sidecars
and order-independent allocation/conservation. Tests count bytes from a real
production pack and sidecar and reject stored CRC corruption, wrong raw SHA,
corrupted index and wrong generation through existing production verifiers.
Calculator integrity guards reject overlapping distinct-identity ranges and
metadata/physical size mismatch. Layout explicitly reports metadata-only integrity.

Both actual adapters race canonical insert and location CAS (one winner each).
The collected report counts both physical packs but only one live identity, and
leaves quota unchanged. An actual canonical CAS injected during inventory makes
refresh fail with conflict and retains the previous report; a later stable
refresh succeeds. Collector cancellation joins work without publishing zeros.

Final Linux verification, Go 1.27.2, MILES_DDB_LOCAL_REQUIRED=1:

```text
go vet ./... && go build ./...
PASS
go test ./... -count=1 -timeout=10m
PASS all packages; Badger 32.308s; native DynamoDB 21.020s.
go test -race ./internal/stats ./internal/api/admin ./internal/metadata/... ./internal/storage/... ./internal/engine/... ./internal/manifest ./internal/pack ./internal/readpath ./internal/app -count=1 -timeout=10m
PASS; stats 1.296s; Badger 40.682s; native DynamoDB 50.309s.
go test -race ./internal/metadata/badger ./internal/metadata/dynamodb -run Observation -count=3 -timeout=4m
PASS Badger 1.918s; native DynamoDB 3.984s (includes during-collection CAS).
go test -race ./internal/stats -count=10 -timeout=3m
PASS 2.455s (600 seeded reference-oracle cases plus integrity/lifecycle tests).
python3 -B -m unittest discover -s scripts/tests -v
PASS 19 tests in Linux.
```

Format, tracked document names and diff whitespace checks pass. Windows tooling
cannot import deployment's fcntl; those tests pass in Linux, not by skipping or
weakening them. DynamoDB fixtures use dummy credentials and isolated table names;
the task's compose project is removed after verification. No real AWS or
distributed R1/R2 acceptance is inferred. Planning files remain untracked.

Final HTTP/cache refinements also passed vet and focused Linux race regression:

```text
go vet ./...
go test -race ./internal/api/admin ./internal/stats ./internal/metadata/badger ./internal/metadata/dynamodb -run 'Statistics|Observation|Collector' -count=1 -timeout=4m
PASS admin 1.815s; stats 1.019s; Badger 1.471s; native DynamoDB 2.293s.
```

These cases include actual Badger collector-to-HTTP responses, distinct bucket/
object 404 mapping, one collection ID shared by global/bucket reports, and a new
committed object absent from cache returning unavailable until the next refresh.
The latter is exercised through both real adapters, followed by external-sharing
verification after refresh. Coverage absence is not treated as resource absence.
