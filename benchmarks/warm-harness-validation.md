# Harness validation

Task: W03-HUNG-02. Runtime composition, LIST/DELETE and authoritative event producers
are now integrated after merged PR #41/#42. The complete direct-S3 versus
single-node Miles Warm baseline still needs a dedicated real AWS run, as described
in [the runbook](warm-harness.md). No further task is a code prerequisite.

The HTTP fixture in `internal/benchmark/run_test.go` is a protocol test, not a
fake performance baseline. It exercises the installed S3 SDK, signed requests,
streamed gzip bytes without transparent decode, concurrency and multipage LIST.
It verifies all 9 smoke files on the initial GET and 3 repeated passes. It also
covers corrupt GET hashes, wrong HEAD sizes, failed PUTs without SDK retries,
pagination loops/unexpected keys, cancellation, redirect rejection, manifest
hash/path/file validation and preserving unrelated keys/existing output.

Completed local gates:

- `make check`: fmt, vet, all Go/Python tests, document names and binary builds.
- `go test -race -count=3 ./internal/benchmark ./cmd/warmbench`.
- Follow-up: race on app, Badger, object, readpath, coordination, config, Miles and harness.
- `make test-local`: real binary + AWS CLI PUT/GET/HEAD/Range/LIST/DELETE; full-file
  hash before/after process restart, backend failure readiness and clean shutdown.
- `git diff --check`.

## Binary smoke run actually executed

On 2026-10-28, built `bin/warmbench` with `go build -trimpath` from clean source
`40b13ff2d91c5070ef966aab0c4f42a1ef46d0e5` using Go 1.27.2. Executable SHA-256:
`7aa0efd748da5a18b3ab9fddb789f87b82ba229b51ca792c748147907bca349d`.
Both artifacts record `vcs.modified=false` and the installed SDK versions.

Started only the pinned S3 emulator from `deploy/local/compose.yaml`:

```bash
docker compose -f deploy/local/compose.yaml -p miles-warmbench-smoke up -d s3
docker compose -f deploy/local/compose.yaml -p miles-warmbench-smoke port s3 5000
```

Image: `motoserver/moto:5.1.15@sha256:8860b18917e1ee4d067df19a406ae95dc26037e6965d5b499e1cffd33755b914`.
Created a dedicated test bucket using a loopback HTTP PUT, then generated dataset
`smoke`, seed `miles-w02-v1`: 9 files, 589914 logical bytes. Config/credential files
were 0600, dummy emulator credentials only, target explicitly `local-s3-smoke`.
Actual host/CPU/filesystem and declared emulator limits are in each artifact.

Executed the [runbook command](warm-harness.md#reproduce-a-run) with this endpoint,
the same binary/manifest/read-repeats=3, once with concurrency 1 and once with 4.
Both completed 10 phases and 66 operation samples. Every run verified 36 full-file
GET hashes (9 files × 4 passes), with zero failed samples. An independent LIST
confirmed the bucket empty after both runs; then removed only this fixture:

```bash
docker compose -f deploy/local/compose.yaml -p miles-warmbench-smoke down --volumes --remove-orphans
```

Raw evidence: [concurrency 1](evidence/harness-smoke/concurrency-1.json),
[concurrency 4](evidence/harness-smoke/concurrency-4.json),
[SHA-256 inventory](evidence/harness-smoke/sha256sums.txt).
No payloads, private config/endpoint/bucket, credential paths or tokens are included.
These are two emulator harness runs, **not a direct-S3/Miles comparison**. Their
latencies must not be used as a production baseline or a task performance result.

No AWS resources were created or resumed. No direct AWS or Miles Warm performance
numbers, R1/R2 proof, backup30 run, cost saving or target throughput is claimed.
The task must remain open until the missing real paired-run artifacts exist.
