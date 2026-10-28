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
that `app.Dependencies` does not expose yet.

## Checks that ran

```sh
go test ./internal/api/s3 ./internal/object
make check
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
