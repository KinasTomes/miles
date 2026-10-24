# Decoder memory and ownership

Contract: **DECODER-MEMORY-V1**, task W03-HUNG-04. Producer:
`internal/pack`; consumer: W03-PHONG-01. This contract does not change pack v1
bytes, codecs, CRC32C or SHA-256 verification.

## Admission API

Call `pack.EstimateDecodeMemory(entry)` on the validated physical binding before
allocating, cache lookup/copy, fetching or decoding. It uses metadata only,
returns no payload and creates no decoder. Invalid identities, codecs, zero or
oversized lengths and ranges outside the pack limit return an error. The raw
codec requires S = R. R is at most 16 MiB; S at most 64 MiB. Conversion to int64
precedes arithmetic, and these limits keep every sum below int64 on 32/64-bit
hosts. An estimate is neither authorization nor integrity/location proof.

| Field | Reservation meaning |
| --- | --- |
| `InputBytes` | S borrowed bytes; the input owner charges its **actual capacity** once |
| `OutputBytes` | R exclusively owned bytes returned on success, capacity exactly R |
| `WorkspaceBytes` | Additional simultaneously live buffers/state, including failure paths |

Additional decoder reservation is `OutputBytes + WorkspaceBytes`. The caller
also reserves its fetch, backend and cache-copy capacities; input can be a slice
of a larger allocation and `InputBytes` does not bound that allocation. Do not
add S a second time as decoder workspace. LRU residency has a separate budget;
a hit still needs a borrowed-input owner's copy charge and verification.

Raw workspace is 96 bytes (SHA-256 digest and its hex comparison buffer).
Zstd workspace is `5*R + 6,492,274` bytes; additional owned peak is therefore
`6*R + 6,492,274`. At R = 1 MiB this is 12,783,730 bytes; at 16 MiB,
107,155,570 bytes. These deliberately conservative reservations cover hostile
blocks and both execution strategies, rather than estimating ordinary usage.
There is no RAM default or production tuning knob here. A consumer must reject
admission when its configured limits cannot fit an occurrence; it must not
substitute the old fixture's 1 MiB allowance.

This is a bound on live operation-owned buffer capacities and decoder state,
**not RSS**. It excludes unreachable allocations awaiting GC, allocator metadata
and physical rounding not exposed as slice capacity, stack provisioning,
shared regexp/runtime facilities, shared immutable tables and idle library
pools. Scratch checked out of a pool **is included** while owned by this decode.
Deployment needs separate process/GC/pool headroom; increasing concurrent work
is not safe merely because measured RSS happened to be small once.

## Fixed decoder configuration

The audit is for `github.com/klauspost/compress v1.20.3` and Go 1.27.2,
with concurrency 1, explicit lowmem, max memory/window 16 MiB,
`WithDecodeAllCapLimit(true)`, a freshly created decoder per call and no
dictionaries. `oneFrame` rejects skippable, dictionary, concatenated, trailing,
oversized-window and oversized-FCS inputs before decoder construction. Unknown
FCS is supported; admission cannot inspect the frame and uses the worst case.

`ReadBlob` synchronously calls `DecodeAll(stored, make([]byte, 0, R))`, verifies
length/hash, closes the decoder and transfers that destination directly.
Raw uses `make(R)` and copies the borrowed input. Neither success aliases the
input or another decode. Failure returns nil, including after partial decode.
Removing the former final zstd clone does not change content or wire bytes.

`TestDecoderMemoryAuditPin` checks the resolved module (including MVS and
replacements), Go version, options/concurrency and fixed-state size envelopes.
Any dependency/toolchain/options, pooling, buffer or ownership change requires
renewing this audit and regression evidence before shipping the consumer. A
failing audit gate must not be bypassed by assigning an arbitrary workspace
allowance. If a changed build has no proved zstd bound, reject its production
zstd admission before decode; raw/prototype/R1 work can continue. Zstd remains
part of final W3 reader acceptance.

## Source-derived envelopes

The following is structural reasoning for live capacities, not a conclusion
from sampled allocations. B = 131,072, N = 98,047, P = 8,192.

| Component | Bound | Source invariant |
| --- | ---: | --- |
| Output growth/temporary overlap, additional to R | `5R + 9B + 2P` | At most one offending block; original output, previous/new grown output and a zero-fill temporary can overlap |
| Block destination replacement | `2*(B+17)` | `blockDec.reset` allocates at most B+16+1; raw/RLE and saved block destination are included |
| Literal buffer replacement | `2*(B+16)` | `decodeLiterals` rejects regenerated length > B before allocation; Huffman destination is capped to regenerated length |
| Sequence array replacement | `2*N*3*8` | Wire count is at most 0x7f00+0xffff; `seqVals` is three ints, at most 24 bytes, including arm64 two-pass |
| Entropy tables and decoder state | `65,536` | Three FSE state envelopes of 8 KiB, Huffman envelope 32 KiB, core state envelope 8 KiB |
| Verification digest/hex buffer | `96` | 32-byte digest + 64-byte encoding |

`DecodeAll` uses caller output as history. The input `byteBuf` borrows block
bytes; neither `dataStorage` nor a streaming window ring is allocated. Fresh
single-frame decoding excludes accumulated previous frames/windows and async
worker buffers. Do not apply this estimate to streaming `Decoder.Read` or the
engine's independent compression helpers.

### Output failure paths

The cap option does not prevent **every transient allocation**. `runDecoder`
checks output length after each block; raw, RLE and literal-only blocks can
append before that check. No-FCS inputs therefore need a failure-path budget.
Before the first offending block, output length is at most R. A block emits at
most B sequence bytes plus B trailing literals before the generic error check;
we use 2B even where an earlier check is tighter.

For generic synchronous decode, every growing append requests length at most
R+2B. In the pinned Go byte-slice growth implementation, the terminal growth
step is below `1.25*requested + 192` (or small-slice doubling), followed by size
class/page rounding of less than P. Thus each grown buffer fits
`G = 2*(R+2B)+P`. Count the original R, two overlapping G buffers, and a
zero-fill temporary bounded by R+B: total at most `6R+9B+2P`.

The arm64 two-pass path can append a zero-fill temporary of L+sequenceSize,
with L <= R and sequenceSize <= B. Its requested new slice length is at most
2R+B. When above twice old capacity Go uses the requested size; otherwise the
terminal growth step is below 2.5R+192, plus page rounding. Original R, grown
buffer and temporary total less than `4.5R+2B+P`, covered by the same envelope.
The sequence count can be maximal even on invalid input before output is
rejected, so its array is charged independently of R and frame validity.

The post-block check stops before any next offending block; old intermediate
buffers without references are garbage, separately excluded above. The
successful output remains exactly R, and no final clone overlaps it.

### Fixed state and entropy tables

`fseDecoder` has a fixed 512-entry uint64 symbol table, a 256-entry uint16
state table and a 256-entry int16 normalization table; each fits 8 KiB.
Huffman scratch is decoder-private to zstd's pool: no arbitrary user compression
scratch can enter it. Its fixed structs, 513 uint64 nodes, 2,048 uint16 decoding
entries, 256 four-byte compression entries, FSE weights scratch/table (tablelog
<= 6), 256-byte/uint16 helper tables and 1,024-byte decode buffer fit 32 KiB.
The weights output borrows the 256-byte weight array; malformed weights can
transiently grow a small output, also within this envelope. Decoder, two frame
structs, options, bit-reader, checksum objects and one-slot channel fit the
remaining 8 KiB. Fixed-state type sizes are checked in the audit test; idle
pooled objects and GC-retained old buffers are not per-call ownership.

Pinned primary sources:

- [Decoder construction, DecodeAll and Close](https://github.com/klauspost/compress/blob/v1.20.3/zstd/decoder.go)
- [Options](https://github.com/klauspost/compress/blob/v1.20.3/zstd/decoder_options.go)
- [Frame runDecoder](https://github.com/klauspost/compress/blob/v1.20.3/zstd/framedec.go)
- [Blocks, literals, sequence allocation](https://github.com/klauspost/compress/blob/v1.20.3/zstd/blockdec.go)
- [Generic sequence decoding](https://github.com/klauspost/compress/blob/v1.20.3/zstd/seqdec.go)
- [Assembly wrappers/two-pass execution](https://github.com/klauspost/compress/blob/v1.20.3/zstd/seqdec_asm.go)
- [FSE state](https://github.com/klauspost/compress/blob/v1.20.3/zstd/fse_decoder.go)
- [Huffman scratch](https://github.com/klauspost/compress/blob/v1.20.3/huff0/huff0.go), [tables/decode](https://github.com/klauspost/compress/blob/v1.20.3/huff0/decompress.go)
- [FSE weights scratch](https://github.com/klauspost/compress/blob/v1.20.3/fse/fse.go), [weights decode](https://github.com/klauspost/compress/blob/v1.20.3/fse/decompress.go)
- [Go slice growth](https://github.com/golang/go/blob/go1.27.2/src/runtime/slice.go), [size rounding](https://github.com/golang/go/blob/go1.27.2/src/runtime/msize.go)

## Lifetime and handoff

Reserve all occurrence dimensions at node, bucket and request before side
effects. Keep the whole reservation through synchronous decode/Close, worker
quiescence and emitter return. Cancel is a signal; it does not stop ReadBlob
mid-call. Do not detach a decoder goroutine, release accounting on context
cancellation, or release an R1 exact-generation pin while work still uses it.
The emitter borrows output only until return; a consumer retaining it needs its
own ownership/reservation. This task supplies no protected reader or R1 lease.

Map `OutputBytes` to the reader's raw budget and `WorkspaceBytes` to workspace;
add independently audited fetch/cache copies to their own dimensions. The
transport/cache fixture's copy charges are not promoted to a production bound.
Use the same estimator on cache hit and miss; a cheap hit cannot bypass decode
accounting, verification or authorization.

Validation and consumer snapshot checks are recorded in
[decoder-memory-validation.md](../testing/decoder-memory-validation.md).
