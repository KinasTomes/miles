package stats

import (
	s "github.com/KinasTomes/miles/internal/storage"
	"reflect"
	"testing"
	"time"
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

func TestPhysicalLifecycleIncludesColdDeadAndProtectedRetired(t *testing.T) {
	in := accountingInput()
	in.ObservedAt = time.Unix(100, 0)
	cold := s.PackRef{ID: "bb", Generation: "g"}
	retired := s.PackRef{ID: "cc", Generation: "g"}
	orphan := s.PackRef{ID: "dd", Generation: "g"}
	in.Packs[cold] = s.PackInfo{Ref: cold, Size: 20, Class: s.Glacier, State: "Cold"}
	in.Packs[retired] = s.PackInfo{Ref: retired, Size: 30, State: "Retired"}
	in.Protected = map[s.PackRef]bool{retired: true}
	in.Physical.Objects = append(in.Physical.Objects,
		s.PhysicalObject{Key: "cold", VersionID: "new", Category: "pack", Class: s.Glacier, Current: true, Pack: cold, Size: 20},
		s.PhysicalObject{Key: "cold", VersionID: "old", Category: "pack", Class: s.Standard, Pack: cold, Size: 15},
		s.PhysicalObject{Key: "retired", Category: "pack", Class: s.Standard, Current: true, Pack: retired, Size: 30},
		s.PhysicalObject{Key: "orphan-pack", Category: "pack", Current: true, Pack: orphan, Size: 5},
	)
	report, err := Calculate(in)
	if err != nil {
		t.Fatal(err)
	}
	g := report.Global
	if g.BytesLogical != 300 || *g.BytesStored != 167 || *g.KnownProtectedBytes != 30 || g.PhysicalClasses[string(s.Glacier)] != 20 || *g.PackOverhead != 20 ||
		!reflect.DeepEqual(g.PhysicalLifecycle, map[string]int64{"head_live": 90, "cold_dead": 20, "noncurrent": 15, "retired": 30, "orphan": 5, "other": 7}) {
		t.Fatal(g)
	}
	if b := report.Buckets["a"]; b.KnownProtectedBytes != nil || b.PhysicalLifecycle != nil || b.PackOverhead != nil || b.BytesLogical != 200 {
		t.Fatal(b)
	}
}

// Revival relocates an identity to a new Warm pack. Physical bytes grow and the
// superseded archive stays listed as cold_dead; identity-based dedup is unchanged.
func TestRevivalKeepsDedupRatioAndListsColdDead(t *testing.T) {
	in := accountingInput()
	before, err := Calculate(in)
	if err != nil {
		t.Fatal(err)
	}
	k := s.ChunkKey{Domain: "default", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	old, warm := s.PackRef{ID: "aa", Generation: "g"}, s.PackRef{ID: "ee", Generation: "g"}
	in.Packs[old] = s.PackInfo{Ref: old, Size: 80, State: "Cold", Class: s.Glacier}
	in.Packs[warm] = s.PackInfo{Ref: warm, Size: 70, State: "Warm", Class: s.Standard}
	c := in.Chunks[k]
	c.Location.Pack, c.LocationRevision = warm, 2
	in.Chunks[k] = c
	in.Physical.Objects = append(in.Physical.Objects, s.PhysicalObject{Key: "packs/ee/ee/g", Size: 70, Current: true, Category: "pack", Pack: warm})
	after, err := Calculate(in)
	if err != nil {
		t.Fatal(err)
	}
	b, a := before.Global, after.Global
	if *b.DedupRatio != 3 || *a.DedupRatio != *b.DedupRatio || a.BytesUniqueRaw != b.BytesUniqueRaw || *a.BytesStored != *b.BytesStored+70 ||
		*a.TotalReduction >= *b.TotalReduction || *a.PackOverhead != 10 || a.PhysicalLifecycle["cold_dead"] != 90 || a.PhysicalLifecycle["head_live"] != 70 {
		t.Fatal(b, a)
	}
}
