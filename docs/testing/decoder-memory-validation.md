# Decoder memory validation

Producer task: W03-HUNG-04. Consumer: W03-PHONG-01.
Contract and source proof: [decoder-memory.md](../contracts/decoder-memory.md).

The committed pack tests cover metadata-only/zero-allocation estimates, bad
identities/codecs/overflow ranges, max R/S, exact output capacity and independent
ownership; raw/zstd at the 16 MiB raw/window bound; no-FCS; truncation, trailing
and concatenated frames, dictionary/skippable/oversized window, CRC/SHA and
stored/raw length mismatches. Synthetic raw, RLE and literal-only compressed
blocks exercise allocation **before** rejection of an undersized destination.

The audit pin test resolves the real module version and replacements, checks
Go 1.27.2 and decoder options, and checks fixed-state size envelopes. Allocation
probes are regression evidence, not the mathematical proof of peak live memory.
Raw correctness relies on exact make/capacity and ownership checks; process-wide
allocation counters also include shared metadata/regexp overhead outside the
buffer contract. No test claims a process RSS maximum.

Reproduce producer checks:

```sh
make check
go test -race -count=1 ./internal/pack ./internal/readpath ./internal/engine/write ./internal/storage/...
go test -tags=noasm -count=1 ./internal/pack
go test ./internal/pack -run '^$' -fuzz '^FuzzDecodeMemory$' -fuzztime=15s -parallel=4
```

On 24 October 2026 these checks passed; fuzz ran 480,937 executions without a
failure. The noasm run exercises the generic sequence decoder in addition to
normal amd64 assembly. The bound also accounts for arm64's sequence array from
source; `GOARCH=arm64 go test -c` passed. The same targeted tests compiled for
386, but this host cannot execute that binary (`exec format error`). Neither
cross-build is evidence of runtime behavior on that architecture.

## Consumer handoff regressions

Readpath arrived in main from PR #26 while this task was in progress. The first
CI run exposed two error-stage compatibility regressions in this change;
ReadBlob now keeps physical `blob_checksum` errors before logical `content`
mismatches, and the verifier's existing tests cover that boundary.

The committed `verified_scheduler_test.go` now uses the pack-owned estimator
instead of the old 1 MiB workspace allowance. It reuses the real Builder/pack
verifier, fake physical storage, LRU, RunReadAhead and Admission. The fixture
retains its test transport/cache-copy charges, while raw/workspace come from:

```go
m, err := pack.EstimateDecodeMemory(entry)
// Return err before any fetch/cache/decode on failure.
cost.RawBytes = m.OutputBytes
cost.WorkspaceBytes = m.WorkspaceBytes
```

The existing mixed Range/cache suite and committed
`decoder_memory_handoff_test.go` passed under the race detector:

- Mixed raw/zstd Range output stays ordered across cache miss/hit; hits still
  invoke the production verifier.
- With workspace below the estimated zstd cost in each of node/bucket/request,
  admission rejects before any fetch, verification or emit.
- Cancel while a zstd occurrence is held before decode, after real synchronous
  decode, or inside the emitter retains its full workspace reservation. The
  scheduler waits for worker/emitter release and then drains accounting.

These tests establish compatibility of the producer handoff with the current
scheduler primitives. Cancellation **inside** DecodeAll is not made interruptible
or artificially hooked: its pinned implementation is synchronous, and consumer
must join its worker through return/Close. Actual producer/consumer composition,
complete fetch/cache-copy bounds, concurrency limits and protected R1/S3 tests
remain acceptance of W03-PHONG-01 and its integration tasks. They are not silently
claimed by this producer task or added as backward dependencies to W2.
