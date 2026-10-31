package stats

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/KinasTomes/miles/internal/engine"
	s "github.com/KinasTomes/miles/internal/storage"
	"sync"
	"time"
)

type Store interface {
	s.ObservationStore
	s.ManifestPageReader
	GetChunks(context.Context, []s.ChunkKey, bool) (map[s.ChunkKey]s.Chunk, error)
	GetObject(context.Context, s.ObjectKey) (s.ObjectHead, error)
	GetBucket(context.Context, string) (s.Bucket, error)
}
type Options struct {
	Store                                         Store
	Inventory                                     s.PhysicalInventorySource
	Now                                           func() time.Time
	MaxRecords, MaxReferences, MaxPhysicalObjects int
	Timeout, MaxAge, Interval                     time.Duration
}
type Service struct {
	o       Options
	refresh sync.Mutex
	mu      sync.RWMutex
	report  *Report
}

func New(o Options) (*Service, error) {
	if o.Store == nil || o.Inventory == nil || o.Now == nil || o.MaxRecords < 1 || o.MaxRecords > 1000000 || o.MaxReferences < 1 || o.MaxReferences > 1000000 || o.MaxPhysicalObjects < 1 || o.MaxPhysicalObjects > 1000000 || o.Timeout <= 0 || o.MaxAge <= 0 || o.Interval <= 0 {
		return nil, s.ErrInvalid
	}
	return &Service{o: o}, nil
}

// Run collects outside HTTP request paths and joins on cancellation. Errors keep
// the last good report only until MaxAge; a failed refresh cannot publish zeros.
func (m *Service) Run(ctx context.Context) {
	for {
		_ = m.Refresh(ctx)
		timer := time.NewTimer(m.o.Interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
func (m *Service) Refresh(ctx context.Context) error {
	m.refresh.Lock()
	defer m.refresh.Unlock()
	ctx, cancel := context.WithTimeout(ctx, m.o.Timeout)
	defer cancel()
	started := m.o.Now()
	in := Input{References: map[s.ObjectKey][]s.ChunkRef{}, Chunks: map[s.ChunkKey]s.Chunk{}, Packs: map[s.PackRef]s.PackInfo{}}
	records := 0
	for _, kind := range []string{"buckets", "heads", "packs"} {
		cursor := ""
		seen := map[string]bool{}
		for pages := 0; ; pages++ {
			if pages >= m.o.MaxRecords+16 {
				return s.ErrUnavailable
			}
			p, err := m.o.Store.ObservePage(ctx, s.ObservationRequest{Kind: kind, Cursor: cursor, Limit: 256})
			if err != nil {
				return err
			}
			records += len(p.Buckets) + len(p.Heads) + len(p.Packs)
			if records > m.o.MaxRecords {
				return s.ErrUnavailable
			}
			in.Buckets = append(in.Buckets, p.Buckets...)
			in.Heads = append(in.Heads, p.Heads...)
			for _, v := range p.Packs {
				if _, ok := in.Packs[v.Ref]; ok {
					return s.ErrUnavailable
				}
				in.Packs[v.Ref] = v
			}
			if p.Next == "" {
				break
			}
			if p.Next == cursor || seen[p.Next] {
				return s.ErrUnavailable
			}
			seen[p.Next] = true
			cursor = p.Next
		}
	}
	active := map[string]s.Bucket{}
	for _, b := range in.Buckets {
		if b.Active {
			active[b.Name] = b
		}
	}
	refs := 0
	for _, h := range in.Heads {
		b, ok := active[h.Object.Bucket]
		if !ok || h.Deleted || h.Object.BucketID != b.ID {
			continue
		}
		if h.ChunkCount > uint64(m.o.MaxReferences-refs) || h.PageCount > uint32(m.o.MaxReferences+1) {
			return s.ErrUnavailable
		}
		pages := make([]s.ManifestPage, 0, h.PageCount)
		for n := uint32(0); n < h.PageCount; n++ {
			p, err := m.o.Store.GetManifestPage(ctx, h.ManifestID, n)
			if err != nil {
				return err
			}
			refs += len(p.Chunks)
			if refs > m.o.MaxReferences {
				return s.ErrUnavailable
			}
			pages = append(pages, p)
			in.References[h.Object] = append(in.References[h.Object], p.Chunks...)
		}
		if _, err := engine.PlanRead(h.Object, h, pages, nil); err != nil {
			return err
		}
		keys := make([]s.ChunkKey, 0, 128)
		for _, ref := range in.References[h.Object] {
			if _, ok := in.Chunks[ref.Key]; !ok {
				keys = append(keys, ref.Key)
			}
			if len(keys) == 128 {
				if err := m.chunks(ctx, &in, keys); err != nil {
					return err
				}
				keys = keys[:0]
			}
		}
		if err := m.chunks(ctx, &in, keys); err != nil {
			return err
		}
		current, err := m.o.Store.GetObject(ctx, h.Object)
		if err != nil {
			return err
		}
		bucket, err := m.o.Store.GetBucket(ctx, b.Name)
		if err != nil {
			return err
		}
		if current.Revision != h.Revision || current.ManifestID != h.ManifestID || bucket.ID != b.ID {
			return s.ErrConflict
		}
	}
	var err error
	in.Physical, err = m.o.Inventory.CollectInventory(ctx, m.o.MaxPhysicalObjects)
	if err != nil {
		return err
	}
	// Re-observe canonical revisions after inventory; never silently mix a moved
	// canonical range with the old physical generation's accounting.
	keys := make([]s.ChunkKey, 0, 128)
	verify := func() error {
		if len(keys) == 0 {
			return nil
		}
		rows, e := m.o.Store.GetChunks(ctx, keys, true)
		if e != nil {
			return e
		}
		for _, k := range keys {
			if rows[k] != in.Chunks[k] {
				return s.ErrConflict
			}
		}
		keys = keys[:0]
		return nil
	}
	for k := range in.Chunks {
		keys = append(keys, k)
		if len(keys) == 128 {
			if err = verify(); err != nil {
				return err
			}
		}
	}
	if err = verify(); err != nil {
		return err
	}
	r, err := Calculate(in)
	if err != nil {
		return err
	}
	var id [16]byte
	if _, err = rand.Read(id[:]); err != nil {
		return err
	}
	asof := m.o.Now()
	r.Global.SnapshotID = hex.EncodeToString(id[:])
	r.Global.AsOf = asof
	r.Global.CollectionStartedAt = started
	for name, b := range r.Buckets {
		b.SnapshotID = r.Global.SnapshotID
		b.AsOf = asof
		b.CollectionStartedAt = started
		r.Buckets[name] = b
	}
	for key, l := range r.Layouts {
		l.AsOf = asof
		r.Layouts[key] = l
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	m.report = &r
	m.mu.Unlock()
	return nil
}
func (m *Service) chunks(ctx context.Context, in *Input, keys []s.ChunkKey) error {
	if len(keys) == 0 {
		return nil
	}
	rows, err := m.o.Store.GetChunks(ctx, keys, true)
	if err != nil {
		return err
	}
	for _, k := range keys {
		c, ok := rows[k]
		if !ok {
			return s.ErrUnavailable
		}
		in.Chunks[k] = c
	}
	return nil
}
func (m *Service) view(ctx context.Context, fn func(*Report) (any, error), target any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.report == nil || m.o.Now().Before(m.report.Global.AsOf) || m.o.Now().Sub(m.report.Global.AsOf) > m.o.MaxAge {
		return s.ErrUnavailable
	}
	v, err := fn(m.report)
	if err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}
func (m *Service) Global(ctx context.Context) (Summary, error) {
	var v Summary
	e := m.view(ctx, func(r *Report) (any, error) { return r.Global, nil }, &v)
	return v, e
}
func (m *Service) Bucket(ctx context.Context, name string) (BucketSummary, error) {
	var v BucketSummary
	current, err := m.o.Store.GetBucket(ctx, name)
	if errors.Is(err, s.ErrNotFound) {
		return v, s.ErrBucketNotFound
	}
	if err != nil {
		return v, err
	}
	e := m.view(ctx, func(r *Report) (any, error) {
		b, ok := r.Buckets[name]
		if !ok {
			return nil, s.ErrUnavailable // exists, but has not been measured yet
		}
		return b, nil
	}, &v)
	if e == nil {
		if current.ID != v.BucketID {
			return v, s.ErrConflict
		}
	}
	return v, e
}
func (m *Service) Layout(ctx context.Context, bucket, key string) (Layout, error) {
	b, err := m.o.Store.GetBucket(ctx, bucket)
	if errors.Is(err, s.ErrNotFound) {
		return Layout{}, s.ErrBucketNotFound
	}
	if err != nil {
		return Layout{}, err
	}
	var v Layout
	h, err := m.o.Store.GetObject(ctx, s.ObjectKey{Bucket: bucket, BucketID: b.ID, Key: key})
	if err != nil {
		return v, err
	}
	if h.Deleted {
		return v, s.ErrNotFound
	}
	e := m.view(ctx, func(r *Report) (any, error) {
		l, ok := r.Layouts[s.ObjectKey{Bucket: bucket, BucketID: b.ID, Key: key}]
		if !ok {
			return nil, s.ErrUnavailable // new live object, absent from the old report
		}
		return l, nil
	}, &v)
	if e == nil {
		if h.Revision != v.Revision {
			return v, s.ErrConflict
		}
	}
	return v, e
}
