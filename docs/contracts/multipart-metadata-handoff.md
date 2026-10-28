# Multipart metadata and producer handoff (W04-HAI-02)

This is the implemented adapter contract for multipart/API and lease/recovery
consumers. It does not implement multipart HTTP routes, a distributed epoch
coordinator, epoch sealing, restore or a multi-node commit protocol.

## Persistence and limits

Both Badger and native DynamoDB implement `storage.MultipartStore`,
`PublisherProtection`, `MultipartRecovery` and `ProducerStore`. Existing
MetadataStore methods and v1 Completion/CompletedReceipt readers remain available.
DynamoDB adds no tables, indexes or TTL deletion rules to the frozen 20-table schema.

| Record | Native physical identity | Mutation condition |
| --- | --- | --- |
| Upload, compatibility Completion and full receipt | Uploads / upload_id; separate nested projections | One physical CAS, preserving sibling projections |
| Current part | UploadParts / upload_id + numeric part_number | Candidate base part revision + upload state |
| Kind-2 part pages | UploadPartPages / manifest_id + page | Immutable exact bytes |
| Kind-3 input / kind-4 snapshot | CompletionPages / kind/owner + page | Immutable exact bytes and envelope |
| Upload ledger | QuotaReservations / reservation_id | Owner, binding, revision and Active state |
| Candidate | QuotaReservations / multipart/candidate/id | Candidate state, upload and writer |
| Publisher membership and retained packs | Leases / upload/id + candidate id | Written atomically with candidate |
| Completion attempt/outcome | SystemConfig / scoped op id | Original reservation, exact request and Closed marker |
| Producer result/payload | SystemConfig / scoped mutation or content key | Immutable scope/input/result and exact body |
| Authoritative event | Outbox / stable event id | Same transaction as business effects/result |

Uploads contain descriptors, not 10,000 part rows or all chunk references.
Completion streams use frozen kinds 3/4, at most 384 KiB per binary page. Native
transaction validation enforces 400 KiB items, 4 MiB total and 100 distinct targets;
large manifests use protected immutable descriptors rather than per-page final
ConditionChecks. Required provider facts count toward those same caps and may
cause fail-closed rejection. Candidate retained packs are bounded to 1,024 exact
physical generations. ListParts/ListPublishers accept 1..1,000 rows per page.

## API sequence

1. Create with a fresh 32-hex upload/op id and immutable object/bucket incarnation,
   checksum mode and metadata. Backend chooses owner/initiator/domain/24-hour
   expiry and ledger accounting; 16 active uploads is an owner bound.
2. Register publisher BEFORE preparing pages/packs. Attach exact pack generations
   with RetainPublisherPacks while also retaining the managed R1 writer/pins.
   An uncertain registration is retried with the original candidate identity.
   Adding retained packs does not change registration identity.
3. PublishPart verifies immutable pages and canonical locations, then atomically
   CASes candidate/current part/upload/quota. A conflicting replacement requires
   reconciliation and a new registration against the observed part revision.
4. Persist normalized kind-3 input, then FreezeCompletion with observed revision.
   This closes new registration/publication and binds completion op/fence/lease.
   Resolve/cancel outstanding publishers before snapshot and preparation.
5. Persist exact kind-4 snapshot; PrepareCompletion verifies selected part
   revisions and assembly/checksums, and writes a fresh kind-1 object manifest.
   Use the returned prepared head, current ledger revision, Completion projection,
   MultipartAttempt and original completion op for the atomic CommitObject call.
6. The final transaction writes head, quota/outcome/event and full receipt once.
   Selected subsets charge only committed size but consume all upload staging.
   Retry reads original outcome/receipt, never republishes a current head after
   overwrite/delete. Receipt validity is seven days from winning backend commit.

The caller renews completion every 15 seconds within the 60-second lease. Renewal
rechecks authorization. ClaimCompletion consumes the old attempt before takeover,
uses a new fence and preserves frozen input/op; eligibility starts at lease+5s.
If expired/revoked or abandoning an unpublishable attempt, use ResolveCompletion
with current recovery authority, rather than retrying a public Abort. Business
expiry and execution lease have separate clocks; no TTL deletes Completing.

Public Abort rejects Completing. Active with outstanding publishers becomes
Aborting, retains counters and fences admission; it must not return a final HTTP
204 until reconciliation/cleanup finishes. Expired Active returns ErrNotFound;
trusted recovery can close it. Errors map through existing API error handling.

## Recovery and roots

`ResolveCompletion` returns Committed, ProvenClosedNoCommit or Unknown. It closes
the original pending operation in the SAME native CAS as the Aborting transition.
Without an acknowledged transaction, including response loss, the result is
Unknown. A retry observes the persisted terminal state. `ClosedFence` and
`ClosedWriter` identify the abandoned execution; they are not lease timestamps.

Aborting retains all unfinished membership/packs and provisional quota. Enumerate
ListPublishers and call ResolvePartPublisher per candidate: Publish won returns
Committed; cancellation CAS won returns ProvenClosedNoCommit. Cancellation fences
late publication by changing both candidate and upload. Once all are terminal,
read current upload/revision and ResolveCompletion again; counters release once.
Recovery does not require a still-valid original signing credential, but DOES
require current trusted upload authority. Missing provider fails closed.

GC/root consumers must retain, while upload is Active/Completing/Aborting:
current published-part manifests, registered candidate manifests/packs, frozen
input/snapshot and prepared manifest descriptors. ListPublishers deliberately
retains terminal membership and packs; do not filter out Published members just
because their publisher lease expired. Completed/Aborted permit cleanup only
with corresponding publication/closure and remaining recovery/read roots. A
receipt alone does not retain payload.

`coordination.Writer.ReconcileMultipart` and `WriteSession.ReconcileMultipart`
consume the trusted metadata port directly after Close/quiescence. They require
matching object/op/upload/old writer/attempt and Aborting/Aborted state before
clearing ONLY `commit/op` uncertainty. Other unknown work still blocks Release.
Committed final outcomes use existing Reconcile/GetCommitOutcome. Caller booleans,
ErrNotFound and elapsed grace never release local pins.

## Producers and recovery payloads

| Entity | Entry point | Retained effects |
| --- | --- | --- |
| Objects | CommitObject/DeleteObject + admitted Producer context | Head/tombstone, quota/ledger, old/new manifests, multipart receipt/upload |
| Buckets | MutateControl; admitted CreateBucket | Incarnation, owner/domain/status, verified empty namespace on delete |
| Users | MutateControl | Status/default domain, logical quota projection |
| BucketGrants | MutateControl | Bucket incarnation, user rights/revision/tombstone |
| Credentials | MutateControl | Status/admin flags, KMS key and ciphertext; no plaintext secret field |
| AdminTokens | MutateControl | User/scope/status/expiry and secret hash |
| SystemConfig | Admitted policy update | Current global policy/revision |
| Chunks | Admitted insert/location CAS | Domain+hash identity, canonical revision/location and pack projection |

Use RequireProducers for enforced admission. Legacy development mode is explicit
compatibility behavior, not evidence of authoritative journaling. HTTP admission
must provide trusted history/epoch/logical mutation and current writer context.
DynamoDB ProducerAuthority and MultipartAuthority must observe durable R1 facts
through the supplied Observe callback so they join the SAME CAS. Badger supplies
single-process local authority; test providers are not distributed implementations.

Each changed transaction has one stable event and persisted exact result. Child
chunk/location mutation ids retain their logical parent. Id reuse across inputs,
entities, incarnations/history/epochs conflicts; no-op outcomes emit no event.
Heartbeat/pin renewal/export progress do not recursively create business events.

Payload v1 uses typed Projection JSON (Base64 in canonical EventPage effects) and
a ProducerScope binding, with durable immutable dependency descriptors. Binary
manifest/completion pages are exported in <=128 KiB segments with bounded group
indexes, then verified using exported artifacts alone. Canonical metadata supports
valid Unicode/UTF-16 ordering and safe nonnegative integers; arbitrary JSON numeric
formats are not claimed. Full epoch closure/seal/replay acceptance remains with
R2 consumers. See multipart-metadata-validation.md under docs/testing for evidence.
