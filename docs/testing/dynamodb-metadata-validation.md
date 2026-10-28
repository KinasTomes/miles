# DynamoDB metadata validation — W04-HAI-01

Implemented and checked on 2026-10-28, from main
`aa219407850dd6178e3e8906e10599daf2a4e1c3` on
`codex/w04-hai-01-dynamodb-metadata`. This is local adapter acceptance, not
AWS or multi-node acceptance. Historical W03 evidence remains unchanged.

## Checklist and commits

1. Native MetadataStore reads, canonical/location CAS and quota/reservation:
   `567f6bb`. Typed native attributes preserve integer precision and CRC presence;
   BatchGet deduplicates, splits at 100 and retries unprocessed keys explicitly.
2. Atomic publication: `30dc30e`. One final transaction publishes head/tombstone,
   quota, ledger, outbox, original outcome, manifest publication and optional
   completion state/receipt. Observed facts are coalesced by physical target.
3. Unknown outcome and exact retry: `edc9877`. A durable pending op stores the
   concrete SDK request/token before final dispatch. Restart replays those exact
   parameters; known cancellation retires the old attempt through CAS before
   recomputation. Pending state blocks reservation claim/release. A committed
   op retains its original response after later overwrite/delete.
4. Acceptance/handoff: the commit containing this document adds mandatory Local
   CI, SDK serialization and throttle tests, actual writer/reader integration,
   and bounded-memory ordered outbox discovery/export progress.

## Reproduce

Use Go from `go.mod`, Docker Compose and the repository's pinned Local image:

```powershell
docker compose -f deploy/local/compose.yaml -p miles-hai04 up -d dynamodb
$taskAddress = docker compose -f deploy/local/compose.yaml -p miles-hai04 port dynamodb 8000
$env:MILES_DDB_LOCAL_ENDPOINT = "http://$taskAddress"
$env:MILES_DDB_LOCAL_REQUIRED = '1'
go test ./internal/metadata/dynamodb ./internal/metadata/badger ./internal/storage/contracttest -count=1 -timeout=10m
go test ./... -count=1 -timeout=10m
go vet ./...
go build ./...
docker compose -f deploy/local/compose.yaml -p miles-hai04 down --volumes --remove-orphans
```

`MILES_DDB_LOCAL_REQUIRED=1` makes missing configuration fail instead of skip.
The Local test client accepts explicit HTTP `127.0.0.1:port` only, uses dummy
credentials, creates a random private inventory of 20 tables per fixture, and
deletes only that inventory. Readiness checks the API with a deadline. CI starts
the pinned ephemeral container, runs the suite with `-race`, and always removes
its own Compose project. The Windows run does not claim a Linux race result.

Observed local results with Go 1.27.2 and pinned DynamoDB Local 3.3.0:

- `go test ./... -count=1 -timeout=10m`: PASS, 28 packages with tests;
  DynamoDB 71.075s, Badger 25.460s, readpath 19.592s.
- Final DynamoDB-only rerun after owner revision guards: PASS, 74.028s.
- `go vet ./...` and `go build ./...`: PASS.
- `python -B -m unittest discover -s scripts/tests -v`: PASS, six tests.
- `python -B scripts/check_doc_names.py`, formatting and `git diff --check`: PASS.
- CI race execution is configured but has not run for this unpushed branch.

## Executed boundaries

| Boundary | Evidence in source |
|---|---|
| Shared Badger/native DDB semantics | `contract_test.go`: unchanged behavioral assertions in `contracttest.RunMetadata`, including quota overwrite/conflict/retry, auth/fence, empty and rejected manifests, receipts and no partial publication |
| Native typed persistence | `store_test.go`: integers above 2^53, required fields; schema/inventory and native item size tests |
| Batch absent vs unavailable | `local_test.go`, `store_test.go`: 105 keys, duplicates, partial/unprocessed response and exhausted budget |
| Bounded publication | `publication_test.go`: 105 unique chunks do not become 105 final transaction conditions; owner is Published |
| Fault/restart/idempotence | `attempt_test.go`: unknown before final dispatch, lost committed response, exact request replay across Store restart, original result after overwrite, revoked grant and known conflict retirement |
| SDK transport | `wire_test.go`: official SDK Binary serialization and conservative guard vs actual serializer; oversize wire budget rejected |
| Writer to reader | `handoff_test.go`: production chunk/compress/pack/publish and Warm reader against native DDB Local; only the physical S3 service is simulated; full/range/cache reads and delete visibility |
| Export handoff | `recovery_test.go`: ordered ID cursor, empty filtered pages, immutable event comparison and idempotent export progress |

The shared fixture now generates valid frozen 32-hex op IDs for three legacy
labels; no behavioral acceptance assertion was relaxed.

## Runtime handoff and limits

Construct `dynamodb.New(Options{API, Tables, Now, Authority})` with the explicit
20-table physical inventory. `Authority` is required. It must implement
`WriterAuthority.Check`; object publication also requires
`ManifestAuthority.CheckManifest`. Every fact returned through `Observe` becomes
a condition in the same native transaction. The provider must establish real
admission, retained roots and closure safety; timestamps alone are insufficient.
The fixture provider is test-only and cannot enable a production runtime.

All non-page records use opaque `record_revision` CAS tokens. Logical aliases
for user/counters, operation result/bindings, upload/receipt and owner/publication
are merged into one action per physical target. Frozen page items remain keys
plus Binary body, with exact-body predicates for immutable retry.

Manifest membership is frozen Building → Closing → Verified → Published.
Verification streams pages under protection; the final transaction consumes the
descriptor rather than enumerating all chunks. A definite validation failure
before dispatch can reopen Building at a new revision; unknown I/O cannot.

Native guards reject more than 400 KiB/item, 100 actions or 4 MiB of complete
transaction images. Transport has an additional conservative 8 MiB budget.
The pending exact request must itself fit its 400 KiB operation item, so a
large bundle can fail earlier than the aggregate limit. It fails before final
dispatch, never by publishing part of a bundle. Pending request encoding is an
internal Go/SDK attempt format, not the frozen R2 event format; incompatible
future versions must retain a decoder/migration before resuming old attempts.

Object/bucket/pack list helpers paginate service reads but currently collect
and order the candidate view in memory; they are suitable for the MVP inventory,
not evidence of bounded total listing memory at arbitrary scale. Outbox discovery
keeps only Limit+1 candidates in memory but scans the base table for each logical
page because the frozen schema has no ordering GSI. Neither scan is a cross-page
snapshot or epoch completeness proof.

This task does not enable a DynamoDB production flag in the app. Distributed
authority/provider integration is a prerequisite for such wiring. Full multipart
Upload/Part/freeze APIs, kinds 3/4, expanded child/control-entity R2 producer
coverage, epoch closure/sealing/restore and real AWS fault/scale acceptance remain
separate work. No real AWS resources or account credentials were used here.
