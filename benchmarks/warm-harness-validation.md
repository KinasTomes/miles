# Harness validation

Task: W03-HUNG-02. Component implementation only; the complete direct-S3 versus
single-node Miles Warm baseline is pending runtime composition, LIST/DELETE and
authoritative event producer dependencies described in [the runbook](warm-harness.md).

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
- `git diff --check`.

No AWS resources were created or resumed. No direct AWS or Miles Warm performance
numbers, R1/R2 proof, backup30 run, cost saving or target throughput is claimed.
The task must remain open until the missing real paired-run artifacts exist.
