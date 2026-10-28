# Multipart metadata and producer validation

W04-HAI-02 implementation starts from main `ca6d6ef` on
`codex/w04-hai-02-multipart-producers`. No AWS provisioning is performed.

## Checklist 1: persistent multipart primitives

Additive typed records preserve existing v1 Completion/Receipt readers. Badger
uses synchronous local transactions; DynamoDB uses native attributes and CAS,
the same frozen 20 tables, and one action per physical target. New upload/full
receipt projections share the Upload item without overwriting compatibility
fields. Empty/prepared nested heads have explicit tagged native encodings;
published root heads still require a valid committed descriptor.

Production kind-3 input and kind-4 snapshot codecs match frozen bytes, including
the 10,000-part input/snapshot goldens, bounded page streaming, digest checking,
cross-record byte boundaries and corruption rejection. HTTP-independent input/
checksum assembly is shared by metadata and the existing multipart HTTP helpers.

Native transaction operations cover Create, candidate registration/publication/
CAS cancellation, bounded ListParts, freeze, verified snapshot and fresh object
manifest preparation, completion renewal/takeover/reopen/abort and receipt reads.
Final publication uses the existing atomic head/quota/outbox/outcome/receipt
transaction with a full-upload guard; selecting a subset of uploaded parts
consumes all staging once but charges only the committed object size. Backend
chosen full receipt timestamps are persisted in the winning transaction.

The new MultipartAttempt marker excludes generated receipt expiry from semantic
input binding while old v1 request digest bytes remain unchanged. A serialized
multipart reset consumes the closed attempt and advances upload/fence/operation
CAS; it does not reopen ordinary HAI-01 PUT/Delete Closed operations.

Executed on 2026-10-28, Go 1.27.2, required native DynamoDB Local:

```text
go test ./internal/metadata/... ./internal/storage/... ./internal/manifest ./internal/multipart/... -count=1 -timeout=10m
Badger PASS 13.139s; DynamoDB PASS 27.768s; storage/contracttest PASS;
storage/s3 PASS; frozen manifest PASS; multipart/core and HTTP compatibility PASS.
```

Shared multipart tests cover subset Complete, candidate cancellation, proven
reopen under the same logical completion ID, atomic full receipt/counters,
idempotent completion, receipt after Active expiry and exact receipt-expiry
equality. Remaining producer/protection integration and fault coverage are
tracked separately below; this evidence does not claim distributed R1/R2 safety.

## Checklist 2: authoritative producers and immutable payloads

Typed ProducerStore mutations cover Users, Buckets, BucketGrants, Credentials
(ciphertext only), AdminTokens, global policy, canonical chunk/location CAS and
object head/quota/receipt transactions. Stable child mutations retain the original
logical operation identity. Exact retries return the persisted original result;
no-ops persist terminal outcomes without adding events. Strict RequireProducers
mode rejects writes without admission. Native ProducerAuthority observations are
included in the same CAS; fixtures provide local test authority, not R1 consensus.

Object payloads include immutable manifest/completion dependencies. Export splits
large binary pages into bounded artifacts, then independently verifies their
hashes, order, envelope and stream digest. Unicode is preserved with UTF-16 member
ordering and safe integer validation. Shared tests export all control entities,
chunk CAS and a two-page object with a page larger than 256 KiB; deleting an
exported binary segment is detected. Bucket deletion requires a verified empty
namespace counter, never an absent counter.

Executed 2026-10-28 with required DynamoDB Local:

```text
go test ./internal/recovery/... ./internal/metadata/... ./internal/object/... ./internal/storage/... -count=1 -timeout=4m
recovery PASS 7.332s; format PASS; Badger PASS 18.636s;
DynamoDB PASS 63.621s; object PASS; storage/shared/S3 PASS.
```

Strict admission is opt-in for compatibility. Production integration must supply
trusted writer/producer/multipart authority; enabling the option is not a claim
that a distributed R1/R2 coordinator has been implemented. Heartbeat Nodes are
operational telemetry and remain outside authoritative recovery producer coverage.
