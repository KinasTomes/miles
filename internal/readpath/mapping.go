package readpath

import (
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

// MapCanonicalRange is metadata-only: it neither acquires authority nor does
// I/O. The caller must obtain c from a serializer enforcing CRC field presence
// (including zero), and retain the R1 binding for expectedRevision and info.Ref.
// Comparing revisions here is not an atomic pin or a check against live state.
// Returned values are copies. No cache access or payload allocation occurs.
func MapCanonicalRange(ref storage.ChunkRef, c storage.Chunk, info storage.PackInfo, expectedRevision uint64) (storage.CacheKey, pack.Entry, error) {
	if ref.Key.Validate() != nil || c.Validate() != nil || c.Key != ref.Key || c.RawBytes != ref.RawBytes || c.RawBytes > engine.MaxChunkBytes || info.Version != storage.ModelVersion || info.StateRevision == 0 || info.Ref != c.Location.Pack || info.Size <= 0 || info.Size > pack.MaxPackBytes || c.Location.Range.Validate(info.Size) != nil {
		return storage.CacheKey{}, pack.Entry{}, storage.ErrInvalid
	}
	if expectedRevision == 0 || expectedRevision != c.LocationRevision {
		return storage.CacheKey{}, pack.Entry{}, storage.ErrConflict
	}
	l := c.Location
	return storage.CacheKey{Pack: l.Pack, Range: l.Range}, pack.Entry{Key: c.Key, Codec: l.Codec, Offset: uint64(l.Range.Offset), StoredLen: uint32(l.Range.Length), RawLength: uint32(c.RawBytes), CRC32C: l.CRC32C}, nil
}
