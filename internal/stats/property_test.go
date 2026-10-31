package stats

import (
	"fmt"
	s "github.com/KinasTomes/miles/internal/storage"
	"math/rand"
	"testing"
)

// The oracle sums the generated logical stream and a distinct identity set;
// it never calls production ratio/allocation/reference collection helpers.
func TestReferenceEnumerationProperty(t *testing.T) {
	for seed := int64(1); seed <= 60; seed++ {
		rng := rand.New(rand.NewSource(seed))
		in := Input{References: map[s.ObjectKey][]s.ChunkRef{}, Chunks: map[s.ChunkKey]s.Chunk{}, Packs: map[s.PackRef]s.PackInfo{}}
		p := s.PackRef{ID: "property", Generation: "g"}
		var offset int64
		for d := 0; d < 2; d++ {
			for h := 0; h < 6; h++ {
				k := s.ChunkKey{Domain: fmt.Sprintf("domain-%d", d), Hash: fmt.Sprintf("%064x", h)}
				raw := int64(50 + h)
				stored := raw
				codec := "raw"
				if h%2 == 0 {
					stored = raw / 2
					codec = "zstd"
				}
				in.Chunks[k] = s.Chunk{Version: 1, Key: k, RawBytes: raw, LocationRevision: 1, Location: s.ChunkLocation{Pack: p, Range: s.ByteRange{Offset: offset, Length: stored}, Codec: codec}}
				offset += stored
			}
		}
		var logical, uniqueRaw, uniqueStored int64
		unique := map[s.ChunkKey]bool{}
		for b := 0; b < 4; b++ {
			bucket := s.Bucket{Name: fmt.Sprintf("bucket-%d", b), ID: fmt.Sprintf("incarnation-%d", b), Domain: fmt.Sprintf("domain-%d", b%2), Active: true}
			in.Buckets = append(in.Buckets, bucket)
			for n := 0; n < 5; n++ {
				obj := s.ObjectKey{Bucket: bucket.Name, BucketID: bucket.ID, Key: fmt.Sprintf("%d/海 + %%2F", n)}
				var size int64
				for occ := rng.Intn(9); occ > 0; occ-- {
					k := s.ChunkKey{Domain: bucket.Domain, Hash: fmt.Sprintf("%064x", rng.Intn(6))}
					c := in.Chunks[k]
					in.References[obj] = append(in.References[obj], s.ChunkRef{Key: k, RawBytes: c.RawBytes, LogicalOffset: size})
					size += c.RawBytes
					logical += c.RawBytes
					if !unique[k] {
						unique[k] = true
						uniqueRaw += c.RawBytes
						uniqueStored += c.Location.Range.Length
					}
				}
				in.Heads = append(in.Heads, s.ObjectHead{Object: obj, Domain: bucket.Domain, Revision: 1, Size: size, ChunkCount: uint64(len(in.References[obj]))})
			}
		}
		in.Packs[p] = s.PackInfo{Ref: p, Size: offset + 17, State: "Warm"}
		in.Physical = s.PhysicalInventory{Complete: true, VersionScope: "all-retained", Objects: []s.PhysicalObject{{Key: "pack", Category: "pack", Current: true, Pack: p, Size: offset + 17}, {Key: "old-version", Category: "pack", Current: false, Pack: p, Size: offset + 13}, {Key: "sidecar", Category: "sidecar", Current: true, Size: 9}}}
		r, err := Calculate(in)
		if err != nil {
			t.Fatal(seed, err)
		}
		if r.Global.BytesLogical != logical || r.Global.BytesUniqueRaw != uniqueRaw || r.Global.BytesUniqueStored != uniqueStored || r.Global.Chunks != int64(len(unique)) {
			t.Fatal(seed, r.Global, logical, uniqueRaw, uniqueStored)
		}
		allocated := *r.Global.Unallocated
		for _, b := range r.Buckets {
			allocated += *b.BytesStored
		}
		if allocated != *r.Global.BytesStored {
			t.Fatal("physical conservation", seed)
		}
		if *r.Global.BytesStored != 2*offset+39 {
			t.Fatal("retained versions/overhead lost", seed, r.Global)
		}
		// Input order cannot change global accounting or bucket allocations.
		rng.Shuffle(len(in.Heads), func(i, j int) { in.Heads[i], in.Heads[j] = in.Heads[j], in.Heads[i] })
		again, err := Calculate(in)
		if err != nil {
			t.Fatal(err)
		}
		for name, b := range r.Buckets {
			if *again.Buckets[name].BytesStored != *b.BytesStored {
				t.Fatal("order-dependent allocation", seed, name)
			}
		}
	}
}
