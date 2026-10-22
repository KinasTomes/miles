package pack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"math/big"
	"testing"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

func TestProductionGoldenPacks(t *testing.T) {
	for _, c := range load(t).Packs {
		t.Run(c.Name, func(t *testing.T) {
			ref := storage.PackRef{ID: c.PackID, Generation: c.Generation}
			builder, err := NewBuilder(ref)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range fixtureBlobs(t, c) {
				codec := codecNames[item.codec]
				if err := builder.Add(Blob{Key: storage.ChunkKey{Domain: item.domain, Hash: hex.EncodeToString(item.hash[:])},
					Codec: codec, RawLength: uint32(len(item.raw)), Stored: item.stored}); err != nil {
					t.Fatal(err)
				}
			}
			data, sidecar, entries, err := builder.Finish()
			if err != nil {
				t.Fatal(err)
			}
			if len(data) != c.Expect.Size || digest(data) != c.Expect.PackSHA256 || digest(sidecar) != c.Expect.IndexSHA256 ||
				c.Expect.PackHex != "" && hex.EncodeToString(data) != c.Expect.PackHex {
				t.Fatalf("production bytes differ from v1 golden: size=%d pack=%s index=%s", len(data), digest(data), digest(sidecar))
			}
			readEntries, readSidecar, err := Inspect(data, ref)
			if err != nil || len(readEntries) != len(entries) || !bytes.Equal(readSidecar, sidecar) {
				t.Fatalf("inspect entries=%d: %v", len(readEntries), err)
			}
			parsed, end, err := ParseIndex(sidecar, ref)
			if err != nil || len(parsed) != c.Expect.Entries || end != uint64(len(data)-len(sidecar)-TrailerBytes) {
				t.Fatalf("sidecar entries=%d end=%d: %v", len(parsed), end, err)
			}
		})
	}
}

func TestProductionRejectsCorruption(t *testing.T) {
	f := load(t)
	bases := map[string]packCase{}
	for _, c := range f.Packs {
		bases[c.Name] = c
	}
	for _, c := range f.Corrupt {
		t.Run(c.Name, func(t *testing.T) {
			base := bases[c.Base]
			builder, err := NewBuilder(storage.PackRef{ID: base.PackID, Generation: base.Generation})
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range fixtureBlobs(t, base) {
				if err := builder.Add(Blob{Key: storage.ChunkKey{Domain: item.domain, Hash: hex.EncodeToString(item.hash[:])},
					Codec: codecNames[item.codec], RawLength: uint32(len(item.raw)), Stored: item.stored}); err != nil {
					t.Fatal(err)
				}
			}
			data, sidecar, _, err := builder.Finish()
			if err != nil {
				t.Fatal(err)
			}
			b := bytes.Clone(data)
			if c.Target == "sidecar" {
				b = bytes.Clone(sidecar)
			}
			if c.XOR != nil {
				at := c.XOR.At
				if at < 0 {
					at += len(b)
				}
				b[at] ^= byte(c.XOR.Mask)
			}
			b = b[:len(b)-c.Truncate]
			if c.Patch != nil {
				idx := b
				if c.Target == "pack" {
					idx = b[len(b)-TrailerBytes-len(sidecar) : len(b)-TrailerBytes]
				}
				copy(idx[c.Patch.At:], unhex(t, c.Patch.Hex))
				sum := sha256.Sum256(idx[:len(idx)-sha256.Size])
				copy(idx[len(idx)-sha256.Size:], sum[:])
			}
			ref := storage.PackRef{ID: base.PackID, Generation: base.Generation}
			if c.ReadAs != nil {
				ref = storage.PackRef{ID: c.ReadAs.PackID, Generation: c.ReadAs.Generation}
			}
			err = nil
			if c.Target == "sidecar" {
				_, _, err = ParseIndex(b, ref)
			} else {
				_, _, err = Inspect(b, ref)
			}
			var formatErr *FormatError
			if !errors.As(err, &formatErr) || formatErr.Stage != c.Stage {
				t.Fatalf("stage %v, want %s", err, c.Stage)
			}
		})
	}
}

func TestProductionInvalidPacks(t *testing.T) {
	for _, c := range load(t).Invalid {
		t.Run(c.Name, func(t *testing.T) {
			data, _ := malformedPack(c.PackID, c.Generation, fixtureBlobs(t, c))
			_, _, err := Inspect(data, storage.PackRef{ID: c.PackID, Generation: c.Generation})
			var formatErr *FormatError
			if !errors.As(err, &formatErr) || formatErr.Stage != c.Stage {
				t.Fatalf("stage %v, want %s", err, c.Stage)
			}
		})
	}
}

func TestProductionRangeReadAndBuilderLimits(t *testing.T) {
	c := load(t).Packs[0]
	ref := storage.PackRef{ID: c.PackID, Generation: c.Generation}
	builder, err := NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range fixtureBlobs(t, c) {
		if err := builder.Add(Blob{Key: storage.ChunkKey{Domain: item.domain, Hash: hex.EncodeToString(item.hash[:])},
			Codec: codecNames[item.codec], RawLength: uint32(len(item.raw)), Stored: item.stored}); err != nil {
			t.Fatal(err)
		}
	}
	data, sidecar, entries, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Add(builder.blobs[0]); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("finalized generation changed: %v", err)
	}
	if _, _, _, err := builder.Finish(); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("generation re-encoded after finish: %v", err)
	}
	ctx := context.Background()
	store := fake.NewPacks(nil)
	info := storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: int64(len(data)), Class: storage.Standard, State: "Warm"}
	if err := store.PutPack(ctx, info, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := store.PutSidecar(ctx, ref, int64(len(sidecar)), bytes.NewReader(sidecar)); err != nil {
		t.Fatal(err)
	}
	gotEntries, gotSidecar, err := ReadFooter(ctx, store, ref)
	if err != nil || len(gotEntries) != len(entries) || !bytes.Equal(gotSidecar, sidecar) {
		t.Fatalf("footer: %v", err)
	}
	for i, e := range gotEntries {
		raw, err := ReadBlobRange(ctx, store, ref, e)
		if err != nil || !bytes.Equal(raw, fixtureBlobs(t, c)[i].raw) {
			t.Fatalf("chunk %d: %v", i, err)
		}
		chunk := storage.Chunk{Version: storage.ModelVersion, Key: e.Key, RawBytes: int64(e.RawLength),
			LocationRevision: 1, Location: e.Location(ref)}
		canonical, err := ReadCanonicalChunk(ctx, store, chunk)
		if err != nil || !bytes.Equal(canonical, raw) {
			t.Fatalf("canonical chunk %d: %v", i, err)
		}
		chunk.Location.CRC32C ^= 1
		if _, err := ReadCanonicalChunk(ctx, store, chunk); err == nil {
			t.Fatal("bad canonical CRC accepted")
		}
	}
}

func TestProductionEntryCapAndSingleFrame(t *testing.T) {
	ref := storage.PackRef{ID: "ab000000000000000000000000000001", Generation: "cd000000000000000000000000000001"}
	builder, err := NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range recipe("max-entries") {
		if err := builder.Add(Blob{Key: storage.ChunkKey{Domain: item.domain, Hash: hex.EncodeToString(item.hash[:])},
			Codec: "raw", RawLength: uint32(len(item.raw)), Stored: item.stored}); err != nil {
			t.Fatal(err)
		}
	}
	if !builder.Ready() {
		t.Fatal("entry cap did not close pack")
	}
	extra := []byte("new chunk")
	sum := sha256.Sum256(extra)
	if err := builder.Add(Blob{Key: storage.ChunkKey{Domain: "default", Hash: hex.EncodeToString(sum[:])},
		Codec: "raw", RawLength: uint32(len(extra)), Stored: extra}); !errors.Is(err, ErrFull) {
		t.Fatalf("entry beyond cap: %v", err)
	}
	frame := rle(0x42, 4096)
	key := storage.ChunkKey{Domain: "default", Hash: digest(bytes.Repeat([]byte{0x42}, 4096))}
	stored := append(bytes.Clone(frame), frame...)
	e := Entry{Key: key, Codec: "zstd", StoredLen: uint32(len(stored)), RawLength: 8192,
		CRC32C: crc32.Checksum(stored, crc32.MakeTable(crc32.Castagnoli))}
	if _, err := ReadBlob(e, stored); err == nil {
		t.Fatal("concatenated zstd frames accepted")
	}
}

func TestEngineZstdPackRoundTrip(t *testing.T) {
	raw := bytes.Repeat([]byte("compressible chunk\n"), 10000)
	key, err := engine.HashChunk("default", raw)
	if err != nil {
		t.Fatal(err)
	}
	compressor, err := engine.NewCompressor(engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3,
		MinSavingRatio: big.NewRat(1, 10)})
	if err != nil {
		t.Fatal(err)
	}
	defer compressor.Close()
	encoded, err := compressor.Encode(engine.Chunk{Key: key, Bytes: raw})
	if err != nil || encoded.Codec != "zstd" {
		t.Fatalf("encode: %s %v", encoded.Codec, err)
	}
	ref, err := NewRef()
	if err != nil {
		t.Fatal(err)
	}
	builder, err := NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Add(Blob{Key: encoded.Key, Codec: encoded.Codec, RawLength: uint32(encoded.RawBytes), Stored: encoded.Bytes}); err != nil {
		t.Fatal(err)
	}
	data, _, entries, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := Inspect(data, ref); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadBlob(entries[0], data[entries[0].Offset:entries[0].Offset+uint64(entries[0].StoredLen)])
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("decoded real zstd frame: %v", err)
	}
}
