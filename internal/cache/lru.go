// Package cache implements disposable physical-byte caching, not read authority.
package cache

import (
	"bytes"
	"container/list"
	"context"
	"fmt"
	"math"
	"sync"

	"github.com/KinasTomes/miles/internal/storage"
)

// Limits is a consumer-review proposal with no production defaults. Byte limits
// count payload lengths, not allocator overhead, caller inputs, retained Get
// outputs, backend fills or decoder memory. Copies are serialized under mu;
// MaxCopyBytes bounds each cache-owned copy operation, not process RSS.
type Limits struct {
	CapacityBytes int64
	MaxEntries    int
	// MaxCopyBytes bounds each internal payload copy. Larger entries are not
	// admitted; best-effort Put may return nil on this bypass. Every resident
	// entry fits this limit when Get copies it. This is not a bound on process
	// RSS or memory retained by callers.
	MaxCopyBytes int64
}

type entry struct {
	key  storage.CacheKey
	data []byte
}

// LRU must be constructed with NewLRU and must not be copied. Its methods do no
// I/O. Callers authorize, resolve, pin and preflight before Get, and verify stored
// bytes before emitting content. Callers must not mutate input during Put.
type LRU struct {
	mu            sync.Mutex
	limits        Limits
	entries       map[storage.CacheKey]*list.Element
	order         list.List // front is MRU
	residentBytes int64
	evictions     uint64
}

var _ storage.Cache = (*LRU)(nil)

func NewLRU(limits Limits) (*LRU, error) {
	if limits.CapacityBytes <= 0 || limits.MaxEntries <= 0 || limits.MaxCopyBytes <= 0 {
		return nil, fmt.Errorf("%w: positive explicit cache limits required", storage.ErrInvalid)
	}
	return &LRU{limits: limits, entries: make(map[storage.CacheKey]*list.Element)}, nil
}

func validKey(key storage.CacheKey) error {
	if err := key.Pack.Validate(); err != nil {
		return err
	}
	// Only arithmetic bounds: authoritative pack size is a consumer obligation.
	return key.Range.Validate(math.MaxInt64)
}

func (c *LRU) Get(ctx context.Context, key storage.CacheKey) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := validKey(key); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	e, ok := c.entries[key]
	if !ok {
		return nil, false, nil
	}
	data := bytes.Clone(e.Value.(*entry).data)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// LP: selection and recency touch; cancellation after this point cannot undo it.
	c.order.MoveToFront(e)
	return data, true, nil
}

// Put is best-effort: nil does not promise residency. An immutable-key conflict
// removes resident ranges of that exact generation and reports ErrConflict.
func (c *LRU) Put(ctx context.Context, key storage.CacheKey, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validKey(key); err != nil {
		return err
	}
	if int64(len(data)) != key.Range.Length {
		return storage.ErrInvalid
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if e, ok := c.entries[key]; ok {
		same := bytes.Equal(e.Value.(*entry).data, data)
		if err := ctx.Err(); err != nil {
			return err
		}
		if !same {
			c.evict(key.Pack)
			return storage.ErrConflict
		}
		c.order.MoveToFront(e)
		return nil
	}
	n := int64(len(data))
	if n > c.limits.CapacityBytes || n > c.limits.MaxCopyBytes {
		return nil
	}
	owned := bytes.Clone(data)
	if err := ctx.Err(); err != nil {
		return err
	}
	// LP: commit this indivisible locked mutation. Subtraction avoids addition
	// overflow even when CapacityBytes is MaxInt64. No fallible work follows.
	for c.residentBytes > c.limits.CapacityBytes-n || len(c.entries) >= c.limits.MaxEntries {
		c.evictions++
		c.remove(c.order.Back())
	}
	c.entries[key] = c.order.PushFront(&entry{key: key, data: owned})
	c.residentBytes += n
	return nil
}

// Occupancy counts payload bytes only; it is not a process RSS estimate.
func (c *LRU) Occupancy() (bytes int64, entries int, evictions uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.residentBytes, len(c.entries), c.evictions
}

func (c *LRU) Purge(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.entries = make(map[storage.CacheKey]*list.Element)
	c.order.Init()
	c.residentBytes = 0
	return nil
}

// Evict affects resident entries only; later Put may refill. It does not unpin,
// revoke access or invalidate owned copies already returned to a caller.
func (c *LRU) Evict(ctx context.Context, ref storage.PackRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	c.evict(ref)
	return nil
}

func (c *LRU) evict(ref storage.PackRef) {
	for key, e := range c.entries {
		if key.Pack == ref {
			c.remove(e)
		}
	}
}
func (c *LRU) remove(e *list.Element) {
	v := e.Value.(*entry)
	delete(c.entries, v.key)
	c.residentBytes -= int64(len(v.data))
	c.order.Remove(e)
}
