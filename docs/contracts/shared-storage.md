# Shared storage contracts — W01-HUNG-04

Status: implementation for review, not a production storage feature or task-completion approval. Miles public task `W01-HUNG-04` retains the historical source identity `W01-HAI-01` (A02). Prerequisite `W01-CHIEN-03` is Done. This document derives the storage boundary from architecture v4 and API v2; neither source document changes.

## Ownership and source mapping

| W1 checklist | Deliverable | Source |
|---|---|---|
| Shared MetadataStore, PackStore, Cache and versioned types | [model.go](../../internal/storage/model.go), [store.go](../../internal/storage/store.go), [validation](../../internal/storage/validate.go) | [Architecture §1.2, §10.1–10.2, §18](../dedup-storage-gateway-architecture.md), [API §2.2.3](../dedup-storage-gateway-api.md) |
| Reusable contract suite, metadata/pack/cache fakes | [contracttest](../../internal/storage/contracttest/metadata.go), [physical contracts](../../internal/storage/contracttest/physical.go), [reference wiring](../../internal/storage/contracttest/reference_test.go), [fake stores](../../internal/storage/fake/metadata.go) | Architecture §9.0, §10.2, §16; API §2.6/4.3/4.4 |
| Domain-aware PK, bucket incarnation, head/location CAS | ChunkKey.PartitionKey, NewBucketID, ExpectedRevision, WriterFence; identity/CAS/race tests | Architecture §1.5, §8.2, §10.1–10.2; API §2.2.3 |

`internal/storage` owns common Go types and ports so engine, API, metadata and physical adapters can import the same definitions without importing each other. Future BadgerDB/DynamoDB/S3 adapters implement these ports; engine orchestration does not belong in a backend adapter. No AWS SDK or new dependency is added.

These are typed logical records, **not a frozen binary or DynamoDB encoding**. `ModelVersion=1` is the first domain-aware Go model; it is not architecture v1 or a fallback to legacy hash-only identities. Footer/sidecar bytes, page encoding/size accounting and journal/checkpoint serialization remain the separate format tasks, with a freeze in W2. Unknown model versions fail validation before mutations.

Clock/global policy remain owned by [the Clock/policy contract](clock-and-policy.md). The fake uses fixed UTC with zero uncertainty for deterministic tests. It does not implement that contract's monotonic deadlines, bounded UTC uncertainty, NTP readiness, virtual scheduler or policy evaluator. Real adapters and coordinators must integrate those requirements.

## Identity, consistency and ownership of values

- `ChunkKey` retains both domain and lowercase SHA-256 of **raw** content. Its single DynamoDB partition key is exactly `domain_id#hash`; domain is 1–64 characters `[a-z0-9_-]`, hash is 64 lowercase hex characters. The same hash in another domain is another identity.
- Every bucket creation uses a new 128-bit random ID. Object, reservation, grant and completion bindings carry this incarnation. Same name/revision after recreation cannot authorize an operation admitted on the old bucket.
- `ExpectedRevision=0` means absent, never “ignore conflicts”. Creation assigns revision1. Overwrite, delete/tombstone and location CAS advance their own revision. Tombstones are visible to internal CAS readers; API handlers translate them to missing objects. Tombstones and old manifests are retained under R1/R2, not deleted by this port.
- `PutChunkIfAbsent` returns the canonical winner and whether this attempt inserted it. Losing physical blobs remain orphan candidates; losing publishers must use the returned winner. `GetChunks(..., consistent=true)` requires authoritative consistent reads. A fake may provide stronger consistency for false; adapters may not weaken true.
- A canonical `ChunkLocation` includes `CRC32C` of its stored byte range, alongside `PackRef`, range and codec. Insert/lookup and location CAS preserve the complete location atomically with `LocationRevision`; replacement stored bytes require the replacement CRC even when the raw hash is unchanged. Zero is a valid CRC value, so Badger/DynamoDB serializers must require presence of the field rather than silently default a missing field or omit zero. The shared contract tests nonzero and zero round-trips. R2 `location_set`, checkpoint and replay must preserve the same field; GET verifies CRC before bounded decode and raw length/SHA-256, and cannot choose a canonical winner from a sidecar.
- `PackRef` includes generation. Cache keys also include exact byte range. A class change preserves content generation. Range validation rejects zero/negative/out-of-bounds/overflowing ranges; empty objects need no physical range read.
- Returned maps/slices and submitted mutable values must not alias backend storage. Cache input/output byte slices are owned copies. Authorization precedes chunk lookup and **every cache read**, including a hit; physical cache ports do not grant user access.
- `Authorize` is a fresh authoritative rights projection. Owner receives read/write; a grant can give write without read; system admin receives no implicit object access. Caller-side snapshot age/node binding remains API §2.2.3 (at most5 seconds, no hit-based refresh); this port adds no authorization cache. Commit reads current authorization again.

## Required W1 query and policy ports

All six additional `MetadataStore` methods named in architecture §18 are part of this W1 contract, with shared request/result types and executable fake behavior. They are not deferred to adapter-specific interfaces.

| Port | Contract and source |
|---|---|
| `ListAccessibleBuckets` | Active principal's owned or read/write-granted buckets, ordered by name with exclusive `After` and explicit limit 1–1000. Admin has no implicit data access; recreated buckets do not inherit old-incarnation grants. Architecture §1.4/18; API §2.2/4.2. |
| `ListObjects` | Authoritative bucket-incarnation/read-right check; consistent key-order scan, prefix/delimiter grouping, tombstone filtering. `CommonPrefixes` count toward `MaxKeys`; internal request has an explicitly normalized limit 0–1000 (API supplies default/cap). Continue scanning duplicates/tombstones until a logical page or exhaustion. Continuation state overrides `StartAfter`, preserves last scanned key and delimiter state, and binds principal/bucket ID/prefix/delimiter. Architecture §10.2; API §4.2. |
| `GetPolicy` | Revision0 selects current; positive revision selects that exact immutable history record, missing history returns `ErrNotFound` without current fallback. Architecture §10.1/18; Clock/policy §4.1/7.2. |
| `PutPolicy` | Current active system-admin credential and writer fence, expected-revision CAS, complete validated candidate. Current/history/outbox publication is atomic. A semantic no-op under a matching revision changes nothing; a stale no-op conflicts. JSON decimal equality is exact; policy revisions stay within 1..2^53−1. Architecture §9.0/18; Clock/policy §4/6. |
| `UpdateAccessStats` | Generation-specific access observations merge by maximum timestamp; retries/older observations cannot move access time backwards or increment state revision. Callers authorize successful reads before scheduling observations. Architecture §7/10.1/18. |
| `ListPacksByState` | Explicit state filter, ordered `(pack_id, generation)` results and exclusive typed cursor, limit 1–1000. Metadata results are candidates, not proof that tier/delete may bypass R1 admission, roots or pins. Architecture §7/8/10.1/18. |

These are trusted internal ports. Principal IDs come from verified authorization, never arbitrary HTTP input. Typed cursors are internal scan state, **not wire continuation tokens**: the API owns opaque encoding, signature/expiry, request validation and response XML. The shared types prevent each adapter from inventing its own cursor semantics; token encoding and handlers remain their existing implementation tasks.

`PolicySnapshot.Document` contains the complete canonical global policy, not a patch. `internal/config` owns nested merge and field/type/scope/range validation from the Clock/policy contract; this store does not replace that validator. The reference fake checks the JSON-object envelope and tests exact decimal/no-op, authorization, revision, history and fault behavior. Revision0 is only an internal empty-store bootstrap precondition that publishes revision1; public policy updates still require the approved positive expected revision. Changed publications require a validated `SystemConfig/global-policy` outbox envelope. After a lost response, stale expected revision conflicts: reconcile current/history/op_id/outbox instead of silently applying with a new revision (Clock/policy §6.8).

## Atomic mutation boundary

| Operation | Required behavior |
|---|---|
| Chunk insert/location CAS | Atomically check the admitted writer's current incarnation/lease/fence and resource admission; insert one winner or compare the expected location revision. Reject stale workers. Physical pack/sidecar durability is a prerequisite. |
| WriteManifestPages | Each page is immutable and an identical write is retryable. **No all-or-nothing batch guarantee**: DynamoDB batch requests may leave some pages durable. Retry unprocessed items, verify completeness and serialized limits before commit. |
| Reserve/Claim/ReleaseLogicalQuota | Atomically bind owner counters and the reservation ledger; claim/release reconcile the authoritative operation outcome and condition-check ledger revision/fence. Retry never counts/releases twice; unresolved outcomes keep reservations. Logical bytes belong to bucket owner, not caller or dedup ratio. |
| CommitObject/DeleteObject | One backend transaction checks current credential/user/write grant, bucket incarnation/domain/state, head revision, writer fence, reservation and optional frozen completion state. It updates head/tombstone, logical quota and ledger, outbox, and Complete state/receipt together or not at all. |

The commit derives overwrite delta from the **current CAS-matched head**, consumes only its reservation and includes other outstanding reservations in the limit check. Same-size overwrite can use zero extra reservation at full quota; staged bytes remain independently bounded by owner limit +100GiB. Complete consumes staged/reserved bytes and the active-upload counter with its receipt. This is the final commit boundary and the PUT/Copy reservation recovery handoff; multipart provisional-part growth and upload-state takeover orchestration remain separate implementation tasks.

Outbox includes history/epoch, entity/object incarnation, resulting revision, op_id and immutable payload pointer/checksum. Receipt binds upload/bucket/key/initiator, normalized input digest, manifest, ETag/checksum and its own expiry. Neither is an opaque “success flag” unrelated to the published head. The producer must durably prepare and verify referenced payload/pages before the transaction. Transactions remain bounded; a real DynamoDB implementation must combine conditions with an Update on the same item rather than issuing both actions for it.

`WriterFence` is a typed proof handle, **not a lock implementation or permission supplied by an HTTP client**. R1's coordinator owns shared-gate admission, writer registration, lease renewal, read views/location pins, retirement and takeover. Mutations must validate their conditions in the same authoritative transaction. This PR tests rejection of expired leases/old fencing epochs/incarnations; it does not claim GC/read-pin safety or a distributed coordinator is implemented.

**R1 review synchronization — Hưng Q6, selected pending integration verification:** bounded local `U<E` with valid lease/fence/incarnation permits a new side-effect dispatch; it is not a strict commit-before-lease-expiry guarantee. A valid pre-E dispatch may commit/settle after E. Every authoritative metadata publication must check shared fence closure and incarnation in the same transaction as its mutation: an LP before closure remains committed even after E, while an LP after closure must fail the fence condition. Takeover atomically closes old authority, stabilizes membership and reconciles every in-flight/unknown outcome before granting new publication authority; no overlapping publication authorities. Request/reservation deadlines, current authorization and other publication conditions remain independent and unchanged. See [R1 §8](r1-and-cache.md#8-lease-fencing-takeover-và-reconciliation). The existing expired-lease fake tests are fixture behavior, not proof of commit-time UTC enforcement or this distributed protocol; adapter integration and affected consumers require review in the same PR.

`ErrUnavailable` can mean an outcome is unknown. Resolve using the same stable op_id and retained outbox/reservation/receipt state before cleanup. An immediate identical retry returns the original result without applying quota twice; changed logical input conflicts. Writer/completion takeover conditions are not part of immutable operation identity. Real adapters must persist/reconstruct this information from R2 records, not a process-local map. Retention follows the approved recovery/request/receipt windows; the fake keeps operation records for its test lifetime, not a new promise of infinite retention.

### Reservation lifecycle and unknown outcomes (R2)

`Reservation` carries `Version`, owner/upload/object incarnation, stable `OpID`, base head revision/size, reserved/staged logical bytes, `State`, CAS `Revision`, `Writer` fence and `Deadline` (architecture §9.2/10.1, `QuotaReservations`). `CommitRequest.ReservationRevision` conditions publication on the admitted ledger version. Base fields record the observed overwrite credit; final quota always uses the current CAS-matched head, never a stale base size.

| Action | Preconditions and atomic effects |
|---|---|
| Reserve | Create `Active/revision1` with current base head and valid writer/deadline; reserve counters once. Exact active retry is idempotent; a different operation or reused terminal ID conflicts. |
| Claim/reconcile | Read the authoritative stable-op outcome and ledger together. Already committed returns the original `CommitResult` plus retained terminal ledger without changing ownership/counters. If proved uncommitted, only `Active` + expected ledger revision + lost old writer + valid new fence in the same service incarnation may transfer ownership; increment ledger revision once. A live old writer or competing/stale claim conflicts. |
| Commit | Require active ledger/op/object/owner/revision, current owner fence and unexpired deadline; publish head/quota/outbox/optional receipt and `Committed/revision+1` together. Logical operation identity excludes only mutable takeover conditions (writer, completion conditions, reservation revision); changed immutable input still conflicts. |
| Release | Require matching op/revision/current owner fence and authoritative proof of no commit, then decrement reserved/staged counters and retain `Released/revision+1`. Same release retry is idempotent; a committed operation cannot be released. |
| Unknown result | `ErrUnavailable` on outcome lookup leaves ledger and counters unchanged. Missing head alone is not proof: check retained op/outbox/receipt under the backend's authoritative transaction protocol. Never infer failure from a timeout. |

Claim does not extend the original deadline or change op/base/upload binding. An expired reservation may be claimed for safe cleanup, but cannot publish merely because its worker changed. A new worker must reconcile before retry/release; old fences cannot mutate. The fake's committed-operation map represents durable R2 proof for its test lifetime; real adapters must transactionally persist/reconstruct this proof and terminal ledgers for approved recovery/idempotency windows, not rely on local RAM or immediate TTL deletion. Tests inject both pre-publication failure and lost ACK, then lose the old writer, claim with a new fence, and verify original result or one resumed commit with no double quota; concurrent claims have one winner.

## PackStore and cache boundaries

- `PutPack`/`PutSidecar` publish complete bytes or report an error; no partial body may become a valid completed object. Exact-generation identical retries are safe; different bytes conflict. A failed multipart backend upload is orphan work to reconcile, never a published pack.
- `HeadPack`/`RangeGet` address the exact generation. `RangeGet` returns compressed physical bytes; decoding and raw SHA-256 verification before emitting client bytes belong to the engine.
- Pack writes use Warm STANDARD. Sidecars remain STANDARD when packs move to STANDARD_IA, GLACIER_IR or GLACIER. The reference suite checks GLACIER_IR reads immediately and archive restore separately.
- `Restore(Days, Tier)` only accepts/starts the backend operation. `Days` must be positive and fit the S3 SDK `int32` field before any backend request; policy-specific upper limits remain configuration-owned. Successful submission **does not make the pack readable**. Only backend completion/HEAD supplies actual restore expiry. The fake has an explicit completion hook; tests verify Cold remains unreadable while restore is pending and at the expiry boundary. Request-deadline safety margins still belong to the approved GET/Copy admission logic.
- `ChangeStorageClass` permits Warm STANDARD/STANDARD_IA changes to supported classes, GLACIER_IR → GLACIER, and same-class idempotent retries. GLACIER_IR/GLACIER → STANDARD/STANDARD_IA and GLACIER → GLACIER_IR are rejected on the existing generation, including Restoring/Restored packs. Same-class retry preserves restore expiry/state. Promotion reads available bytes into a **new Warm pack/generation**, then the engine CAS-publishes chunk locations; the old Cold generation remains immutable until approved retirement. Tests cover all four Cold → Warm pairs, restored/pending rejection, no-op preservation and independent Warm data. Architecture §7.2/8.2; API §4.2/4.4.
- `ChangeStorageClass`/`DeletePack` cannot fence an already sent S3 request. The caller must first persist Transitioning/Deleting under the R1 maintenance/resource fence, reconcile uncertain backend results and respect pins/roots/protection/minimum retention. These trusted physical methods deliberately cannot be called a complete GC/tiering service. This fake does not simulate provider minimum-storage billing or asynchronous self-COPY reconciliation.
- `Cache.Evict` only affects the specified pack generation. Eviction/cache loss never changes authoritative metadata, rights or logical usage. No LRU/admission/read-ahead engine is added here.

**Cache review synchronization — selected pending integration verification:** [R1 §11–12](r1-and-cache.md) records best-effort `Cache.Put` (full/oversized/admission bypass may return nil; invalid input/cancellation remain errors), same-key different-bytes integrity conflict (no silent overwrite; evict/report the exact generation), and resident-only `Evict`; strong purge belongs to downstream job coordination. Write admission uses final exact physical identity/complete bytes and is never a durability/publication dependency. Read safety pins exact PackRef/generation for generation-level retirement; no range-pin requirement is added. Shared Go types/ports are unchanged. The current fake silently replaces same-key bytes and has no bounded admission policy; it is not evidence of these selected behaviors. Conflict error mapping, fixture/adapter changes and consumer compatibility remain a separate integration review item within the same PR, not implemented by this wording update.

## R2 handoff — selected W1, integration pending

[R2 OQ-01–09](r2-recovery.md#2-source-precedence-và-decision-register) chọn mutation/event identity, projections, closure receipts/proof tree, S3 completion-marker discovery và exact-generation retention. `OpID` của transaction là mutation_id; final object/receipt/reservation giữ original logical op_id, child mutations có parent binding và event_id history-scoped riêng. Không đổi existing Go signatures/fixtures trong docs update này.

W2+ mở rộng shared models/ports/validation tại boundary hiện có cho registry/attempt/outcome, producer/epoch closure, enumeration/export/seal/checkpoint; không tạo recovery MetadataStore trùng. Existing `OutboxEvent.Validate`/fakes chưa hỗ trợ đủ inventory, R2 field/byte schemas hoặc distributed closure. Production transaction phải atomically check authority/attempt revision và write entity effects/outbox/committed outcome, account tổng action/byte limits; reference suite hiện tại không chứng minh protocol này. Giữ R1 dispatch/closure và reservation/receipt semantics; serializer goldens/adapter fault proofs theo [R2 §18](r2-recovery.md#18-decisions-và-freezeenablement-gates).

## Reusing the suite

The [reference wiring](../../internal/storage/contracttest/reference_test.go) demonstrates each factory. `RunMetadata`, `RunPacks`, `RunCache` invoke only their storage port plus explicit **fixture control hooks**. Supply a fresh isolated namespace/database per subtest and clean it with `t.Cleanup`.

For BadgerDB/DynamoDB adapters, seed equivalent authoritative records, provide test-only changes for revoke/recreation/fences/quota, and inject failures immediately before the transaction, after a successful commit acknowledgement is lost, and while the authoritative operation-outcome lookup is unavailable. No production method may bypass auth/fencing just to satisfy a test. For S3 adapters, use a controlled fake for deterministic stream/restore faults; real S3 restore/retention tests require genuine backend completion and separately recorded Cloud/Edge evidence. Do not turn a fixture completion into a claim that AWS restored data.

The in-memory `fake.Metadata` uses one mutex as its linearization point. Its JSON copy/page-size checks are a reference fixture representation, not a DynamoDB serializer or evidence that real attribute names/encoding fit400KiB. Real adapters must account for actual item/transaction limits and run the same behavioral suite. The suite does not prove fault-durable storage across process restarts.

```bash
go test ./internal/storage/...
go test -race ./internal/storage/...
make check
```

CI's existing `check` job runs the full Go checks and a focused race run for storage contracts. No AWS credentials, apply, gateway runtime or production benchmark is needed for this W1 deliverable.

## Follow-up ownership and review limits

| Follow-up | Existing task boundary |
|---|---|
| Binary pack/footer/sidecar and page/golden encodings | W1 pack/manifest format task (historical W01-HAI-02); W02-HAI-02/03 |
| Chunking/zstd thresholds, dedup/revival flow, detailed commit producer/event encoding | W1 chunking/commit task (historical W01-HAI-03); W02-HAI-01 and later engine tasks |
| BadgerDB, DynamoDB and S3 adapters, request handlers and API fixture integration | W02-HAI-02/03, W04-HAI-01/02 and Chiến's API/auth tasks |
| R1 coordinator, read/pin/maintenance protocol; R2 export/seal/checkpoint/recovery | W01-PHONG-02/03 and their implementation tasks |
| HTTP LIST token codec/XML, complete policy merge/validation/evaluation, grant management and jobs | Existing API/config/policy/metadata tasks implement on these shared ports; the six required §18 ports and their reference behaviors are already included in W1 |

Source documents, client fixtures, approved R1–R4 decisions and Cloud/Edge behavior remain authoritative. Extending these core ports must preserve the tested invariants and reference their source sections. Passing this suite on fakes is handoff evidence, not proof that either production adapter, full multipart, R1/R2 recovery or the gateway is Done.
