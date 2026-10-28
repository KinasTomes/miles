# Object P0 handlers and service validation

Task: W03-HUNG-05 — PUT/GET/HEAD/Range P0 và checksum (source task W03-CHIEN-02).

## Scope

`internal/api/s3/object.go` owns the HTTP operations: decoded length,
HTTP metadata, Range grammar, ETag/Last-Modified/Content-Range/checksum
response headers, `If-Match`, and bodyless HEAD. `internal/object` owns the
stateful side: quota reservation, the managed write session
(`engine/write`), head publication, and the Warm read path (`readpath`).

The Router map in `cmd/miles/s3.go` (#38, merged) still wires health and bucket
operations only. Wiring the object handlers additionally needs the shared
outbox producer/journal (W04-HAI-02) and concrete Warm reader/gate adapters
that `app.Dependencies` does not expose yet. The runtime must also supply the
coordinator-backed per-request `Options.AdmitWriter`; test fixture fences are
not a production admission provider.

## Checks that ran

```sh
make check
go test -race -count=1 ./internal/object ./internal/api/s3 ./internal/readpath ./internal/coordination/... ./internal/engine/write/...
```

- Handler tests pin full object, open/closed/suffix/clamped Range, ignored
  Range unit, multi-range and past-object 416, empty object and empty-object
  Range 416, multi-range 416, HEAD bodyless responses (200 with the selected
  length, no Content-Range), checksum mode ENABLED versus absent, Range
  suppressing the full-object checksum, `If-Match` 412, key length, missing
  length and storage error mapping.
- The service integration test runs the production Badger store, the SDK pack
  adapter over an in-memory S3 seam, the LRU, the Warm reader and the managed
  write session. It proves: PUT then HEAD/GET/GET Range returns the committed
  bytes and ETag; Last-Modified and checksum survive; empty objects work;
  overwrite is visible; a body that fails before commit leaves the old object
  and releases the quota reservation.

## Review corrections

| Finding | Behavior after correction | Regression evidence |
|---|---|---|
| Writer lifetime | One Service serves successive/concurrent PUTs; each call obtains a fresh admitted writer bound to object/op/credential/deadline. Protection must cover the request deadline plus 15 minutes. Service does not mint backend authority. A longer parent context cannot extend RequestTimeout. | `TestServiceServesSuccessiveWrites`, `TestServiceServesConcurrentWrites`, `TestServiceRejectsInsufficientWriterProtection`, `TestServiceDeadlineCannotExtendConfiguredBudget` |
| Range consistency | HTTP Range and If-Match are evaluated by SelectRange against the head already bound by the Warm read view, before page planning/payload I/O. No separate GET Stat determines size. Normalized Range and SelectRange are mutually exclusive. | `TestRangeSelectsBoundHeadDuringOverwrite` (growth/shrink, suffix/open/clamped), `TestIfMatchUsesBoundHeadBeforeEmission`, `TestSelectRangeUsesCommittedHeadAfterOverwrite`, `TestWarmSelectRangeRunsUnderBoundReadView`, `TestWarmSelectRangeErrorStopsBeforePayload` |
| Transport encoding | Stored and returned Content-Encoding excludes aws-chunked, case-insensitively; other encodings and their order are retained across multiple header values. | `TestObjectMetadataRemovesTransportEncoding` |
| Producer handoff | prepared.BindHead fills page count, chunk count and page index before EventBuilder receives the candidate. Candidate Head.Revision remains zero; CommitEvent.Revision supplies the resulting publication revision. | `TestEventReceivesPreparedManifestDescriptor` compares the event descriptor/index with the committed head |
| PUT limits | Decoded length follows the same STREAMING-* mode as the payload decoder; a plain PUT cannot bypass limits with x-amz-decoded-content-length. Shared ValidatePut rejects a declared single PUT above 5 GiB or user metadata above 2 KiB before writer admission/reservation. HTTP maps these failures to EntityTooLarge. User metadata counts UTF-8 bytes of full x-amz-meta-* header names and their stored values; system/internal metadata is excluded. No multi-gigabyte body is generated for boundary tests. | `TestPutLimits`, `TestServiceRejectsPutLimitsBeforeAdmission`, `TestObjectPutLimitsBeforeWriter` (exact limits, UTF-8 values, multiple keys, no quota changes) |

These limits retain API v2 §2.5 and the architecture §19.1 single-PUT cap.
The metadata byte-count convention follows the REST header keys described in
[AWS metadata editing](https://docs.aws.amazon.com/AmazonS3/latest/userguide/add-object-metadata.html).
The stored/returned descriptor and selector tests run locally; they are not
proof of a journal exporter, shared root binding or distributed coordinator.

Validation after these corrections: make check and the race command above PASS
on Linux. This includes all Go tests/build/vet, formatting, six Python checks
and document-name validation. No AWS or process-level object endpoint run was
performed. CI must be checked against the updated PR head separately.

## Limits

- No live AWS call and no real S3 endpoint; the pack adapter is exercised
  through its SDK interface only.
- The outbox event is a test fixture built with `storage.EventID` and a fixed
  payload reference. A production producer (journal payload, history/epoch
  source) is W04-HAI-02; without it a real commit would publish an event whose
  payload pointer is not durable.
- `ObjectHead` has no commit timestamp. Until the metadata owner adds one, the
  service stores the commit wall time in the head metadata map under
  `x-dsg-commit-time` and never echoes reserved `x-dsg-*` keys to clients.
- The object service is not wired into the runtime; `cmd/miles` still serves
  only the operations of its own handler map.
- Multipart, GET/HEAD `partNumber`, `GetObjectAttributes`, Delete, List,
  copy and restore remain out of scope (W03-CHIEN-03 and W04 tasks).
