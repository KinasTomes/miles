# Durable development metadata

Implementation: W02-HAI-03, `internal/metadata/badger`. This adapter implements
the existing `storage.MetadataStore` port on one local Badger database. It does
not import the reference fake and does not serialize a whole in-memory store.

## Lifecycle and durability

Open with `badger.Open(badger.Options{Path: absoluteDataDirectory})`, initialize a
new development database with `Store.Bootstrap`, and close it after all users of
the adapter have stopped. Bootstrap is atomic and one-shot, not an administrative
upsert API. Reopening must not reseed or replace existing authoritative records.
`Now` is injectable for tests; persisted deadlines are wall-clock UTC timestamps,
not serialized process-local monotonic instants.

Badger uses synchronous writes and conflict detection. Its directory lock permits
one process. A local writer gate serializes mutations; Badger transactions, not
the gate, own durable state and rollback. Cancellation is checked before commit.
An I/O error or lost acknowledgement is `ErrUnavailable`, never proof of absence.
Transaction-too-large fails the entire publication; it is never split into
separately visible head/quota/outbox writes. The filesystem must itself provide
the durability promised by synchronous writes.

## Local record schema

The database schema marker is 1. Non-page values use a JSON envelope
`{"Schema":1,"Value":...}` containing the shared typed model. This is an
adapter-local representation, not the frozen R2 export/JCS format. Table/key
components are separated and hex encoded; object keys cannot escape namespaces.
Unknown schema versions fail closed. Future schema changes require migration.

| Namespace | Identity | Durable information |
| --- | --- | --- |
| buckets/users/credentials/grants | Name / ID / bucket+user | Active state, rights, ownership, domain and revision |
| chunks | Domain+hash | Raw size, canonical pack+generation+range, codec, CRC32C, location revision |
| packs/writers | Pack+generation / lease ID | Physical state/protection/access fields; current lease and incarnation/epoch |
| heads | Bucket+incarnation+key | Complete head or tombstone, monotonic revision, operation ID, manifest descriptor |
| pages | Manifest ID+page number | Exact `MILESPAG` binary body, not JSON |
| part-pages | Part manifest ID+page number | Exact kind-2 `MILESPAG`; optional PartPageStore stages pages without publishing a part head |
| manifest-seals | Manifest ID | Publishing operation ID and page count; forbids appending pages or reusing the manifest for another publication |
| quotas | Owner | Limit, Used, Reserved, Staged, ActiveUploads |
| reservations | Reservation ID | Operation/object/owner/upload binding, base size/revision, bytes, staged bytes, state, deadline, writer, revision |
| reservation-ops | Operation ID | Unique reservation identity, including terminal records |
| events/event-ops | Event ID / operation ID | Durable outbox envelope and outcome reconciliation index |
| outcomes | Operation ID | Immutable input SHA-256 and original committed head |
| uploads/receipts | Upload ID | Completion CAS state; durable completed receipt |
| policies/policy-current | Revision / singleton | Immutable policy history and current pointer |

CRC32C zero is valid. Location CAS changes the whole location and increments its
revision, conditioned on the previous revision and live writer fence. Lookup is
authoritative in Badger; neither an LRU nor a Bloom filter proves existence.

New canonical inserts and location CAS additionally require the destination pack
to be exactly `Warm`, checked inside the mutation transaction. Every other state,
including empty/unknown states, returns `ErrConflict`; an invalid writer still
returns `ErrFenced`. Missing packs return `ErrNotFound`. The fake adapter follows
the same contract. An existing insert winner is returned before destination
admission, even when its pack is Cold, so the engine can classify reuse/revival.
This guard does not replace R1 pin/protection or require every reused chunk to be
Warm at object commit time.

## Publication and retry

Manifest pages are individually immutable and idempotent. A batch can partially
succeed; retry or verify every page before publication. Commit scans pages in one
snapshot, validates domain, page sequence, logical offsets, referenced chunk raw
sizes and total object size, then publishes the head. Missing pages/chunks or bad
descriptors prevent publication; already staged pages remain unreferenced orphans.
No physical pack/S3 calls happen in a metadata transaction.

Use `manifest.Paginate` for greedy streaming construction. Every non-final page
must be full, and commit rejects non-greedy splits. A successful publication seals
the manifest in the same transaction (including an empty manifest). Identical
page retries remain legal, but appending another page or reusing the owner ID for
another publication conflicts. Fresh owner IDs are required for overwrite/copy.

`ObjectHead.PageCount`, `ChunkCount`, and `PageIndex` are derived and persisted by
this adapter. An entirely omitted/zero descriptor requests derivation for existing
callers; a supplied descriptor must exactly match the pages. Empty objects have
zero pages, an empty index, and a manifest ID. Tombstones have no manifest. Use
`GetManifestPage` with the head index for bounded-memory reads; the legacy
`GetManifest` method materializes the whole manifest by contract.

Within the same write transaction, publication rechecks active credential/user,
current bucket incarnation/domain/grant, writer lease, expected head revision,
reservation binding/revision/deadline, quota, and optional completion CAS. It then
writes head, quota counters, terminal reservation, outbox, operation outcome, and
optional completed upload/receipt together. Quota accounts for replacement:
`Used = previous Used - previous head size + new size`; reserved/staged counters
are consumed once. Delete publishes a new tombstone revision, never physical GC.

The outcome digest covers the immutable commit request. Writer fence,
reservation revision and completion takeover conditions are excluded, allowing
renewal/takeover without changing logical input. Retry the same operation and
input; changed input conflicts. A retry returns the original head even after a
later overwrite. Current authorization/fencing still applies on retry.

Outbox fields preserve Version, ID, HistoryID, Epoch, Entity/Action/EntityID, Object,
Revision, OpID, PayloadPointer and PayloadSHA256. The adapter validates the
envelope, not the external payload. The engine must durably prepare that payload
before publication; a future worker exports/acknowledges it. No fake dispatch or
acknowledgement is performed here.

Receipts preserve upload/bucket/key/initiator binding, operation/input digest,
manifest/ETag/checksum, version and expiry. Publication checks the matching
Completing revision/fence, consumes ActiveUploads once and changes it to
Completed. Expiry is stored, not a Badger TTL: deleting evidence requires the
future recovery/retention protocol, not automatic local expiration.

Reservation claim/release first resolves the outcome inside the transaction.
Unknown/inconsistent outcome fails closed. An active old writer prevents takeover;
a different service incarnation cannot claim. A committed operation cannot have
its quota released. Repeated release is idempotent.

## Integration boundaries

- Chiến: stable metadata port for authorization, LIST, multipart completion and
  receipt retries. Credential secret storage and S3 handlers are separate tasks.
- Hải: use this adapter for engine canonical CAS and write publication; register
  physical packs through PackLifecycleStore, not test hooks.
- Phong: head index and page reads support read/cache work; pack access timestamps
  are monotonic. This is not full R1 admission/closure or distributed recovery.
- Hưng: lifecycle ownership belongs to application wiring. The existing `dev`
  health fixture remains explicitly volatile; it must not be presented as a
  production-ready durable service merely because this backend is implemented.

Worker loops, durable R1 admissions, recovery roots/epoch closure, outbox draining,
GC, multipart upload creation and DynamoDB implementation remain later tasks.

W03-HAI-03 adds the [storage handoff](storage-handoff.md), a distinct part-page
staging capability and [DynamoDB preparation](dynamodb-adapter.md). Part pages
survive reopen and validate kind/owner/number/checksum; they do not publish part
heads or freeze uploads. Native item/request bounds are preparation for W4, not
a distributed backend implementation.
