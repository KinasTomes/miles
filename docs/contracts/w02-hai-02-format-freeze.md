# W02-HAI-02 — serialization review / freeze record

Date: 2026-10-21. Base: `origin/main` at `2227fe3` (W02-HAI-01 merged).
Status: **pack/footer/sidecar v1 candidate ready for independent review**; not a
claim that the team has approved a global freeze or that live AWS integration
has passed. No binary golden was regenerated from the production writer.

## Freeze boundary and ownership

| Artifact | Current evidence | Freeze gate |
| --- | --- | --- |
| Pack header, blobs, footer index, 24-byte trailer, sidecar | Production writer/parser/range reader in `internal/pack`; `pack.json` v1 bytes/digests and corrupt stages pass; AWS SDK v2 physical adapter with controlled tests | Chiến (format), Hưng (shared location/CRC), Phong (read/R1) review; live S3 evidence when dev bucket exists |
| Manifest/part pages (kinds 1–2) | `pages.json` v1 golden/reference test only | W02-HAI-03 production codec, Badger item/head fields and page-size tests; independent review |
| Completion input/snapshot pages (kinds 3–4) | `pages.json` v1 golden/reference test only | Multipart producer/frozen snapshot and production codec; Chiến/Hải review |
| R2 event, membership/seal, checkpoint/catalog/marker | Logical R2 decisions in `r2-recovery.md`; no complete field schemas or production byte goldens | Phong plus format/metadata/auth consumers approve exact schemas, caps and negative goldens; these are **not** `MILESPAG` kinds 1–4 |

The fixture files `pack.json` and `pages.json` explicitly pin
`format_version: 1`. Pack bytes and expected SHA-256 are unchanged. Unknown
binary versions fail closed. After team approval, changes to v1 bytes require
new version, compatibility/migration review, and continued v1 reader tests.

## Physical invariants verified by code and tests

- Pack identity is `(pack_id, generation)` in header, index, S3 key, location
  and cache. Key prefixes are `packs/` and `packs-index/`; no per-chunk S3
  object is written. `NewRef` creates two independent random 128-bit IDs.
- Builder copies bounded stored chunks, signals close at 48 MiB target, 64 MiB hard
  object cap or 16,384 entries. The final request/part pack may be smaller.
  Index is at most 2 MiB; no empty pack, duplicate `(domain, hash)`, gaps or
  overlaps. Sidecar bytes equal the footer index exactly.
- Footer SHA-256, entry CRC-32C of stored bytes, bounded zstd decode, raw length
  and raw SHA-256 are checked before returning client bytes. One zstd frame is
  required; dictionary, concatenation and trailing bytes are rejected.
- `ChunkLocation.CRC32C` is the selected resolution of the GET contract gap:
  canonical metadata carries the footer entry's stored-blob CRC so normal GET
  needs only the chunk range, not footer I/O. Badger/DynamoDB serializers and
  Phong's read path must persist/use this field. A missing/incorrect CRC fails
  verification; no legacy production data is asserted to exist.
- `s3.Store.ReadVerifiedSidecar` is the selected narrow inventory extension:
  it reads and validates a STANDARD sidecar independently of a Cold pack,
  without widening the shared `PackStore` interface prematurely. It does not
  identify the canonical winner, refcount, namespace, state, or recovery cut.
- Pack and sidecar PUTs use S3 conditional create. A same-key retry is accepted
  only after bounded full-byte comparison; a different body conflicts.
  Ambiguous responses remain `ErrUnavailable` until retry/reconciliation.
  Physical class/restore/delete methods are not R1 authorization, retention or
  GC decisions. The adapter's tests use a controlled SDK fake, not AWS Glacier.

## Evidence and remaining integration gates

Run `go test ./internal/pack ./internal/manifest ./internal/storage/...` and
`go test ./...`, `go vet ./...`, plus the existing project checks. The pack
suite verifies small/raw+zstd, mixed-domain, >64 KiB index, 16,384 entries,
invalid and corrupt stages; the page suite verifies manifest/part/completion
goldens including size/page boundaries and negative cases. Controlled adapter
tests cover keys, immutable retry/lost ACK, exact RangeGet, sidecar STANDARD,
generation isolation and Cold/restore acceptance. Local tests do **not** prove
S3 IAM/KMS, real restore completion/expiry, versioning behavior, or live AWS
conditional-write races.

Before declaring W02-HAI-02 globally Done/frozen, record the reviewers and
live S3 evidence or an explicit team-approved waiver. Do not label pages or R2
event/checkpoint formats production-frozen merely because their W1 reference
fixtures pass. W03 write orchestration still owns `Uploading → Warm`, writer
fences, canonical location CAS, and orphan reconciliation.
