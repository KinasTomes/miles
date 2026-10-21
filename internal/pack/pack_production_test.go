package pack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

func fixtureBlobs(t *testing.T, c packCase) []blob {
	t.Helper()
	out := recipe(c.Recipe)
	for _, s := range c.Chunks {
		raw := unhex(t, s.RawHex)
		if s.RawRepeat != nil {
			raw = bytes.Repeat([]byte{s.RawRepeat.Byte}, s.RawRepeat.Count)
		}
		b := blob{domain: s.Domain, codec: codecs[s.Codec], raw: raw, stored: raw, hash: sha256.Sum256(raw)}
		if s.StoredHex != "" {
			b.stored = unhex(t, s.StoredHex)
		}
		if s.Hash != "" {
			copy(b.hash[:], unhex(t, s.Hash))
		}
		out = append(out, b)
	}
	return out
}

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
			data, sidecar := base.build(t)
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
			var err error
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
	if err := builder.Add(builder.blobs[0]); err == nil {
		t.Fatal("duplicate chunk accepted")
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
	}
}
