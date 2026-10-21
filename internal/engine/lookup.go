package engine

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/KinasTomes/miles/internal/storage"
)

// Bloom is a bounded, node-local hint. A negative result is not proof that a
// shared metadata store has no row: another node may have inserted it.
type Bloom struct {
	mu    sync.RWMutex
	words []uint64
}

func NewBloom(bitCount uint64) (*Bloom, error) {
	if bitCount < 64 || bitCount > 1<<26 {
		return nil, fmt.Errorf("%w: bloom size", storage.ErrInvalid)
	}
	return &Bloom{words: make([]uint64, (bitCount+63)/64)}, nil
}

func (b *Bloom) positions(key storage.ChunkKey) ([4]uint64, error) {
	pk, err := key.PartitionKey()
	if err != nil {
		return [4]uint64{}, err
	}
	h := sha256.Sum256([]byte(pk))
	a := binary.BigEndian.Uint64(h[:8])
	step := binary.BigEndian.Uint64(h[8:16]) | 1
	bits := uint64(len(b.words)) * 64
	return [4]uint64{a % bits, (a + step) % bits, (a + 2*step) % bits, (a + 3*step) % bits}, nil
}

func (b *Bloom) Add(key storage.ChunkKey) error {
	if b == nil {
		return fmt.Errorf("%w: nil bloom", storage.ErrInvalid)
	}
	positions, err := b.positions(key)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, p := range positions {
		b.words[p/64] |= uint64(1) << (p % 64)
	}
	return nil
}

func (b *Bloom) MaybeContains(key storage.ChunkKey) (bool, error) {
	if b == nil {
		return false, fmt.Errorf("%w: nil bloom", storage.ErrInvalid)
	}
	positions, err := b.positions(key)
	if err != nil {
		return false, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, p := range positions {
		if b.words[p/64]&(uint64(1)<<(p%64)) == 0 {
			return false, nil
		}
	}
	return true, nil
}

type cachedChunk struct {
	key   storage.ChunkKey
	value storage.Chunk
}

// CandidateLRU stores bounded metadata candidates, never publication proof.
type CandidateLRU struct {
	mu       sync.Mutex
	capacity int
	entries  map[storage.ChunkKey]*list.Element
	recent   list.List
}

func NewCandidateLRU(capacity int) (*CandidateLRU, error) {
	if capacity < 1 || capacity > 1<<20 {
		return nil, fmt.Errorf("%w: candidate LRU capacity", storage.ErrInvalid)
	}
	return &CandidateLRU{capacity: capacity, entries: make(map[storage.ChunkKey]*list.Element)}, nil
}

func (c *CandidateLRU) Get(key storage.ChunkKey) (storage.Chunk, bool) {
	if c == nil {
		return storage.Chunk{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return storage.Chunk{}, false
	}
	c.recent.MoveToFront(e)
	return e.Value.(cachedChunk).value, true
}

func (c *CandidateLRU) Put(value storage.Chunk) error {
	if c == nil {
		return fmt.Errorf("%w: nil candidate LRU", storage.ErrInvalid)
	}
	if err := value.Validate(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[value.Key]; ok {
		e.Value = cachedChunk{key: value.Key, value: value}
		c.recent.MoveToFront(e)
		return nil
	}
	e := c.recent.PushFront(cachedChunk{key: value.Key, value: value})
	c.entries[value.Key] = e
	if c.recent.Len() > c.capacity {
		old := c.recent.Back()
		delete(c.entries, old.Value.(cachedChunk).key)
		c.recent.Remove(old)
	}
	return nil
}

func (c *CandidateLRU) Evict(key storage.ChunkKey) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		delete(c.entries, key)
		c.recent.Remove(e)
	}
}

type LookupIndex struct {
	Store storage.MetadataStore
	Bloom *Bloom
	LRU   *CandidateLRU
}

// Candidate returns only a speculative row. A caller must never reuse a
// location from this method without an authoritative read and R1 protection.
func (l LookupIndex) Candidate(key storage.ChunkKey) (storage.Chunk, bool, error) {
	if err := key.Validate(); err != nil {
		return storage.Chunk{}, false, err
	}
	if l.Bloom != nil {
		maybe, err := l.Bloom.MaybeContains(key)
		if err != nil {
			return storage.Chunk{}, false, err
		}
		if !maybe {
			return storage.Chunk{}, false, nil
		}
	}
	row, ok := l.LRU.Get(key)
	return row, ok, nil
}

// Canonical always reads the authoritative store consistently, even when the
// Bloom filter is negative or an LRU candidate exists. The returned row still
// needs an R1 location pin before reuse by a writer.
func (l LookupIndex) Canonical(ctx context.Context, key storage.ChunkKey) (storage.Chunk, bool, error) {
	if err := key.Validate(); err != nil {
		return storage.Chunk{}, false, err
	}
	if l.Store == nil {
		return storage.Chunk{}, false, fmt.Errorf("%w: no metadata store", storage.ErrInvalid)
	}
	rows, err := l.Store.GetChunks(ctx, []storage.ChunkKey{key}, true)
	if err != nil {
		return storage.Chunk{}, false, err
	}
	row, ok := rows[key]
	if !ok {
		l.LRU.Evict(key)
		return storage.Chunk{}, false, nil
	}
	if row.Key != key {
		return storage.Chunk{}, false, fmt.Errorf("%w: mismatched canonical chunk", storage.ErrInvalid)
	}
	if err := row.Validate(); err != nil {
		return storage.Chunk{}, false, err
	}
	if l.Bloom != nil {
		_ = l.Bloom.Add(key)
	}
	if l.LRU != nil {
		_ = l.LRU.Put(row)
	}
	return row, true, nil
}

type LookupAction string

const (
	Insert LookupAction = "insert"
	Reuse  LookupAction = "reuse"
	Revive LookupAction = "revive"
)

// DecideLookup classifies a consistent row and its current pack after the
// caller has attempted R1 protection. pinAccepted must never be inferred from
// a cache hit or a successful metadata read.
func DecideLookup(found bool, state string, class storage.StorageClass, pinAccepted bool) LookupAction {
	if !found {
		return Insert
	}
	if pinAccepted && (state == "Warm" && (class == storage.Standard || class == storage.StandardIA) || state == "Cold" && class == storage.GlacierIR) {
		return Reuse
	}
	return Revive
}
