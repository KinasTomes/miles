package stats

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/readpath"
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
)

func TestActualPackPhysicalAccountingAndCorruption(t *testing.T) {
	data := []byte("physical inventory is independent of logical quota")
	sum := sha256.Sum256(data)
	key := s.ChunkKey{Domain: "default", Hash: hex.EncodeToString(sum[:])}
	ref := s.PackRef{ID: "integrity", Generation: "generation"}
	b, err := pack.NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Add(pack.Blob{Key: key, Codec: "raw", RawLength: uint32(len(data)), Stored: data}); err != nil {
		t.Fatal(err)
	}
	encoded, sidecar, entries, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	in := accountingInput()
	obj := in.Heads[0].Object
	in.Heads = in.Heads[:1]
	in.Heads[0].Size = int64(len(data))
	in.Heads[0].ChunkCount = 1
	in.References = map[s.ObjectKey][]s.ChunkRef{obj: {{Key: key, RawBytes: int64(len(data))}}}
	in.Chunks = map[s.ChunkKey]s.Chunk{key: {Version: 1, Key: key, RawBytes: int64(len(data)), LocationRevision: 1, Location: entries[0].Location(ref)}}
	in.Packs = map[s.PackRef]s.PackInfo{ref: {Ref: ref, Size: int64(len(encoded)), State: "Warm"}}
	in.Physical.Objects = []s.PhysicalObject{{Key: "pack", Category: "pack", Pack: ref, Current: true, Size: int64(len(encoded))}, {Key: "sidecar", Category: "sidecar", Current: true, Size: int64(len(sidecar))}}
	r, err := Calculate(in)
	if err != nil || *r.Global.BytesStored != int64(len(encoded)+len(sidecar)) || r.Global.BytesUniqueStored != int64(len(data)) {
		t.Fatal(r, err)
	}
	e := entries[0]
	raw, err := readpath.VerifyPackEntry(context.Background(), e, encoded[e.Offset:e.Offset+uint64(e.StoredLen)])
	if err != nil || !bytes.Equal(raw, data) {
		t.Fatal(err)
	}
	corrupt := bytes.Clone(encoded)
	corrupt[e.Offset] ^= 1
	if _, err = readpath.VerifyPackEntry(context.Background(), e, corrupt[e.Offset:e.Offset+uint64(e.StoredLen)]); err == nil {
		t.Fatal("corrupted stored CRC accepted")
	}
	corrupt = bytes.Clone(sidecar)
	corrupt[len(corrupt)-1] ^= 1
	if _, _, err = pack.ParseIndex(corrupt, ref); err == nil {
		t.Fatal("corrupt index accepted")
	}
	wrong := e
	wrong.Key.Hash = hex.EncodeToString(make([]byte, 32))
	if _, err = readpath.VerifyPackEntry(context.Background(), wrong, encoded[e.Offset:e.Offset+uint64(e.StoredLen)]); err == nil {
		t.Fatal("wrong raw identity accepted")
	}
	if _, _, err = pack.ParseIndex(sidecar, s.PackRef{ID: ref.ID, Generation: "other"}); err == nil {
		t.Fatal("wrong physical generation accepted")
	}
}
