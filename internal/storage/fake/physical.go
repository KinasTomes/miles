package fake

import (
	"bytes"
	"context"
	"io"
	"math"
	"sync"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

type blob struct {
	Info s.PackInfo
	Body []byte
}
type Packs struct {
	mu              sync.Mutex
	now             func() time.Time
	packs, sidecars map[s.PackRef]blob
	pending         map[s.PackRef]s.RestoreRequest
}

var _ s.PackStore = (*Packs)(nil)

func NewPacks(now func() time.Time) *Packs {
	if now == nil {
		now = time.Now
	}
	return &Packs{now: now, packs: map[s.PackRef]blob{}, sidecars: map[s.PackRef]blob{}, pending: map[s.PackRef]s.RestoreRequest{}}
}
func readBody(ctx context.Context, size int64, r io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil || size < 0 || size == math.MaxInt64 {
		return nil, s.ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(r, size+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, s.ErrInvalid
	}
	return data, nil
}
func (p *Packs) PutPack(ctx context.Context, info s.PackInfo, r io.Reader) error {
	if info.Version != s.ModelVersion || info.Class != s.Standard || info.State != "Warm" {
		return s.ErrInvalid
	}
	if err := info.Ref.Validate(); err != nil {
		return err
	}
	data, err := readBody(ctx, info.Size, r)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if old, ok := p.packs[info.Ref]; ok {
		if old.Info.Size == info.Size && bytes.Equal(old.Body, data) {
			return nil
		}
		return s.ErrConflict
	}
	p.packs[info.Ref] = blob{info, data}
	return nil
}
func (p *Packs) PutSidecar(ctx context.Context, ref s.PackRef, size int64, r io.Reader) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	data, err := readBody(ctx, size, r)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := p.packs[ref]; !ok {
		return s.ErrNotFound
	}
	if old, ok := p.sidecars[ref]; ok {
		if bytes.Equal(old.Body, data) {
			return nil
		}
		return s.ErrConflict
	}
	p.sidecars[ref] = blob{s.PackInfo{Version: s.ModelVersion, Ref: ref, Size: size, Class: s.Standard, State: "Warm"}, data}
	return nil
}
func (p *Packs) HeadPack(ctx context.Context, ref s.PackRef) (s.PackInfo, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.PackInfo{}, err
	}
	if err := ref.Validate(); err != nil {
		return s.PackInfo{}, err
	}
	v, ok := p.packs[ref]
	if !ok {
		return s.PackInfo{}, s.ErrNotFound
	}
	return v.Info, nil
}
func (p *Packs) RangeGet(ctx context.Context, ref s.PackRef, r s.ByteRange) (io.ReadCloser, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	v, ok := p.packs[ref]
	if !ok {
		return nil, s.ErrNotFound
	}
	if err := r.Validate(v.Info.Size); err != nil {
		return nil, err
	}
	if v.Info.Class == s.Glacier && !p.now().Before(v.Info.RestoredUntil) {
		return nil, s.ErrCold
	}
	return io.NopCloser(bytes.NewReader(bytes.Clone(v.Body[r.Offset : r.Offset+r.Length]))), nil
}
func (p *Packs) ChangeStorageClass(ctx context.Context, ref s.PackRef, class s.StorageClass) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := class.Validate(); err != nil {
		return err
	}
	v, ok := p.packs[ref]
	if !ok {
		return s.ErrNotFound
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := v.Info.Class.ValidateTransition(class); err != nil {
		return err
	}
	if v.Info.Class == class {
		return nil
	}
	v.Info.Class = class
	v.Info.RestoredUntil = time.Time{}
	v.Info.State = "Warm"
	if class == s.Glacier || class == s.GlacierIR {
		v.Info.State = "Cold"
	}
	v.Info.StateRevision++
	p.packs[ref] = v
	return nil
}
func (p *Packs) Restore(ctx context.Context, ref s.PackRef, request s.RestoreRequest) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	v, ok := p.packs[ref]
	if !ok {
		return s.ErrNotFound
	}
	if v.Info.Class != s.Glacier || request.Days <= 0 || (request.Tier != "Standard" && request.Tier != "Bulk" && request.Tier != "Expedited") {
		return s.ErrInvalid
	}
	p.pending[ref] = request
	v.Info.State = "Restoring"
	p.packs[ref] = v
	return nil
}

// CompleteRestore is a fixture-only backend completion, separate from acceptance.
func (p *Packs) CompleteRestore(ref s.PackRef, expiry time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.packs[ref]
	if !ok {
		return s.ErrNotFound
	}
	if _, ok := p.pending[ref]; !ok || !expiry.After(p.now()) {
		return s.ErrInvalid
	}
	if expiry.After(v.Info.RestoredUntil) {
		v.Info.RestoredUntil = expiry
	}
	v.Info.State = "Restored"
	p.packs[ref] = v
	delete(p.pending, ref)
	return nil
}
func (p *Packs) DeletePack(ctx context.Context, ref s.PackRef) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	delete(p.packs, ref)
	delete(p.pending, ref)
	return nil
}

// SidecarInfo is a fixture inspection hook, not an additional production port.
func (p *Packs) SidecarInfo(ref s.PackRef) (s.PackInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.sidecars[ref]
	return v.Info, ok
}

type MemoryCache struct {
	mu   sync.Mutex
	data map[s.CacheKey][]byte
}

var _ s.Cache = (*MemoryCache)(nil)

func NewCache() *MemoryCache { return &MemoryCache{data: map[s.CacheKey][]byte{}} }
func validCacheKey(k s.CacheKey) error {
	if err := k.Pack.Validate(); err != nil {
		return err
	}
	return k.Range.Validate(math.MaxInt64)
}
func (c *MemoryCache) Get(ctx context.Context, key s.CacheKey) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if err := validCacheKey(key); err != nil {
		return nil, false, err
	}
	v, ok := c.data[key]
	return bytes.Clone(v), ok, nil
}
func (c *MemoryCache) Put(ctx context.Context, key s.CacheKey, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validCacheKey(key); err != nil {
		return err
	}
	if int64(len(data)) != key.Range.Length {
		return s.ErrInvalid
	}
	c.data[key] = bytes.Clone(data)
	return nil
}
func (c *MemoryCache) Evict(ctx context.Context, ref s.PackRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	for k := range c.data {
		if k.Pack == ref {
			delete(c.data, k)
		}
	}
	return nil
}
