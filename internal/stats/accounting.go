// Package stats observes storage without modifying quota, reachability or R1.
package stats

import (
	"fmt"
	s "github.com/KinasTomes/miles/internal/storage"
	"math"
	"math/big"
	"sort"
	"strings"
	"time"
)

const Methodology = "retained-listed-v1/equal-bucket-canonical/verified-overhead/fractional-remainder-unallocated"

type Metrics struct {
	BytesLogical      int64    `json:"bytes_logical"`
	BytesUniqueRaw    int64    `json:"bytes_unique_raw"`
	BytesUniqueStored int64    `json:"bytes_unique_stored"`
	BytesStored       *int64   `json:"bytes_stored"`
	DedupRatio        *float64 `json:"dedup_ratio"`
	CompressionRatio  *float64 `json:"compression_ratio"`
	TotalReduction    *float64 `json:"total_reduction"`
	Objects           int64    `json:"objects"`
	Chunks            int64    `json:"chunks"`
}
type Summary struct {
	Metrics
	SnapshotID             string           `json:"snapshot_id"`
	AsOf                   time.Time        `json:"as_of"`
	CollectionStartedAt    time.Time        `json:"collection_started_at"`
	Consistency            string           `json:"consistency"`
	Methodology            string           `json:"allocation_methodology"`
	VersionScope           string           `json:"physical_version_scope"`
	PhysicalComplete       bool             `json:"physical_complete"`
	Breakdown              map[string]int64 `json:"listed_bytes_by_category"`
	Unallocated            *int64           `json:"unallocated_physical_bytes"`
	TemporaryRestoredBytes *int64           `json:"temporary_restored_billed_bytes"`
	RevivedChunks          *uint64          `json:"revived_chunks_total"`
	RevivedBytes           *uint64          `json:"revived_bytes_stored_total"`
	Packs                  map[string]int64 `json:"packs"`
}
type BucketSummary struct {
	Summary
	Bucket   string `json:"bucket"`
	BucketID string `json:"bucket_id"`
}
type LayoutPack struct {
	PackID       string         `json:"pack_id"`
	Generation   string         `json:"generation"`
	State        string         `json:"state"`
	StorageClass s.StorageClass `json:"storage_class"`
	Chunks       int64          `json:"chunks"`
}
type Layout struct {
	Integrity   string       `json:"integrity"`
	Bucket      string       `json:"bucket"`
	Domain      string       `json:"domain_id"`
	Key         string       `json:"key"`
	Revision    uint64       `json:"revision"`
	Size        int64        `json:"size"`
	Occurrences int64        `json:"chunk_occurrences"`
	Shared      int64        `json:"chunk_occurrences_shared_with_other_objects"`
	Packs       []LayoutPack `json:"packs"`
	AsOf        time.Time    `json:"as_of"`
	Consistency string       `json:"consistency"`
}
type Input struct {
	Buckets    []s.Bucket
	Heads      []s.ObjectHead
	References map[s.ObjectKey][]s.ChunkRef
	Chunks     map[s.ChunkKey]s.Chunk
	Packs      map[s.PackRef]s.PackInfo
	Physical   s.PhysicalInventory
}
type Report struct {
	Global  Summary
	Buckets map[string]BucketSummary
	Layouts map[s.ObjectKey]Layout
}

func add(a *int64, b int64) error {
	if b < 0 || *a > math.MaxInt64-b {
		return s.ErrUnavailable
	}
	*a += b
	return nil
}
func ratio(a, b int64) *float64 {
	if b == 0 {
		return nil
	}
	v := float64(a) / float64(b)
	return &v
}
func finish(m *Metrics) {
	m.DedupRatio = ratio(m.BytesLogical, m.BytesUniqueRaw)
	m.CompressionRatio = ratio(m.BytesUniqueRaw, m.BytesUniqueStored)
	if m.BytesStored != nil {
		m.TotalReduction = ratio(m.BytesLogical, *m.BytesStored)
	}
}

// Calculate requires validated, bounded committed references. It derives live
// identities from heads, never from every row in Chunks or asynchronous refcounts.
func Calculate(in Input) (Report, error) {
	r := Report{Buckets: map[string]BucketSummary{}, Layouts: map[s.ObjectKey]Layout{}}
	r.Global = Summary{Consistency: "approximate", Methodology: Methodology, VersionScope: in.Physical.VersionScope, PhysicalComplete: in.Physical.Complete, Breakdown: map[string]int64{}, Packs: map[string]int64{}}
	current := map[string]s.Bucket{}
	for _, b := range in.Buckets {
		if b.Active {
			if _, ok := current[b.Name]; ok {
				return r, s.ErrUnavailable
			}
			current[b.Name] = b
			bs := r.Global
			bs.Breakdown = nil
			bs.Packs = map[string]int64{}
			r.Buckets[b.Name] = BucketSummary{Summary: bs, Bucket: b.Name, BucketID: b.ID}
		}
	}
	users := map[s.ChunkKey]map[string]bool{}
	total := map[s.ChunkKey]int64{}
	self := map[s.ObjectKey]map[s.ChunkKey]int64{}
	seen := map[s.ObjectKey]bool{}
	for _, h := range in.Heads {
		b, ok := current[h.Object.Bucket]
		if !ok || b.ID != h.Object.BucketID || h.Deleted {
			continue
		}
		if seen[h.Object] || h.Domain != b.Domain || h.Size < 0 {
			return r, s.ErrUnavailable
		}
		seen[h.Object] = true
		bs := r.Buckets[b.Name]
		bs.Objects++
		r.Global.Objects++
		if err := add(&bs.BytesLogical, h.Size); err != nil {
			return r, err
		}
		if err := add(&r.Global.BytesLogical, h.Size); err != nil {
			return r, err
		}
		counts := map[s.ChunkKey]int64{}
		groups := map[s.PackRef]int64{}
		var size int64
		for _, ref := range in.References[h.Object] {
			c, ok := in.Chunks[ref.Key]
			if !ok || c.Validate() != nil || c.Key != ref.Key || ref.Key.Domain != h.Domain || ref.RawBytes != c.RawBytes || ref.LogicalOffset != size {
				return r, s.ErrUnavailable
			}
			if err := add(&size, ref.RawBytes); err != nil {
				return r, err
			}
			p, ok := in.Packs[c.Location.Pack]
			if !ok || p.Ref != c.Location.Pack || p.Size <= 0 || c.Location.Range.Validate(p.Size) != nil {
				return r, s.ErrUnavailable
			}
			counts[c.Key]++
			total[c.Key]++
			groups[p.Ref]++
			if users[c.Key] == nil {
				users[c.Key] = map[string]bool{}
			}
			users[c.Key][b.Name] = true
		}
		if size != h.Size || uint64(len(in.References[h.Object])) != h.ChunkCount {
			return r, s.ErrUnavailable
		}
		self[h.Object] = counts
		r.Buckets[b.Name] = bs
		layout := Layout{Integrity: "metadata-only/not-verified", Bucket: b.Name, Key: h.Object.Key, Domain: h.Domain, Revision: h.Revision, Size: h.Size, Occurrences: int64(h.ChunkCount), Packs: []LayoutPack{}, Consistency: "approximate"}
		for ref, n := range groups {
			p := in.Packs[ref]
			layout.Packs = append(layout.Packs, LayoutPack{PackID: ref.ID, Generation: ref.Generation, State: strings.ToLower(p.State), StorageClass: p.Class, Chunks: n})
		}
		sort.Slice(layout.Packs, func(i, j int) bool {
			a, b := layout.Packs[i], layout.Packs[j]
			return a.PackID+"/"+a.Generation < b.PackID+"/"+b.Generation
		})
		r.Layouts[h.Object] = layout
	}
	weights := map[s.PackRef]map[string]*big.Rat{}
	ranges := map[s.PackRef][]s.ByteRange{}
	var liveStored int64
	for k, buckets := range users {
		c := in.Chunks[k]
		ranges[c.Location.Pack] = append(ranges[c.Location.Pack], c.Location.Range)
		r.Global.Chunks++
		if add(&r.Global.BytesUniqueRaw, c.RawBytes) != nil || add(&r.Global.BytesUniqueStored, c.Location.Range.Length) != nil {
			return r, s.ErrUnavailable
		}
		if add(&liveStored, c.Location.Range.Length) != nil {
			return r, s.ErrUnavailable
		}
		if weights[c.Location.Pack] == nil {
			weights[c.Location.Pack] = map[string]*big.Rat{}
		}
		share := big.NewRat(c.Location.Range.Length, int64(len(buckets)))
		for name := range buckets {
			bs := r.Buckets[name]
			bs.Chunks++
			if add(&bs.BytesUniqueRaw, c.RawBytes) != nil || add(&bs.BytesUniqueStored, c.Location.Range.Length) != nil {
				return r, s.ErrUnavailable
			}
			r.Buckets[name] = bs
			if weights[c.Location.Pack][name] == nil {
				weights[c.Location.Pack][name] = new(big.Rat)
			}
			weights[c.Location.Pack][name].Add(weights[c.Location.Pack][name], share)
		}
	}
	// Distinct domain/hash identities cannot claim the same physical bytes.
	// Repeated logical occurrences have already been deduplicated above.
	for _, spans := range ranges {
		sort.Slice(spans, func(i, j int) bool { return spans[i].Offset < spans[j].Offset })
		for i := 1; i < len(spans); i++ {
			if spans[i].Offset < spans[i-1].Offset+spans[i-1].Length {
				return r, s.ErrUnavailable
			}
		}
	}
	for obj, counts := range self {
		l := r.Layouts[obj]
		for k, n := range counts {
			if total[k] > n {
				l.Shared += n
			}
		}
		r.Layouts[obj] = l
	}
	for _, p := range in.Packs {
		r.Global.Packs[strings.ToLower(p.State)]++
	}
	for ref, buckets := range weights {
		for name := range buckets {
			b := r.Buckets[name]
			b.Packs[strings.ToLower(in.Packs[ref].State)]++
			r.Buckets[name] = b
		}
	}
	alloc := map[string]*big.Rat{}
	physicalSeen := map[string]bool{}
	packSeen := map[s.PackRef]bool{}
	var physical int64
	for _, o := range in.Physical.Objects {
		id := fmt.Sprintf("%d:%s%s", len(o.Key), o.Key, o.VersionID)
		if physicalSeen[id] || o.Key == "" || o.Category == "" {
			return r, s.ErrUnavailable
		}
		physicalSeen[id] = true
		if add(&physical, o.Size) != nil {
			return r, s.ErrUnavailable
		}
		v := r.Global.Breakdown[o.Category]
		if add(&v, o.Size) != nil {
			return r, s.ErrUnavailable
		}
		r.Global.Breakdown[o.Category] = v
		if !o.Current || o.Category != "pack" {
			continue
		}
		if p, ok := in.Packs[o.Pack]; ok && len(weights[o.Pack]) > 0 && p.Size != o.Size {
			return r, s.ErrUnavailable
		}
		if packSeen[o.Pack] {
			return r, s.ErrUnavailable
		}
		packSeen[o.Pack] = true
		factor := big.NewRat(1, 1)
		if o.PayloadBytes != nil {
			if *o.PayloadBytes <= 0 || *o.PayloadBytes > o.Size {
				return r, s.ErrUnavailable
			}
			factor = big.NewRat(o.Size, *o.PayloadBytes)
		}
		var payload big.Rat
		for name, w := range weights[o.Pack] {
			payload.Add(&payload, w)
			if alloc[name] == nil {
				alloc[name] = new(big.Rat)
			}
			alloc[name].Add(alloc[name], new(big.Rat).Mul(w, factor))
		}
		if payload.Cmp(big.NewRat(o.Size, 1)) > 0 || o.PayloadBytes != nil && payload.Cmp(big.NewRat(*o.PayloadBytes, 1)) > 0 {
			return r, s.ErrUnavailable
		}
	}
	if in.Physical.Complete {
		for ref := range weights {
			if !packSeen[ref] {
				return r, s.ErrUnavailable
			}
		}
		if liveStored > physical {
			return r, s.ErrUnavailable
		}
		r.Global.BytesStored = &physical
		unallocated := physical
		for name, b := range r.Buckets {
			n := int64(0)
			if w := alloc[name]; w != nil {
				q := new(big.Int).Quo(w.Num(), w.Denom())
				if !q.IsInt64() {
					return r, s.ErrUnavailable
				}
				n = q.Int64()
			}
			if n > unallocated {
				return r, s.ErrUnavailable
			}
			unallocated -= n
			b.BytesStored = &n
			finish(&b.Metrics)
			r.Buckets[name] = b
		}
		r.Global.Unallocated = &unallocated
	}
	finish(&r.Global.Metrics)
	for name, b := range r.Buckets {
		finish(&b.Metrics)
		r.Buckets[name] = b
	}
	return r, nil
}
