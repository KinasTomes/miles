# Development metadata validation

Task: W02-HAI-03. Backend: `internal/metadata/badger`, schema 1, Badger v4.9.6.

## Checklist evidence

| Checklist | Implementation and evidence |
| --- | --- |
| Durable models, location CAS, pages, atomic dev commit | Per-record transactions; frozen binary pages; foundation reopen/rollback tests; shared canonical insert and location CAS contention tests |
| Quota/outbox/receipt contracts | Durable reservation/outcome/index records; same-transaction publication and completion; contract tests for authorization, quota overwrite/delete, claim/release and completion fences |
| Acceptance and restart safety | Full MetadataStore suite on Badger; orphan restart, lost ACK then overwrite/reopen, unclean subprocess exit, page-size/index/greedy checks and corruption rejection |

## Reproduce

```sh
go test ./...
go vet ./...
go build ./...
go test ./internal/metadata/badger ./internal/manifest -cover
go test ./internal/manifest -run '^$' -fuzz '^FuzzPageDecode$' -fuzztime=10s -parallel=2
go test -race ./internal/metadata/... ./internal/manifest ./internal/storage/...
python3 -B -m unittest discover -s scripts/tests -v
python3 -B scripts/check_doc_names.py
```

Windows validation used the repository's cached Go 1.27.2. Badger's file
rename/mmap operations required running tests outside the restricted Windows
sandbox; no remote database, S3 credentials or cloud resources were used.
The local race invocation cannot execute without CGO/compiler support. The CI
Linux race job now includes metadata and manifest; this document does not claim
that a CI run has already passed.

## Important cases

- `TestMetadataContract`: reusable storage acceptance suite, including concurrent
  insert/CAS/head publication, permission revocation, quota and completion rules,
  LIST pagination, monotonic access statistics and revisioned policy updates.
- `TestDurableFoundation` and `TestAtomicAbort`: all durable publication records
  survive reopen; abort before commit leaves no head/outbox/quota publication.
- `TestLostACKAndOverwriteSurviveReopen`: original operation retry returns its
  original outcome without overwriting a newer head or charging quota again.
- `TestUncleanExitRecovery`: child process commits completion then exits without
  closing Badger; parent reopens and verifies head, pages, outbox, receipt,
  counters and reservation outcome. This is process-crash testing, not a claim
  about power failure or arbitrary disk corruption.
- `TestOrphanSurvivesRestartWithoutHead`: pages and reservation survive, while
  neither head nor event becomes visible before successful publication.
- `TestManifestPublicationValidation`: bad size/descriptor, missing chunk, page
  gap, non-greedy split and domain mismatch cannot publish.
- `TestMultiPageCommitAndIndexedRead`: greedy page split, head page count/chunk
  count/index and bounded page lookup agree.
- `TestManifestSealedAndEmptyObject`: immutable retry allowed, append after
  publication forbidden; zero-byte objects need no synthetic empty page.
- `TestOversizedCommitDoesNotPartiallyPublish`: deliberately reduced real Badger
  transaction budget produces `ErrTxnTooBig`; no partial publication survives,
  and a corrected retry works. Large values alone are not sufficient to trigger
  this condition because Badger can store them in its value log.
- `TestCorruptChunkRequiredCRCAndSchema`: omitted/null CRC is not valid zero;
  unsupported record schema fails closed.
- Manifest golden fixtures run through the production object/part codec without
  changing expected version, bytes or digest. Completion stream codecs remain
  reference-only, as in the existing multipart roadmap.

## Handoff and scope

See [the backend contract](../contracts/dev-metadata.md) for lifecycle, schema,
transaction boundaries, retry semantics and member integration points. No new
metadata admin/lease/pack registration port is invented in this task. Application
runtime wiring, R1/R2 workers, outbox delivery, GC and multipart upload creation
remain explicit downstream work. Tests use internal-only fixture controls; those
controls are not exported as production mutation APIs.
