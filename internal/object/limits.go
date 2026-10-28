package object

import (
	"errors"
	"strings"

	"github.com/KinasTomes/miles/internal/storage"
)

const MaxPutBytes int64 = 5 << 30
const MaxUserMetadataBytes = 2 << 10

var (
	ErrObjectTooLarge   = errors.New("object: single PUT exceeds 5 GiB")
	ErrMetadataTooLarge = errors.New("object: user metadata exceeds 2 KiB")
)

// ValidatePut applies API v2 2.5 before admission, reservation or payload I/O.
// User metadata counts UTF-8 bytes of stored header keys and values, including
// x-amz-meta-; internal and system metadata do not consume this budget.
func ValidatePut(length int64, metadata map[string]string) error {
	if length < 0 {
		return storage.ErrInvalid
	}
	if length > MaxPutBytes {
		return ErrObjectTooLarge
	}
	size := 0
	for name, value := range metadata {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-meta-") {
			size += len(name) + len(value)
			if size > MaxUserMetadataBytes {
				return ErrMetadataTooLarge
			}
		}
	}
	return nil
}
