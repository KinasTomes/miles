package readpath

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/KinasTomes/miles/internal/storage"
)

var (
	ErrShortRange     = errors.New("physical range response is short")
	ErrOversizedRange = errors.New("physical range response is oversized")
)

// FetchRange is a PROPOSAL physical I/O helper, not a verifier. The caller must
// establish authorization, exact-generation protection and archive preflight,
// and reserve resources before calling. Success transfers an owned, unverified
// stored-byte buffer; an error always returns nil bytes. There is no retry.
//
// maxStoredBytes caps the payload allocation, not total RSS: this helper also
// uses a one-byte probe and fixed bookkeeping. Reserve backend buffers too (the
// shared fake copies the range), and retain the payload charge until consumed.
// There are no admission tickets or production defaults inside this helper.
//
// The body is closed exactly once before return, including on read/close error.
// Cancellation is checked between reads and after Close. PackStore does not
// promise interruptible Read/Close: a blocked backend can delay return. No
// detached goroutine releases ownership early; callers must retain reservations
// until this call and all subsequent buffer use are quiescent. Cancellation
// concurrent with the final check can race success, requiring caller revalidation.
func FetchRange(ctx context.Context, store storage.PackStore, ref storage.PackRef, span storage.ByteRange, maxStoredBytes int64) (data []byte, err error) {
	if ctx == nil || store == nil || maxStoredBytes <= 0 {
		return nil, fmt.Errorf("%w: physical fetch dependencies/limit", storage.ErrInvalid)
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if err := span.Validate(math.MaxInt64); err != nil {
		return nil, err
	}
	if span.Length > maxStoredBytes || uint64(span.Length) > uint64(^uint(0)>>1) {
		return nil, fmt.Errorf("%w: physical fetch length exceeds limit", storage.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := store.RangeGet(ctx, ref, span)
	if body != nil {
		defer func() {
			err = errors.Join(err, body.Close(), ctx.Err())
			if err != nil {
				data = nil
			}
		}()
	}
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, fmt.Errorf("%w: nil range body", storage.ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data = make([]byte, int(span.Length))
	for offset := 0; offset < len(data); {
		n, readErr := readRangePart(ctx, body, data[offset:])
		offset += n
		if readErr != nil {
			if readErr == io.EOF {
				if offset == len(data) {
					break
				}
				return nil, errors.Join(ErrShortRange, io.ErrUnexpectedEOF)
			}
			return nil, readErr
		}
	}
	var probe [1]byte
	n, readErr := readRangePart(ctx, body, probe[:])
	if n != 0 {
		return nil, errors.Join(ErrOversizedRange, readErr)
	}
	if readErr != io.EOF {
		return nil, readErr
	}
	return data, nil
}

// Fail closed on a no-progress reader instead of spinning indefinitely. Such
// a response is not proof of EOF and must never make an exact response succeed.
func readRangePart(ctx context.Context, r io.Reader, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.Read(p)
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("%w: invalid reader count", storage.ErrInvalid)
	}
	if cancelErr := ctx.Err(); cancelErr != nil {
		return n, cancelErr
	}
	if n == 0 && err == nil {
		return 0, io.ErrNoProgress
	}
	return n, err
}
