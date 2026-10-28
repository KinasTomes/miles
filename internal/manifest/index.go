package manifest

import (
	"encoding/binary"

	"github.com/KinasTomes/miles/internal/storage"
)

// EncodePageIndex is the frozen head field encoding: u64 little endian offsets,
// never a DynamoDB list of numbers or all individual chunk offsets.
func EncodePageIndex(h storage.ObjectHead) ([]byte, error) {
	if err := h.ValidateManifestDescriptor(); err != nil {
		return nil, err
	}
	b := make([]byte, 0, 8*len(h.PageIndex))
	for _, offset := range h.PageIndex {
		b = binary.LittleEndian.AppendUint64(b, offset)
	}
	return b, nil
}

// DecodePageIndex validates shape/order before allocating; the caller installs
// it on the head and validates against logical size with ValidateManifestDescriptor.
func DecodePageIndex(body []byte, pages uint32) ([]uint64, error) {
	if pages > MaxPages || len(body) != int(pages)*8 {
		return nil, storage.ErrInvalid
	}
	out := make([]uint64, pages)
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(body[i*8:])
		if (i == 0 && out[i] != 0) || (i > 0 && out[i] <= out[i-1]) {
			return nil, storage.ErrInvalid
		}
	}
	return out, nil
}
