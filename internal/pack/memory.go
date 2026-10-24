package pack

import (
	"crypto/sha256"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/klauspost/compress/zstd"
)

// DecodeMemory bounds live buffer capacities for one synchronous ReadBlob,
// including failure paths. It does not bound process RSS, garbage awaiting GC,
// allocator metadata, goroutine stacks, or idle library pools. See
// docs/contracts/decoder-memory.md for the pinned source audit and lifetime.
//
// InputBytes is borrowed: charge the actual input owner's capacity once, not
// again as decoder workspace. OutputBytes + WorkspaceBytes is the additional
// reservation. Fetch/cache copies and LRU residency belong to the caller.
type DecodeMemory struct {
	InputBytes     int64
	OutputBytes    int64
	WorkspaceBytes int64
}

// EstimateDecodeMemory uses metadata only, before fetching or copying payloads.
// A successful estimate is not integrity, authorization, or location proof.
// The limits below make all arithmetic safe in int64, including on 32-bit hosts.
func EstimateDecodeMemory(e Entry) (DecodeMemory, error) {
	if err := validateDecodeLengths(e); err != nil {
		return DecodeMemory{}, err
	}
	if e.Key.Validate() != nil {
		return DecodeMemory{}, invalid("content", "invalid chunk identity")
	}
	m := DecodeMemory{InputBytes: int64(e.StoredLen), OutputBytes: int64(e.RawLength), WorkspaceBytes: sha256.Size * 3}
	switch e.Codec {
	case "raw":
		if e.StoredLen != e.RawLength {
			return DecodeMemory{}, invalid("content", "raw length mismatch")
		}
	case "zstd":
		// DecodeAll borrows input and uses the destination as history (no
		// window-sized ring). Include malformed-block growth, replacement
		// overlap and the arm64 two-pass sequence array, not just output R.
		const block = int64(128 << 10)
		const sequences = int64(0x7f00 + 0xffff)
		const blockBuffers = 2*(block+17) + 2*(block+16)
		const sequenceBuffers = 2 * sequences * 3 * 8 // at most 64-bit ints
		const tablesAndState = 64 << 10
		const growthRounding = 2 * 8192 // Go 1.27.2 byte-slice page rounding
		m.WorkspaceBytes += 5*m.OutputBytes + 9*block + growthRounding +
			blockBuffers + sequenceBuffers + tablesAndState
	default:
		return DecodeMemory{}, invalid("index", "unknown codec")
	}
	return m, nil
}

// Keep decoder options with the estimator: changes invalidate the source audit.
func newBlobDecoder() (*zstd.Decoder, error) {
	return zstd.NewReader(nil,
		zstd.WithDecoderConcurrency(1), zstd.WithDecoderLowmem(true),
		zstd.WithDecoderMaxMemory(engine.MaxChunkBytes),
		zstd.WithDecoderMaxWindow(engine.MaxChunkBytes), zstd.WithDecodeAllCapLimit(true))
}

func validateDecodeLengths(e Entry) error {
	if e.RawLength == 0 || e.RawLength > engine.MaxChunkBytes ||
		e.StoredLen == 0 || e.StoredLen > MaxPackBytes || e.Offset > MaxPackBytes-uint64(e.StoredLen) {
		return invalid("index", "invalid chunk length or range")
	}
	return nil
}
