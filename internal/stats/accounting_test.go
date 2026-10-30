package stats

import (
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
)

func accountingInput() Input {
	k := s.ChunkKey{Domain: "default", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	p := s.PackRef{ID: "aa", Generation: "g"}
	a := s.ObjectKey{Bucket: "a", BucketID: "a-id", Key: "repeat"}
	b := s.ObjectKey{Bucket: "b", BucketID: "b-id", Key: "other"}
	return Input{Buckets: []s.Bucket{{Name: "a", ID: "a-id", Domain: "default", Active: true}, {Name: "b", ID: "b-id", Domain: "default", Active: true}}, Heads: []s.ObjectHead{{Object: a, Domain: "default", Revision: 1, Size: 200, ChunkCount: 2}, {Object: b, Domain: "default", Revision: 1, Size: 100, ChunkCount: 1}}, References: map[s.ObjectKey][]s.ChunkRef{a: {{Key: k, RawBytes: 100}, {Key: k, RawBytes: 100, LogicalOffset: 100}}, b: {{Key: k, RawBytes: 100}}}, Chunks: map[s.ChunkKey]s.Chunk{k: {Version: 1, Key: k, RawBytes: 100, LocationRevision: 1, Location: s.ChunkLocation{Pack: p, Range: s.ByteRange{Length: 60}, Codec: "zstd"}}}, Packs: map[s.PackRef]s.PackInfo{p: {Ref: p, Size: 80, State: "Warm"}}, Physical: s.PhysicalInventory{Complete: true, VersionScope: "all-retained", Objects: []s.PhysicalObject{{Key: "packs/aa/aa/g", Size: 80, Current: true, Category: "pack", Pack: p}, {Key: "packs-index/aa/aa/g.idx", Size: 10, Current: true, Category: "sidecar", Pack: p}, {Key: "orphan", Size: 7, Current: true, Category: "other"}}}}
}
func TestSharedPhysicalAccounting(t *testing.T) {
	in := accountingInput()
	r, err := Calculate(in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Global.BytesLogical != 300 || r.Global.BytesUniqueRaw != 100 || r.Global.BytesUniqueStored != 60 || *r.Global.BytesStored != 97 || *r.Buckets["a"].BytesStored != 30 || *r.Global.Unallocated != 37 {
		t.Fatal(r)
	}
	for obj, l := range r.Layouts {
		if obj.Bucket == "a" && l.Shared != 2 || obj.Bucket == "b" && l.Shared != 1 {
			t.Fatal(l)
		}
	}
	in.Heads = in.Heads[:1]
	r, err = Calculate(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range r.Layouts {
		if l.Shared != 0 {
			t.Fatal("self repetition is not external sharing", l)
		}
	}
}
func TestZeroAndIncomplete(t *testing.T) {
	r, err := Calculate(Input{Physical: s.PhysicalInventory{Complete: true}})
	if err != nil || r.Global.DedupRatio != nil || r.Global.CompressionRatio != nil || r.Global.TotalReduction != nil {
		t.Fatal(r, err)
	}
	in := accountingInput()
	in.Physical.Complete = false
	r, err = Calculate(in)
	if err != nil || r.Global.BytesStored != nil || r.Global.TotalReduction != nil {
		t.Fatal("unknown is not zero", r, err)
	}
}
func TestVerifiedOverheadAndRounding(t *testing.T) {
	in := accountingInput()
	payload := int64(60)
	in.Physical.Objects[0].PayloadBytes = &payload
	r, err := Calculate(in)
	if err != nil {
		t.Fatal(err)
	}
	if *r.Buckets["a"].BytesStored != 40 || *r.Buckets["b"].BytesStored != 40 || *r.Global.Unallocated != 17 {
		t.Fatal(r)
	}
	*in.Physical.Objects[0].PayloadBytes = 1
	if _, err = Calculate(in); err == nil {
		t.Fatal("impossible verified index accepted")
	}
}

func TestCorruptCanonicalAccountingRejected(t *testing.T) {
	in := accountingInput()
	in.Physical.Objects[0].Size = 1
	if _, err := Calculate(in); err == nil {
		t.Fatal("physical/metadata size mismatch accepted")
	}
	in = accountingInput()
	old := in.Heads[1].Object
	k := in.References[old][0].Key
	other := k
	other.Domain = "isolated"
	c := in.Chunks[k]
	c.Key = other
	in.Chunks[other] = c
	in.Buckets[1].Domain = other.Domain
	in.Heads[1].Domain = other.Domain
	in.References[old][0].Key = other
	if _, err := Calculate(in); err == nil {
		t.Fatal("cross-domain identities aliased to overlapping range")
	}
}
