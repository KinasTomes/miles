# W03-HAI-01 write preparation validation

The `internal/engine/write` package composes the Sprint 2 chunker, compressor,
pack format, physical PackStore, canonical metadata and streaming manifest
pages. `PrepareObject` returns a descriptor; it never publishes an object head.
The caller must validate the complete logical request and invoke the existing
fenced `CommitObject` separately.

## Publication order

1. Admit a writer and reserve quota using stable operation identity.
2. Stream FastCDC chunks; make complete consistent lookup batches of at most
   100 keys and 32 MiB raw bytes. Missing rows in a successful response mean
   absent; unavailable/throttled/incomplete responses are not absence.
3. Protect exact canonical locations before reuse. Encode only new/revived
   chunks. Build immutable pack and byte-identical sidecar.
4. Register `Packs[pack_id,generation]` as `Uploading` under writer fence. Write
   pack, write sidecar, verify physical identity/size and sidecar where the
   adapter exposes readback, then CAS pack metadata to `Warm`.
5. Insert or CAS canonical chunk locations. A duplicate-race loser uses the
   winner only after protection, or tries location CAS. Its physical bytes
   remain an orphan candidate. Unknown mutation outcomes fail closed unless
   the intended location can be confirmed authoritatively.
6. Emit ordered refs; write greedy immutable manifest pages and read each page
   back. Only a complete descriptor can be passed to `CommitObject`.

The Badger and fake adapters reject canonical destinations unless the pack is
`Warm`. Register/mark mutations are fenced and revisioned. A sidecar failure
leaves `Uploading` and cannot publish a canonical row through this writer.
PackStore's immutable PUT contract and generation-specific keys prevent a
different retry body from replacing a completed pack.

## Reproduce

Use a writable temporary directory for Badger on Windows. The default
sandboxed Windows temp directory can reject Badger's `MANIFEST-REWRITE`
rename even when the code is correct.

```powershell
$env:TEMP='C:\UET\ki1_2627\miles\.cache\test-tmp'
$env:TMP=$env:TEMP
.\.cache\go1.27.2\go\bin\go.exe test ./internal/engine/write ./internal/manifest ./internal/metadata/badger ./internal/storage/...
.\.cache\go1.27.2\go\bin\go.exe vet ./...
```

`TestPipelineBatchesReusesAndKeepsDomainsSeparate` covers same-domain reuse,
cross-domain isolation and ordered refs. `TestPipelineUniqueAndLowCompression`
covers unique raw fallback. `TestPipelineRetriesUnavailableLookupWithoutAssumingAbsence`
checks bounded retry. Publisher tests cover incomplete sidecar, stale fence,
duplicate insert loser and location CAS. `TestPackLifecycleFencesAndWarmAdmission`
uses durable Badger. `TestPrepareObjectPublishesOnlyAfterPhysicalDependencies`
commits only after preparation and reconstructs the object from canonical
Warm chunks.

## Integration boundary

`ReuseGuard.Pin` must be backed by R1 protection of the exact location and
revision through head publication. This package deliberately has no
lookup-only fallback. The tests supply a guard in an isolated one-node fixture;
it is not proof of distributed R1 admission, GC safety or tiering concurrency.
The existing writer fence fixture is not a production R1 coordinator.

The S3 router and runtime are not wired to this writer in W03-HAI-01. Logical
payload validation, quota reservation, ETag/checksum, final head transaction
and API error mapping belong to the adjacent API/HAI-02 integration work.
Current chunk insert/location CAS ports also do not emit the full R2 child
mutation event/outcome records described in the recovery contract. That
extension is required before claiming distributed recovery or multi-node
publication. A failing preparation leaves physical chunks/pages as orphans;
GC must wait for R1/R2 root and outcome reconciliation rather than deleting
them immediately.
