package jobs

import (
	"context"
	"sort"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

// Cache samples travel with the existing node heartbeat and boot identity.
// No peer Admin bearer or inbound connection is required, including on Edge.
type CacheTier struct {
	UsedBytes     int64  `json:"used_bytes"`
	CapacityBytes int64  `json:"capacity_bytes"`
	Entries       int    `json:"entries"`
	MaxEntries    int    `json:"max_entries"`
	Evictions     uint64 `json:"evictions"`
}
type CacheSample struct {
	SampledAt time.Time `json:"sampled_at"`
	Hits      uint64    `json:"hits"`
	Misses    uint64    `json:"misses"`
	Errors    uint64    `json:"errors"`
	Lookups   uint64    `json:"lookups"`
	Puts      uint64    `json:"puts"`
	Memory    CacheTier `json:"memory"`
	Disk      CacheTier `json:"disk"`
}

func (s CacheSample) Validate() error {
	max := storage.MaxPolicyRevision
	// Errors includes failed puts as well as gets; only successful gets form
	// the ratio denominator. Failed gets account for the remaining lookups.
	if s.SampledAt.IsZero() || s.Hits > max || s.Misses > max-s.Hits || s.Errors > max || s.Lookups > max || s.Lookups < s.Hits+s.Misses || s.Lookups-s.Hits-s.Misses > s.Errors || s.Puts > max {
		return storage.ErrInvalid
	}
	for _, t := range []CacheTier{s.Memory, s.Disk} {
		if t.UsedBytes < 0 || t.CapacityBytes < t.UsedBytes || t.Entries < 0 || t.MaxEntries < t.Entries || t.Evictions > max {
			return storage.ErrInvalid
		}
	}
	return nil
}

type CacheNode struct {
	NodeID      string       `json:"node_id"`
	Incarnation string       `json:"incarnation"`
	State       string       `json:"state"`
	ErrorCode   string       `json:"error_code,omitempty"`
	Sample      *CacheSample `json:"sample"`
	Denominator uint64       `json:"denominator"`
	HitRatio    *float64     `json:"hit_ratio"`
}
type CacheReport struct {
	Scope         string      `json:"scope"`
	Complete      bool        `json:"complete"`
	ObservedAt    time.Time   `json:"observed_at"`
	Nodes         []CacheNode `json:"nodes"`
	IncludedNodes int         `json:"included_nodes"`
	Hits          uint64      `json:"hits"`
	Misses        uint64      `json:"misses"`
	Denominator   uint64      `json:"denominator"`
	HitRatio      *float64    `json:"hit_ratio"`
}

func (c *Cluster) CacheStats(ctx context.Context, scope, nodeID string) (CacheReport, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out := CacheReport{Scope: scope, Complete: true, ObservedAt: c.Registry.Clock.Now().WallUTC, Nodes: []CacheNode{}}
	var nodes []Node
	if scope == "node" && ValidID(nodeID) {
		n, e := c.Runner.Service.Store.GetNode(ctx, nodeID)
		if e != nil {
			return out, e
		}
		nodes = append(nodes, n)
	} else if scope == "cluster" && nodeID == "" {
		cursor := ""
		for pages := 0; ; pages++ {
			if pages >= 100 {
				return out, storage.ErrUnavailable
			}
			p, e := c.ListNodes(ctx, PageRequest{Limit: 100, Cursor: cursor})
			if e != nil {
				return out, e
			}
			for _, n := range p.Nodes {
				if n.ServiceIncarnation == c.Registry.Identity.ServiceIncarnation && out.ObservedAt.Sub(n.HeartbeatAt) <= 2*time.Minute {
					nodes = append(nodes, n)
				}
			}
			if len(nodes) > 64 {
				return out, ErrTooManyTargets
			}
			if p.Next == "" {
				break
			}
			if p.Next == cursor {
				return out, storage.ErrUnavailable
			}
			cursor = p.Next
		}
		if len(nodes) == 0 {
			return out, storage.ErrUnavailable
		}
	} else {
		return out, storage.ErrInvalid
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	// A slow discovery read must not make an expired sample look fresh.
	out.ObservedAt = c.Registry.Clock.Now().WallUTC
	for _, n := range nodes {
		r := CacheNode{NodeID: n.ID, Incarnation: n.Incarnation, State: "unavailable", ErrorCode: "cache_sample_unavailable"}
		s := n.Cache
		if n.ServiceIncarnation == c.Registry.Identity.ServiceIncarnation && !n.HeartbeatAt.After(out.ObservedAt.Add(2*time.Second)) && out.ObservedAt.Sub(n.HeartbeatAt) < HeartbeatMaxAge && !s.SampledAt.IsZero() && s.Validate() == nil && !s.SampledAt.After(n.HeartbeatAt) && out.ObservedAt.Sub(s.SampledAt) < HeartbeatMaxAge {
			r.State = "succeeded"
			r.ErrorCode = ""
			r.Sample = &s
			r.Denominator = s.Hits + s.Misses
			if r.Denominator > 0 {
				ratio := float64(s.Hits) / float64(r.Denominator)
				r.HitRatio = &ratio
			}
			if out.Denominator > storage.MaxPolicyRevision-r.Denominator {
				return out, storage.ErrUnavailable
			}
			out.Hits += s.Hits
			out.Misses += s.Misses
			out.Denominator += r.Denominator
			out.IncludedNodes++
		} else {
			out.Complete = false
		}
		out.Nodes = append(out.Nodes, r)
	}
	if out.Denominator > 0 {
		ratio := float64(out.Hits) / float64(out.Denominator)
		out.HitRatio = &ratio
	}
	return out, nil
}
