package cache

import (
	"context"
	"errors"
	"sync"

	"github.com/KinasTomes/miles/internal/storage"
)

// Node serializes tier admission/purge locally. It never authorizes a request.
// Eviction/purge removes resident bytes, and later fills may repopulate them.
type Node struct {
	mu                                  sync.Mutex
	id                                  string
	memory                              *LRU
	disk                                *Disk
	lookups, hits, misses, errors, puts uint64
	closed                              bool
}
type TierStats struct {
	UsedBytes     int64
	CapacityBytes int64
	MaxEntries    int
	Entries       int
	Evictions     uint64
}
type NodeStats struct {
	Scope, NodeID                       string
	Lookups, Hits, Misses, Errors, Puts uint64
	HitRatio                            float64
	Memory, Disk                        TierStats
}

func NewNode(id string, memory *LRU, disk *Disk) (*Node, error) {
	if id == "" || memory == nil {
		return nil, storage.ErrInvalid
	}
	return &Node{id: id, memory: memory, disk: disk}, nil
}

var _ storage.Cache = (*Node)(nil)

func (n *Node) Get(ctx context.Context, k storage.CacheKey) ([]byte, bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if n.closed {
		return nil, false, storage.ErrUnavailable
	}
	n.lookups++
	b, hit, err := n.memory.Get(ctx, k)
	if err == nil && !hit && n.disk != nil {
		b, hit, err = n.disk.Get(ctx, k)
		if err == nil && hit {
			err = n.memory.Put(ctx, k, b)
		}
	}
	if err != nil {
		n.errors++
		if errors.Is(err, storage.ErrConflict) {
			_ = n.memory.Evict(context.WithoutCancel(ctx), k.Pack)
			if n.disk != nil {
				_ = n.disk.Evict(context.WithoutCancel(ctx), k.Pack)
			}
		}
		return nil, false, err
	}
	if hit {
		n.hits++
	} else {
		n.misses++
	}
	return b, hit, nil
}
func (n *Node) Put(ctx context.Context, k storage.CacheKey, b []byte) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if n.closed {
		return storage.ErrUnavailable
	}
	n.puts++
	var err error
	if n.disk != nil {
		err = n.disk.Put(ctx, k, b)
	}
	if err == nil {
		err = n.memory.Put(ctx, k, b)
	}
	if err != nil {
		n.errors++
		if errors.Is(err, storage.ErrConflict) {
			_ = n.memory.Evict(context.WithoutCancel(ctx), k.Pack)
			if n.disk != nil {
				_ = n.disk.Evict(context.WithoutCancel(ctx), k.Pack)
			}
		}
	}
	return err
}
func (n *Node) Evict(ctx context.Context, ref storage.PackRef) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return storage.ErrUnavailable
	}
	err := n.memory.Evict(ctx, ref)
	if n.disk != nil {
		err = errors.Join(err, n.disk.Evict(ctx, ref))
	}
	return err
}
func (n *Node) Stats() NodeStats {
	n.mu.Lock()
	defer n.mu.Unlock()
	s := NodeStats{Scope: "node", NodeID: n.id, Lookups: n.lookups, Hits: n.hits, Misses: n.misses, Errors: n.errors, Puts: n.puts}
	s.Memory.UsedBytes, s.Memory.Entries, s.Memory.Evictions = n.memory.Occupancy()
	s.Memory.CapacityBytes = n.memory.limits.CapacityBytes
	s.Memory.MaxEntries = n.memory.limits.MaxEntries
	if n.disk != nil {
		s.Disk.UsedBytes, s.Disk.Entries, s.Disk.Evictions = n.disk.Occupancy()
		s.Disk.CapacityBytes = n.disk.limits.CapacityBytes
		s.Disk.MaxEntries = n.disk.limits.MaxEntries
	}
	// Errors are excluded from the successful lookup denominator; tier promotion
	// is one logical hit, not two. No attempts means ratio zero with zero samples.
	if d := s.Hits + s.Misses; d > 0 {
		s.HitRatio = float64(s.Hits) / float64(d)
	}
	return s
}
func (n *Node) PurgeNode(ctx context.Context, id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if id != n.id {
		return storage.ErrInvalid
	}
	if n.closed {
		return storage.ErrUnavailable
	}
	err := n.memory.Purge(ctx)
	if n.disk != nil {
		err = errors.Join(err, n.disk.Purge(ctx))
	}
	return err
}
func (n *Node) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.closed {
		return nil
	}
	if err := n.memory.Purge(context.Background()); err != nil {
		return err
	}
	if n.disk != nil {
		if err := n.disk.Close(); err != nil {
			return err
		}
	}
	n.closed = true
	return nil
}

// PurgeClient is the Jobs/Nodes fan-out hook. The trusted caller authorizes an
// admin, freezes target membership and supplies bounded transport deadlines.
// W04 never discovers nodes or claims that one local purge clears the cluster.
type PurgeClient interface {
	PurgeNode(context.Context, string) error
}
type PurgeResult struct {
	NodeID string
	Err    error
}

func FanoutPurge(ctx context.Context, nodes []string, client PurgeClient) ([]PurgeResult, error) {
	if client == nil || len(nodes) == 0 || len(nodes) > 256 {
		return nil, storage.ErrInvalid
	}
	nodes = append([]string(nil), nodes...)
	seen := map[string]bool{}
	for _, id := range nodes {
		if id == "" || seen[id] {
			return nil, storage.ErrInvalid
		}
		seen[id] = true
	}
	out := make([]PurgeResult, 0, len(nodes))
	for _, id := range nodes {
		err := ctx.Err()
		if err == nil {
			err = client.PurgeNode(ctx, id)
		}
		out = append(out, PurgeResult{id, err})
	}
	return out, nil
}
