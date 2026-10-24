package readpath_test

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestCanonicalMapping(t *testing.T) {
	for _, codec := range []string{"raw", "zstd", "zero"} {
		t.Run(codec, func(t *testing.T) {
			ref, data, e, raw := writerVerificationFixture(t, codec)
			c := storage.Chunk{Version: 1, Key: e.Key, RawBytes: int64(e.RawLength), LocationRevision: 3,
				Location: storage.ChunkLocation{Pack: ref, Range: storage.ByteRange{Offset: int64(e.Offset), Length: int64(e.StoredLen)}, Codec: e.Codec, CRC32C: e.CRC32C}}
			info := storage.PackInfo{Version: 1, Ref: ref, Size: int64(len(data)), StateRevision: 1}
			logical := storage.ChunkRef{Key: e.Key, RawBytes: int64(len(raw))}
			key, entry, err := readpath.MapCanonicalRange(logical, c, info, 3)
			if err != nil || key.Pack != ref || key.Range != c.Location.Range || entry != e {
				t.Fatal(key, entry, err)
			}
			if _, err = readpath.VerifyPackEntry(context.Background(), entry, entryBytes(data, e)); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				name     string
				mutate   func(*storage.Chunk, *storage.PackInfo)
				revision uint64
				want     error
			}{
				{"stale", func(*storage.Chunk, *storage.PackInfo) {}, 2, storage.ErrConflict},
				{"zero revision", func(*storage.Chunk, *storage.PackInfo) {}, 0, storage.ErrConflict},
				{"generation", func(_ *storage.Chunk, p *storage.PackInfo) { p.Ref.Generation = "other" }, 3, storage.ErrInvalid},
				{"bounds", func(c *storage.Chunk, p *storage.PackInfo) { c.Location.Range.Offset = p.Size }, 3, storage.ErrInvalid},
				{"overflow", func(c *storage.Chunk, _ *storage.PackInfo) { c.Location.Range.Offset = math.MaxInt64 }, 3, storage.ErrInvalid},
				{"raw length", func(c *storage.Chunk, _ *storage.PackInfo) { c.RawBytes++ }, 3, storage.ErrInvalid},
				{"pack cap", func(_ *storage.Chunk, p *storage.PackInfo) { p.Size = pack.MaxPackBytes + 1 }, 3, storage.ErrInvalid},
			} {
				t.Run(tc.name, func(t *testing.T) {
					cc, pp := c, info
					tc.mutate(&cc, &pp)
					k, e, err := readpath.MapCanonicalRange(logical, cc, pp, tc.revision)
					if !errors.Is(err, tc.want) || k != (storage.CacheKey{}) || e != (pack.Entry{}) {
						t.Fatal(k, e, err)
					}
				})
			}
		})
	}
}
