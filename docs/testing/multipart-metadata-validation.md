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
