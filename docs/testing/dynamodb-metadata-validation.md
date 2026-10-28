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
- These are initial local results. See the review correction below for the
  integrated main baseline and additional fault/expiry coverage.

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

## PR 36 review correction

Review findings: pending reconciliation after credential/grant revocation (P1)
and reservation expiry before fresh/replayed publication (P2). The correction
starts from `2b15aa7`, which already includes main PRs 37 and 38. Historical
W03 acceptance files and their checksums were not changed.

`review_regression_test.go` reproduced five failing assertions before the fix:
credential cleanup, grant cleanup, expiry during verification, expired first
publication through pending replay, and retained outcome after revoke/expiry.
The baseline failures were captured locally before changing implementation.

### Reconciliation and closure

`CloseCommitAttempt(ctx, ReservationClaim)` is an explicit trusted adapter
operation. It reads the reservation/op binding and original outcome. If already
committed, it returns that original result without re-publication. Otherwise it
requires a registered writer; a different writer must establish that the original
authority is closed in the same service incarnation. Its observations and exact
operation revision are CASed in one native transaction.

Successful closure writes a terminal `Closed` state plus `closed_attempt`,
removes pending bytes, and changes `record_revision`. Original registration and
final transactions both depend on that operation row, so they cannot publish
after closure wins. If final publication wins first, closure retries and reads
the committed result instead. A closed logical operation cannot reopen; further
execution requires a new logical operation under current permission.

Revoked credential/grant rejection on pending replay now closes the operation
under reconciliation authority before returning AccessDenied. An unconfirmed
closure, failed authority check or unreadable outcome stays ErrUnavailable.
Ordinary claim/release still refuses genuinely pending attempts. Cleanup runs
only after confirmed closure and normal reservation ownership/revision checks.
Committed outcomes remain available through GetCommitOutcome/CloseCommitAttempt
after signing credential or execution lease expiry. CommitObject retries still
enforce the shared stale-writer contract; with a valid writer they return the
original result after reservation expiry or credential revocation. These internal
metadata capabilities do not bypass public API auth.

This method does not auto-clear `coordination.Writer` unknown state or release
R1 pins. In particular, GetCommitOutcome returning ErrNotFound is still not a
no-commit proof for WriteSession.Reconcile. Shared durable closed-outcome/R1
provider integration remains separate; callers must retain protection when that
consumer cannot yet consume an authoritative closure proof.

### Dispatch expiry and SDK retry boundary

The same remaining-authority guard runs after verification/serialization,
before attempt registration, after registration and before pending replay sends.
It checks current writer, authorization, bucket/domain, reservation binding,
state/revision, reservation deadline and optional receipt expiry. Equality is
expired. Already-committed outcomes are read before those execution checks.

Every final SDK call overrides its Retryer with NopRetryer. Hidden SDK retries
cannot dispatch behind the guard; an ambiguous call requires explicit exact
replay after another guard. Native transaction conditions still check the
original observed records. This is an adapter dispatch boundary, not a claim
that DynamoDB offers a server-current-time transaction condition.

### Additional acceptance cases

- Credential and grant revocation permit cleanup without restoring permission.
- Original committed outcome is retained after revoke and expiry, without a send.
  An expired execution writer is still fenced on CommitObject; trusted outcome
  reconciliation can read the result independently.
- Closure/final publication races run in both orderings on native Local.
- Replaying the saved original request after closure fails its native condition.
- Unknown closure before commit retains quota; lost closure ACK needs a fresh
  authoritative read; repeated cleanup never subtracts counters twice.
- An unregistered replacement cannot close an attempt. A registered replacement
  can close, claim and release after the original fence has changed.
- Expiry during manifest verification, pending replay and registration blocks
  first publication. Tests retain an independently valid writer lease.
- An official-SDK HTTP fault test confirms one final send for a retryable 500;
  the outcome stays unknown and cannot release quota.

The previous grant-revoke/restore test is now a permission-preserving revision
conflict test. Stronger independent revocation tests cover permanent revocation
and cleanup instead of requiring reactivation to make progress.

Correction-round local results:

- Affected suite PASS without cache: DynamoDB Local 55.849s; Badger 22.142s;
  shared contracttest 0.516s; engine 2.051s; engine/write 1.872s; coordination
  0.382s; coordination/fake 0.380s; readpath 18.801s.
- `go vet ./...`, `go build ./...`, six Python tests, document names, gofmt and
  `git diff --check`: PASS.
- The attempted Windows `go test ./...` also exposed POSIX 0600/0700 permission
  and directory-sync failures in unchanged main-PR-38 CLI/adminclient/local app/
  secretfile code. Those failures are not recorded as a full-suite PASS. The
  correction uses Linux PR CI for full-suite/race validation; results belong to
  the exact pushed head and PR check links.
