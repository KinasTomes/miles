# Recovery epoch validation

Task [W04-HUNG-04](https://miles.extase.dev/tasks/W04-HUNG-04), baseline `98ad195`.
Implementation and downstream contract:
[recovery-epoch-implementation.md](../contracts/recovery-epoch-implementation.md).

| Checklist / acceptance | Executable proof |
|---|---|
| Export60s; Standard pages/descriptors; verify before mark; immutable retry | Existing exporter/S3 fault suite; new worker cadence/heartbeat/join test; signed S3 SDK against Moto |
| Recovery-cut blocks all new authoritative producers; admitted cohort completes; Closed/E+1 atomic | Native shared R1 control transactions, exact retained generation receipts; healthy producer/reader/abort and lost final ACK tests |
|120s wait budget releases gate without killing healthy requests | Native UTC equality case and worker monotonic timeout despite wall rollback |
| Complete consistent pagination; exact membership; count/hash/predecessor; fenced publication |137 native producers/events, >100 cohort, >128 membership leaf, independent Outbox scan, explicit empty successor epoch |
| Missing event/input never seals | Truncated discovery scan, deleted base event, missing retained producer payload; no marker and state remains Closed |
| Worker death/takeover; stale publish rejected | Resume paged proof cursor; retained reused lease generations; expiry+5s closure before transfer; marker written before pointer/lost ACK; identical retry bytes |
| Closed distinct from Sealed; unsealed incomplete | S3-only VerifyCut rejects Open/Closed and missing predecessor; malformed/golden schemas; no mutable metadata input to verifier |
| Job backend/authority reuse | Native journal_flush accepts recovery fence; GC rejects that same authority |

## Reproduce without AWS

Use the pinned ephemeral fixtures; the compose project below owns only these
containers. No host AWS credentials/default SDK chain, IMDS, proxy or external
endpoint is used by the new S3 test. Existing native DDB fixture requires literal
loopback and uses dummy credentials. Each test cleans only its own random tables
and the S3 test removes its own synthetic bucket/objects.

```bash
docker compose -f deploy/local/compose.yaml -p miles-recovery-acceptance up -d dynamodb s3
```

Get the two ephemeral loopback ports with `docker compose ... port dynamodb 8000`
and `docker compose ... port s3 5000`, then use their actual URLs:

```bash
MILES_DDB_LOCAL_ENDPOINT=http://127.0.0.1:DDB_PORT \
MILES_DDB_LOCAL_REQUIRED=1 \
MILES_RECOVERY_S3_ENDPOINT=http://127.0.0.1:S3_PORT \
MILES_RECOVERY_S3_REQUIRED=1 \
go test -v -race -count=1 -timeout=10m ./internal/metadata/dynamodb

go test -race -count=1 ./internal/recovery ./internal/storage ./internal/coordination/...
make check

docker compose -f deploy/local/compose.yaml -p miles-recovery-acceptance down --volumes --remove-orphans
```

The existing `dynamodb-contract` CI job now starts both pinned emulators and
requires the signed S3 end-to-end test. `make check` alone skips native fixtures
when endpoints are absent; it must not be reported as native/emulator evidence.

## Local results

Executed on Linux/aarch64: focused native recovery fault tests with race detector,
full native DynamoDB race suite, signed S3 SDK/Moto end-to-end, shared
recovery/storage/coordination race tests, and `make check`. The137-member case
crosses both transaction page and membership leaf boundaries. Test logs are kept
privately under `/home/ubuntu/.local/state/miles/recovery-*.log`; CI provides
reviewable evidence on the PR head.

No live AWS, real NTP/UTC source attestation, full checkpoint/rebuild, physical
retention/GC permission or production service composition is claimed. Those
retain their own downstream acceptance; they do not invalidate the W4 local
export/close/seal evidence above.
