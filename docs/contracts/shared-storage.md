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
- `PackRef` includes generation. Cache keys also include exact byte range. A class change preserves content generation. Range validation rejects zero/negative/out-of-bounds/overflowing ranges; empty objects need no physical range read.
- Returned maps/slices and submitted mutable values must not alias backend storage. Cache input/output byte slices are owned copies. Authorization precedes chunk lookup and **every cache read**, including a hit; physical cache ports do not grant user access.
- `Authorize` is a fresh authoritative rights projection. Owner receives read/write; a grant can give write without read; system admin receives no implicit object access. Caller-side snapshot age/node binding remains API §2.2.3 (at most5 seconds, no hit-based refresh); this port adds no authorization cache. Commit reads current authorization again.

## Atomic mutation boundary

| Operation | Required behavior |
|---|---|
| Chunk insert/location CAS | Atomically check the admitted writer's current incarnation/lease/fence and resource admission; insert one winner or compare the expected location revision. Reject stale workers. Physical pack/sidecar durability is a prerequisite. |
| WriteManifestPages | Each page is immutable and an identical write is retryable. **No all-or-nothing batch guarantee**: DynamoDB batch requests may leave some pages durable. Retry unprocessed items, verify completeness and serialized limits before commit. |
| Reserve/ReleaseLogicalQuota | Atomically update owner counters with the reservation ledger; retry does not count/release twice. Logical bytes belong to bucket owner, not caller or dedup ratio. A stale writer cannot release a reservation whose outcome is unresolved. |
| CommitObject/DeleteObject | One backend transaction checks current credential/user/write grant, bucket incarnation/domain/state, head revision, writer fence, reservation and optional frozen completion state. It updates head/tombstone, logical quota and ledger, outbox, and Complete state/receipt together or not at all. |

The commit derives overwrite delta from the **current CAS-matched head**, consumes only its reservation and includes other outstanding reservations in the limit check. Same-size overwrite can use zero extra reservation at full quota; staged bytes remain independently bounded by owner limit +100GiB. Complete consumes staged/reserved bytes and the active-upload counter with its receipt. This is the final commit boundary, not a full multipart reservation/grow/takeover implementation.

Outbox includes history/epoch, entity/object incarnation, resulting revision, op_id and immutable payload pointer/checksum. Receipt binds upload/bucket/key/initiator, normalized input digest, manifest, ETag/checksum and its own expiry. Neither is an opaque “success flag” unrelated to the published head. The producer must durably prepare and verify referenced payload/pages before the transaction. Transactions remain bounded; a real DynamoDB implementation must combine conditions with an Update on the same item rather than issuing both actions for it.

`WriterFence` is a typed proof handle, **not a lock implementation or permission supplied by an HTTP client**. R1's coordinator owns shared-gate admission, writer registration, lease renewal, read views/location pins, retirement and takeover. Mutations must validate their conditions in the same authoritative transaction. This PR tests rejection of expired leases/old fencing epochs/incarnations; it does not claim GC/read-pin safety or a distributed coordinator is implemented.

`ErrUnavailable` can mean an outcome is unknown. Resolve using the same stable op_id and retained outbox/reservation/receipt state before cleanup. An immediate identical retry returns the original result without applying quota twice; changed logical input conflicts. Writer/completion takeover conditions are not part of immutable operation identity. Real adapters must persist/reconstruct this information from R2 records, not a process-local map. Retention follows the approved recovery/request/receipt windows; the fake keeps operation records for its test lifetime, not a new promise of infinite retention.

## PackStore and cache boundaries

- `PutPack`/`PutSidecar` publish complete bytes or report an error; no partial body may become a valid completed object. Exact-generation identical retries are safe; different bytes conflict. A failed multipart backend upload is orphan work to reconcile, never a published pack.
- `HeadPack`/`RangeGet` address the exact generation. `RangeGet` returns compressed physical bytes; decoding and raw SHA-256 verification before emitting client bytes belong to the engine.
- Pack writes use Warm STANDARD. Sidecars remain STANDARD when packs move to STANDARD_IA, GLACIER_IR or GLACIER. The reference suite checks GLACIER_IR reads immediately and archive restore separately.
- `Restore(Days, Tier)` only accepts/starts the backend operation. Successful submission **does not make the pack readable**. Only backend completion/HEAD supplies actual restore expiry. The fake has an explicit completion hook; tests verify Cold remains unreadable while restore is pending and at the expiry boundary. Request-deadline safety margins still belong to the approved GET/Copy admission logic.
- `ChangeStorageClass`/`DeletePack` cannot fence an already sent S3 request. The caller must first persist Transitioning/Deleting under the R1 maintenance/resource fence, reconcile uncertain backend results and respect pins/roots/protection/minimum retention. These trusted physical methods deliberately cannot be called a complete GC/tiering service. This fake does not simulate provider minimum-storage billing or asynchronous self-COPY reconciliation.
- `Cache.Evict` only affects the specified pack generation. Eviction/cache loss never changes authoritative metadata, rights or logical usage. No LRU/admission/read-ahead engine is added here.

## Reusing the suite

The [reference wiring](../../internal/storage/contracttest/reference_test.go) demonstrates each factory. `RunMetadata`, `RunPacks`, `RunCache` invoke only their storage port plus explicit **fixture control hooks**. Supply a fresh isolated namespace/database per subtest and clean it with `t.Cleanup`.

For BadgerDB/DynamoDB adapters, seed equivalent authoritative records, provide test-only changes for revoke/recreation/fences/quota, and inject failures immediately before the transaction and after a successful commit acknowledgement is lost. No production method may bypass auth/fencing just to satisfy a test. For S3 adapters, use a controlled fake for deterministic stream/restore faults; real S3 restore/retention tests require genuine backend completion and separately recorded Cloud/Edge evidence. Do not turn a fixture completion into a claim that AWS restored data.

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
| Additional architecture §18 queries/control-plane ports (LIST/cursors, policy/grant management, access stats/jobs) | Their owning API/policy/metadata tasks; no stub implementations or second storage abstraction are introduced here |

Source documents, client fixtures, approved R1–R4 decisions and Cloud/Edge behavior remain authoritative. Extending these core ports must preserve the tested invariants and reference their source sections. Passing this suite on fakes is handoff evidence, not proof that either production adapter, full multipart, R1/R2 recovery or the gateway is Done.
