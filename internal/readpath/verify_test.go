package readpath_test

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

// All positive expected CRCs are produced by Builder.Finish, not recomputed
// from response bytes. These explicit fixture bindings are not canonical rows.
func writerVerificationFixture(t testing.TB, codec string) (storage.PackRef, []byte, pack.Entry, []byte) {
	t.Helper()
	raw := []byte("raw fixture")
	if codec == "zstd" {
		raw = bytes.Repeat([]byte("compressible fixture\n"), 200)
	}
	if codec == "zero" {
		raw = []byte{0xab, 0x9b, 0xe0, 0x9b}
	}
	key, err := engine.HashChunk("default", raw)
	if err != nil {
		t.Fatal(err)
	}
	stored := raw
	actualCodec := "raw"
	if codec == "zstd" {
		compressor, err := engine.NewCompressor(engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)})
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := compressor.Encode(engine.Chunk{Key: key, Bytes: raw})
		compressor.Close()
		if err != nil || encoded.Codec != "zstd" {
			t.Fatal("zstd fixture", err)
		}
		stored = encoded.Bytes
		actualCodec = encoded.Codec
	}
	ref := storage.PackRef{ID: "verified-pack", Generation: "g1"}
	b, err := pack.NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Add(pack.Blob{Key: key, Codec: actualCodec, RawLength: uint32(len(raw)), Stored: stored}); err != nil {
		t.Fatal(err)
	}
	data, _, entries, err := b.Finish()
	if err != nil || len(entries) != 1 {
		t.Fatal(err)
	}
	return ref, data, entries[0], raw
}
func entryBytes(data []byte, e pack.Entry) []byte {
	return data[e.Offset : e.Offset+uint64(e.StoredLen)]
}

func TestVerifyPackEntryWriterFixtures(t *testing.T) {
	for _, codec := range []string{"raw", "zstd", "zero"} {
		t.Run(codec, func(t *testing.T) {
			_, data, e, want := writerVerificationFixture(t, codec)
			stored := entryBytes(data, e)
			before := bytes.Clone(stored)
			if codec == "zero" && e.CRC32C != 0 {
				t.Fatal("writer did not produce zero CRC")
			}
			got, err := readpath.VerifyPackEntry(context.Background(), e, stored)
			if err != nil || !bytes.Equal(got, want) || !bytes.Equal(stored, before) {
				t.Fatal("verification/input mutation", err)
			}
			got[0] ^= 0xff
			if !bytes.Equal(stored, before) {
				t.Fatal("output aliases stored")
			}
			again, err := readpath.VerifyPackEntry(context.Background(), e, stored)
			if err != nil || !bytes.Equal(again, want) {
				t.Fatal("owned output changed future verification", err)
			}
		})
	}
}

func TestVerifyPackEntryRejects(t *testing.T) {
	for _, name := range []string{"CRC", "SHA", "raw length", "stored length", "truncated", "output cap", "nil context", "canceled"} {
		t.Run(name, func(t *testing.T) {
			codec := "raw"
			if name == "output cap" {
				codec = "zstd"
			}
			_, data, e, _ := writerVerificationFixture(t, codec)
			stored := entryBytes(data, e)
			ctx := context.Background()
			stage := "content"
			var sentinel error
			switch name {
			case "CRC":
				e.CRC32C ^= 1
				stage = "blob_checksum"
			case "SHA":
				e.Key.Hash = "0000000000000000000000000000000000000000000000000000000000000000"
			case "raw length":
				e.RawLength++
			case "stored length":
				e.StoredLen++
				stage = "blob_checksum"
			case "truncated":
				stored = stored[:len(stored)-1]
				stage = "blob_checksum"
			case "output cap":
				e.RawLength = 1
			case "nil context":
				ctx = nil
				sentinel = storage.ErrInvalid
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				sentinel = context.Canceled
			}
			got, err := readpath.VerifyPackEntry(ctx, e, stored)
			if err == nil || got != nil {
				t.Fatal("returned partial/success", err)
			}
			if sentinel != nil {
				if !errors.Is(err, sentinel) {
					t.Fatal(err)
				}
				return
			}
			var format *pack.FormatError
			if !errors.As(err, &format) || format.Stage != stage {
				t.Fatal("wrong typed stage", err)
			}
		})
	}
}

// Reuse independently self-checked malformed frames. CRCs in these negative
// fixtures are explicit test metadata; the adapter never calculates an expected
// checksum. Map only the real coarse Stage, never parse error text or rerun.
func TestVerifyPackEntryMalformedFrames(t *testing.T) {
	names := map[string]bool{"truncated zstd": true, "zstd trailing byte": true, "multiple zstd frames": true, "unsupported dictionary ID": true, "window exceeds contract bound": true, "content size exceeds contract bound": true, "no FCS output exceeds expected raw length": true, "corrupt zstd with valid stored CRC": true}
	count := 0
	for _, v := range verificationVectors(t) {
		if !names[v.name] {
			continue
		}
		count++
		t.Run(v.name, func(t *testing.T) {
			e := pack.Entry{Key: v.metadata.Key, Codec: v.metadata.Codec, StoredLen: uint32(v.metadata.Range.Length), RawLength: uint32(v.metadata.RawBytes), CRC32C: v.metadata.CRC32C}
			got, err := readpath.VerifyPackEntry(context.Background(), e, v.stored)
			var format *pack.FormatError
			if got != nil || !errors.As(err, &format) || format.Stage != "content" {
				t.Fatal(got, err)
			}
		})
	}
	if count != len(names) {
		t.Fatal("missing frame vectors")
	}
}

// Test-only cache orchestration with a writer-provided Entry. No authority,
// scheduler integration, CRC presence validation or production endpoint.
func fetchVerifiedFixture(ctx context.Context, s storage.PackStore, c storage.Cache, ref storage.PackRef, e pack.Entry) ([]byte, error) {
	k := storage.CacheKey{Pack: ref, Range: storage.ByteRange{Offset: int64(e.Offset), Length: int64(e.StoredLen)}}
	stored, hit, err := c.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if !hit {
		stored, err = readpath.FetchRange(ctx, s, ref, k.Range, pack.MaxPackBytes)
		if err != nil {
			return nil, err
		}
	}
	raw, err := readpath.VerifyPackEntry(ctx, e, stored)
	if err != nil {
		if hit {
			return nil, errors.Join(err, c.Evict(ctx, ref))
		}
		return nil, err
	}
	if !hit {
		if err = c.Put(ctx, k, stored); err != nil {
			return nil, err
		}
	}
	return raw, nil
}
func TestVerifyPackEntryFetchCache(t *testing.T) {
	for _, codec := range []string{"raw", "zstd"} {
		t.Run(codec, func(t *testing.T) {
			ref, data, e, want := writerVerificationFixture(t, codec)
			ctx := context.Background()
			packs := fake.NewPacks(nil)
			if err := packs.PutPack(ctx, storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: int64(len(data)), State: "Warm", Class: storage.Standard}, bytes.NewReader(data)); err != nil {
				t.Fatal(err)
			}
			s := &faultPackStore{PackStore: packs}
			c, err := cache.NewLRU(cache.Limits{CapacityBytes: 8192, MaxEntries: 4, MaxCopyBytes: 8192})
			if err != nil {
				t.Fatal(err)
			}
			for pass := 0; pass < 2; pass++ {
				raw, err := fetchVerifiedFixture(ctx, s, c, ref, e)
				if err != nil || !bytes.Equal(raw, want) {
					t.Fatal(err)
				}
			}
			calls := s.snapshot()
			key := storage.CacheKey{Pack: ref, Range: storage.ByteRange{Offset: int64(e.Offset), Length: int64(e.StoredLen)}}
			if len(calls) != 1 || calls[0] != (rangeCall{ref, key.Range}) {
				t.Fatal(calls)
			}
			if err := c.Evict(ctx, ref); err != nil {
				t.Fatal(err)
			}
			bad := bytes.Clone(entryBytes(data, e))
			bad[0] ^= 0xff
			if err := c.Put(ctx, key, bad); err != nil {
				t.Fatal(err)
			}
			if raw, err := fetchVerifiedFixture(ctx, s, c, ref, e); raw != nil || err == nil {
				t.Fatal("corrupt cache accepted")
			}
			if len(s.snapshot()) != 1 {
				t.Fatal("hidden backend retry")
			}
			if _, hit, err := c.Get(ctx, key); hit || err != nil {
				t.Fatal("corrupt generation not evicted", err)
			}
			badEntry := e
			badEntry.CRC32C ^= 1
			if raw, err := fetchVerifiedFixture(ctx, s, c, ref, badEntry); raw != nil || err == nil {
				t.Fatal("bad expected CRC accepted")
			}
			if _, hit, err := c.Get(ctx, key); hit || err != nil {
				t.Fatal("unverified fill", err)
			}
		})
	}
}

// Allocated bytes/op and allocation count are observations, not peak live RAM
// or a proven workspace bound. No production admission budget derives from this.
func BenchmarkVerifyPackEntry(b *testing.B) {
	for _, codec := range []string{"raw", "zstd"} {
		b.Run(codec, func(b *testing.B) {
			_, data, e, _ := writerVerificationFixture(b, codec)
			stored := entryBytes(data, e)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := readpath.VerifyPackEntry(context.Background(), e, stored); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
