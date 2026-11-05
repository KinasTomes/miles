// Package lifecycle owns pack administration and bounded lifecycle execution.
package lifecycle

import (
	"context"
	"encoding/json"
	s "github.com/KinasTomes/miles/internal/storage"
	"sort"
	"time"
)

// Catalog is an observational head-live projection, never a GC/R2 root proof.
type Inventory interface {
	s.ObservationStore
	GetManifestPage(context.Context, string, uint32) (s.ManifestPage, error)
	GetChunks(context.Context, []s.ChunkKey, bool) (map[s.ChunkKey]s.Chunk, error)
	GetPacks(context.Context, []s.PackRef) (map[s.PackRef]s.PackInfo, error)
}
type Catalog struct {
	Store                     Inventory
	Now                       func() time.Time
	MaxRecords, MaxReferences int
}
type Pack struct {
	PackID            string         `json:"pack_id"`
	Generation        string         `json:"generation"`
	State             string         `json:"state"`
	StorageClass      s.StorageClass `json:"storage_class"`
	StateRevision     uint64         `json:"state_revision"`
	Bytes             int64          `json:"bytes"`
	LiveBytes         int64          `json:"live_bytes"`
	LiveRatio         *float64       `json:"live_ratio"`
	ProtectedUntil    time.Time      `json:"protected_until"`
	MinRetentionUntil time.Time      `json:"min_retention_until"`
	RestoredUntil     time.Time      `json:"restored_until"`
	Readable          bool           `json:"readable"`
	AsOf              time.Time      `json:"as_of"`
	Consistency       string         `json:"consistency"`
}

func Readable(p s.PackInfo, upper time.Time) bool {
	return p.State == "Warm" && (p.Class == s.Standard || p.Class == s.StandardIA) || p.State == "Cold" && p.Class == s.GlacierIR || p.State == "Restored" && p.Class == s.Glacier && upper.Before(p.RestoredUntil)
}
func (c *Catalog) Collect(ctx context.Context) ([]Pack, map[s.PackRef][]s.Chunk, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c == nil || c.Store == nil || c.Now == nil || c.MaxRecords < 1 || c.MaxReferences < 1 {
		return nil, nil, s.ErrInvalid
	}
	packs := []s.PackInfo{}
	heads := []s.ObjectHead{}
	count := 0
	for _, kind := range []string{"packs", "heads"} {
		cursor := ""
		for pages := 0; ; pages++ {
			if pages >= 1000 {
				return nil, nil, s.ErrUnavailable
			}
			page, e := c.Store.ObservePage(ctx, s.ObservationRequest{Kind: kind, Limit: 100, Cursor: cursor})
			if e != nil {
				return nil, nil, e
			}
			packs = append(packs, page.Packs...)
			heads = append(heads, page.Heads...)
			count += len(page.Packs) + len(page.Heads)
			if count > c.MaxRecords {
				return nil, nil, s.ErrUnavailable
			}
			if page.Next == "" {
				break
			}
			if page.Next == cursor {
				return nil, nil, s.ErrUnavailable
			}
			cursor = page.Next
		}
	}
	keys := map[s.ChunkKey]bool{}
	references := 0
	for _, head := range heads {
		if head.Deleted {
			continue
		}
		if head.ValidateManifestDescriptor() != nil {
			return nil, nil, s.ErrUnavailable
		}
		for number := uint32(0); number < head.PageCount; number++ {
			page, e := c.Store.GetManifestPage(ctx, head.ManifestID, number)
			if e != nil {
				return nil, nil, e
			}
			if page.ManifestID != head.ManifestID || page.Number != number || page.Domain != head.Domain {
				return nil, nil, s.ErrUnavailable
			}
			for _, ref := range page.Chunks {
				references++
				if references > c.MaxReferences {
					return nil, nil, s.ErrUnavailable
				}
				keys[ref.Key] = true
			}
		}
	}
	ordered := []s.ChunkKey{}
	for k := range keys {
		ordered = append(ordered, k)
	}
	live := map[s.PackRef][]s.Chunk{}
	bytes := map[s.PackRef]int64{}
	for start := 0; start < len(ordered); start += 64 {
		end := min(start+64, len(ordered))
		rows, e := c.Store.GetChunks(ctx, ordered[start:end], true)
		if e != nil {
			return nil, nil, e
		}
		for _, k := range ordered[start:end] {
			row, ok := rows[k]
			if !ok || row.Validate() != nil {
				return nil, nil, s.ErrUnavailable
			}
			live[row.Location.Pack] = append(live[row.Location.Pack], row)
			bytes[row.Location.Pack] += row.Location.Range.Length
		}
	}
	now := c.Now().UTC()
	out := []Pack{}
	for _, p := range packs {
		if p.Ref.Validate() != nil || p.Size <= 0 || p.Size > 64<<20 || bytes[p.Ref] > p.Size {
			return nil, nil, s.ErrUnavailable
		}
		ratio := float64(bytes[p.Ref]) / float64(p.Size)
		out = append(out, Pack{PackID: p.Ref.ID, Generation: p.Ref.Generation, State: p.State, StorageClass: p.Class, StateRevision: p.StateRevision, Bytes: p.Size, LiveBytes: bytes[p.Ref], LiveRatio: &ratio, ProtectedUntil: p.ProtectedUntil, MinRetentionUntil: p.MinRetentionUntil, RestoredUntil: p.RestoredUntil, Readable: Readable(p, now), AsOf: now, Consistency: "bounded-nonatomic-head-live-projection-not-gc-proof"})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PackID != out[j].PackID {
			return out[i].PackID < out[j].PackID
		}
		return out[i].Generation < out[j].Generation
	})
	for ref := range live {
		sort.Slice(live[ref], func(i, j int) bool {
			a, _ := json.Marshal(live[ref][i].Key)
			b, _ := json.Marshal(live[ref][j].Key)
			return string(a) < string(b)
		})
	}
	return out, live, nil
}
