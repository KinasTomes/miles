package readpath

import (
	"context"

	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

// VerifyPackEntry is a lower-level verifier, NOT a protected reader or a
// canonical metadata validator. expected must come from an explicit trusted
// producer/validated format binding that supplied the stored CRC, including
// valid zero. This API cannot prove field presence after metadata decoding;
// never construct expected from an unchecked canonical row or derive its CRC
// from the bytes being verified. Offset/generation admission remains upstream.
//
// stored is borrowed and must remain immutable during this synchronous call.
// Success transfers pack.ReadBlob's owned output; failure returns nil.
// Pack FormatError is preserved. Context is checked before and after verification;
// ReadBlob has no context and cannot interrupt an active decode. No goroutine is
// detached. The caller retains resources until return, then owns the output.
//
// Use pack.EstimateDecodeMemory before any cache copy/fetch/decode to charge
// owned output and workspace. Add the input owner's actual capacity and all
// transport/cache copies separately; process/allocator overhead needs headroom.
// This adapter does not reserve resources or supply a production executor.
func VerifyPackEntry(ctx context.Context, expected pack.Entry, stored []byte) ([]byte, error) {
	if ctx == nil {
		return nil, storage.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(stored) > pack.MaxPackBytes || expected.StoredLen > pack.MaxPackBytes {
		return nil, storage.ErrInvalid
	}
	raw, err := pack.ReadBlob(expected, stored)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}
