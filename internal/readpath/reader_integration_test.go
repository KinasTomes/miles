package readpath

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

// Test-only orchestration for immutable, Warm fixtures. Metadata lookup is NOT
// a pin, root binding or TOCTOU proof. No production verifier/API is introduced.
// Verifier callbacks compare known fixture bytes, not CRC/SHA/zstd semantics.
type integrationExecutor struct {
	chunks   []storage.Chunk
	cache    storage.Cache
	packs    storage.PackStore
	verify   func(context.Context, int, storage.Chunk, []byte) ([]byte, error)
	finished func(int)
}

func (e integrationExecutor) Estimate(i int, c engine.PlannedChunk) (Cost, error) {
	if e.chunks[i].Key != c.Ref.Key || e.chunks[i].RawBytes != c.Ref.RawBytes {
		return Cost{}, storage.ErrInvalid
	}
	// Explicit SMALL FIXTURE budgets, not production defaults or measured RSS.
	// Stored includes cache-hit copy; IO includes probe; Copy covers the fake's
	// range clone and cache fill copy. Raw is exact-owned. Workspace is reserved
	// even though the injected callback is not a decoder. LRU residents have
	// their own capacity; allocator overhead/GC retention are not measured here.
	return Cost{ChunkSlots: 1, StoredBytes: 16, IOSlots: 1, IOBytes: 4, RawBytes: 3, WorkspaceBytes: 1, CopyBytes: 32}, nil
}
func (e integrationExecutor) Execute(ctx context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
	if e.finished != nil {
		defer e.finished(i)
	}
	c := e.chunks[i]
	k := storage.CacheKey{Pack: c.Location.Pack, Range: c.Location.Range}
	stored, hit, err := e.cache.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	if !hit {
		stored, err = FetchRange(ctx, e.packs, k.Pack, k.Range, 3)
		if err != nil {
			return nil, err
		}
	}
	raw, err := e.verify(ctx, i, c, stored)
	if err != nil {
		return nil, err
	}
	if len(raw) != 3 || cap(raw) > 3 {
		return nil, storage.ErrInvalid
	}
	if !hit {
		if err = e.cache.Put(ctx, k, stored); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

// Authorize before any cache/backend access, including on a warm cache.
// Resolve metadata OUTSIDE Estimate. These stable fixtures deliberately reject
// non-Warm classes rather than pretending to implement archive/R1 preflight.
func integrationRead(ctx context.Context, m storage.MetadataStore, credential string, a *Admission, b BucketBinding, p engine.ReadPlan, rb Budget, k int, e integrationExecutor, emit ChunkEmitter) error {
	auth, err := m.Authorize(ctx, credential, p.Object.Bucket)
	if err != nil {
		return err
	}
	if !auth.Read || auth.Bucket.ID != p.Object.BucketID || auth.Bucket.Name != p.Object.Bucket {
		return storage.ErrAccessDenied
	}
	e.chunks = make([]storage.Chunk, len(p.Chunks))
	for i, c := range p.Chunks {
		found, err := m.GetChunks(ctx, []storage.ChunkKey{c.Ref.Key}, true)
		if err != nil {
			return err
		}
		v, ok := found[c.Ref.Key]
		if !ok {
			return storage.ErrNotFound
		}
		if v.Validate() != nil || v.Key != c.Ref.Key || v.RawBytes != c.Ref.RawBytes || v.RawBytes != 3 || v.Location.Range.Length != 3 {
			return storage.ErrInvalid
		}
		packs, err := m.GetPacks(ctx, []storage.PackRef{v.Location.Pack})
		if err != nil {
			return err
		}
		pack, ok := packs[v.Location.Pack]
		if !ok {
			return storage.ErrNotFound
		}
		if pack.Ref != v.Location.Pack || pack.Version != storage.ModelVersion || v.Location.Range.Validate(pack.Size) != nil {
			return storage.ErrInvalid
		}
		if pack.State != "Warm" || pack.Class != storage.Standard {
			return storage.ErrCold
		}
		e.chunks[i] = v // value snapshot, nonzero revision checked by Validate.
	}
	return RunReadAhead(ctx, a, b, rb, p, k, e, emit)
}

type integrationCache struct {
	storage.Cache
	gets, puts atomic.Int32
	beforePut  func(context.Context, storage.CacheKey)
	observe    func()
}

func (c *integrationCache) Get(ctx context.Context, k storage.CacheKey) ([]byte, bool, error) {
	c.gets.Add(1)
	if c.observe != nil {
		c.observe()
	}
	return c.Cache.Get(ctx, k)
}
func (c *integrationCache) Put(ctx context.Context, k storage.CacheKey, b []byte) error {
	c.puts.Add(1)
	if c.beforePut != nil {
		c.beforePut(ctx, k)
	}
	return c.Cache.Put(ctx, k, b)
}

type integrationPacks struct {
	storage.PackStore
	mu               sync.Mutex
	keys             []storage.CacheKey
	mode             string
	entered, unblock chan struct{}
	closes           atomic.Int32
}
type integrationBody struct {
	io.Reader
	body  io.Closer
	owner *integrationPacks
}

func (b *integrationBody) Close() error { b.owner.closes.Add(1); return b.body.Close() }

type integrationBlocked struct {
	p *integrationPacks
	r io.Reader
}

func (r *integrationBlocked) Read(b []byte) (int, error) {
	close(r.p.entered)
	<-r.p.unblock
	return r.r.Read(b)
}
func (p *integrationPacks) RangeGet(ctx context.Context, ref storage.PackRef, span storage.ByteRange) (io.ReadCloser, error) {
	p.mu.Lock()
	p.keys = append(p.keys, storage.CacheKey{Pack: ref, Range: span})
	p.mu.Unlock()
	if p.mode == "backend" {
		return nil, storage.ErrUnavailable
	}
	body, err := p.PackStore.RangeGet(ctx, ref, span)
	if err != nil {
		return nil, err
	}
	var r io.Reader = body
	switch p.mode {
	case "short":
		r = io.LimitReader(body, span.Length-1)
	case "oversized":
		r = io.MultiReader(body, strings.NewReader("!"))
	case "corrupt":
		r = strings.NewReader("BAD")
	case "blocked":
		r = &integrationBlocked{p, body}
	}
	return &integrationBody{r, body, p}, nil
}
func (p *integrationPacks) calls() []storage.CacheKey {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]storage.CacheKey(nil), p.keys...)
}

type integrationFixture struct {
	m        *fake.Metadata
	a        *Admission
	b        BucketBinding
	p        engine.ReadPlan
	c        *integrationCache
	s        *integrationPacks
	e        integrationExecutor
	verified atomic.Int32
	expected map[storage.ChunkKey]string
}

var integrationCorrupt = errors.New("injected fixture verifier rejected bytes")

func newIntegration(t *testing.T, span *storage.ByteRange, w int64) *integrationFixture {
	t.Helper()
	f := &integrationFixture{a: admission(t, 256, 256), b: scheduleBinding(t, w), expected: map[storage.ChunkKey]string{}}
	now := time.Unix(0, 0).UTC()
	info := storage.PackInfo{Version: storage.ModelVersion, Ref: storage.PackRef{ID: "pack", Generation: "g1"}, Size: 8, State: "Warm", Class: storage.Standard}
	writer := storage.WriterFence{LeaseID: "seed", ServiceIncarnation: "fixture", Epoch: 1}
	f.m = fake.NewMetadata(fake.Seed{Now: func() time.Time { return now }, Packs: []storage.PackInfo{info}, Writers: []storage.WriterLease{{Fence: writer, ExpiresAt: now.Add(time.Hour)}}, Buckets: []storage.Bucket{{Name: "demo", ID: "bucket-1", OwnerID: "owner", Domain: "default", Active: true}}, Users: []storage.User{{ID: "reader", Active: true}}, Credentials: []storage.Credential{{ID: "credential", UserID: "reader", Active: true}}, Grants: []storage.Grant{{BucketID: "bucket-1", UserID: "reader", Read: true}}})
	packs := fake.NewPacks(func() time.Time { return now })
	if err := packs.PutPack(context.Background(), info, strings.NewReader("xabcDEFy")); err != nil {
		t.Fatal(err)
	}
	f.s = &integrationPacks{PackStore: packs}
	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 32, MaxEntries: 8, MaxCopyBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	f.c = &integrationCache{Cache: lru}
	head := storage.ObjectHead{Version: storage.ModelVersion, Object: storage.ObjectKey{Bucket: "demo", BucketID: "bucket-1", Key: "object"}, Revision: 1, Domain: "default", ManifestID: "manifest", Size: 9}
	page := storage.ManifestPage{Version: storage.ModelVersion, Domain: "default", ManifestID: "manifest"}
	for i, raw := range []string{"abc", "DEF", "abc"} {
		key := storage.ChunkKey{Domain: "default", Hash: fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))} // fixture identity only
		f.expected[key] = raw
		offset := int64(1)
		if raw == "DEF" {
			offset = 4
		}
		c := storage.Chunk{Version: storage.ModelVersion, Key: key, RawBytes: 3, LocationRevision: 1, Location: storage.ChunkLocation{Pack: info.Ref, Range: storage.ByteRange{Offset: offset, Length: 3}, Codec: "raw"}}
		if _, _, err := f.m.PutChunkIfAbsent(context.Background(), c, writer); err != nil {
			t.Fatal(err)
		}
		page.Chunks = append(page.Chunks, storage.ChunkRef{Key: key, RawBytes: 3, LogicalOffset: int64(i) * 3})
	}
	f.p, err = engine.PlanRead(head.Object, head, []storage.ManifestPage{page}, span)
	if err != nil {
		t.Fatal(err)
	}
	f.e = integrationExecutor{cache: f.c, packs: f.s, verify: func(ctx context.Context, _ int, c storage.Chunk, stored []byte) ([]byte, error) {
		f.verified.Add(1)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if string(stored) != f.expected[c.Key] {
			return nil, integrationCorrupt
		}
		raw := make([]byte, 3)
		copy(raw, stored)
		return raw, nil
	}}
	// The fixture's resource observer runs at Cache.Get, before physical I/O.
	f.c.observe = func() {
		f.a.mu.Lock()
		defer f.a.mu.Unlock()
		for _, v := range f.a.used.total {
			if v <= 0 {
				t.Error("cache access before full resource reservation")
			}
		}
	}
	return f
}
func (f *integrationFixture) run(ctx context.Context, emit ChunkEmitter) error {
	return integrationRead(ctx, f.m, "credential", f.a, f.b, f.p, budget(256), 3, f.e, emit)
}

func TestReaderIntegrationMissHitRange(t *testing.T) {
	for _, span := range []*storage.ByteRange{nil, {Offset: 1, Length: 7}, {Offset: 3, Length: 3}} {
		f := newIntegration(t, span, 0) // sequential repeated occurrence must use cache
		for pass := 0; pass < 2; pass++ {
			var out []byte
			err := f.run(context.Background(), func(_ context.Context, _ int, b []byte) error { out = append(out, b...); b[0] = '!'; return nil })
			if err != nil {
				t.Fatal(err)
			}
			want := "abcDEFabc"[f.p.Range.Offset : f.p.Range.Offset+f.p.Range.Length]
			if string(out) != want {
				t.Fatalf("body %q want %q", out, want)
			}
			drained(t, f.a)
		}
		wantCalls := 2
		if len(f.p.Chunks) == 1 {
			wantCalls = 1
		}
		calls := f.s.calls()
		if len(calls) != wantCalls || f.s.closes.Load() != int32(wantCalls) || f.verified.Load() != int32(2*len(f.p.Chunks)) {
			t.Fatal(calls, f.verified.Load())
		}
		for i, k := range calls {
			offset := int64(1 + i*3)
			if wantCalls == 1 {
				offset = 4
			}
			if k != (storage.CacheKey{Pack: storage.PackRef{ID: "pack", Generation: "g1"}, Range: storage.ByteRange{Offset: offset, Length: 3}}) {
				t.Fatal(k)
			}
		}
	}
}

func TestReaderIntegrationErrors(t *testing.T) {
	for _, mode := range []string{"short", "oversized", "backend", "corrupt", "cache corrupt", "conflict", "emit", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newIntegration(t, &storage.ByteRange{Length: 3}, 0)
			key := storage.CacheKey{Pack: storage.PackRef{ID: "pack", Generation: "g1"}, Range: storage.ByteRange{Offset: 1, Length: 3}}
			want := error(integrationCorrupt)
			switch mode {
			case "short":
				f.s.mode = mode
				want = ErrShortRange
			case "oversized":
				f.s.mode = mode
				want = ErrOversizedRange
			case "backend":
				f.s.mode = mode
				want = storage.ErrUnavailable
			case "corrupt":
				f.s.mode = mode
			case "cache corrupt":
				if err := f.c.Cache.Put(context.Background(), key, []byte("BAD")); err != nil {
					t.Fatal(err)
				}
			case "conflict":
				want = storage.ErrConflict
				f.c.beforePut = func(ctx context.Context, k storage.CacheKey) {
					if err := f.c.Cache.Put(ctx, k, []byte("BAD")); err != nil {
						t.Error(err)
					}
				}
			case "emit":
				want = io.ErrClosedPipe
			case "cancel":
				want = context.Canceled
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			emits := 0
			err := f.run(ctx, func(context.Context, int, []byte) error { emits++; return io.ErrClosedPipe })
			if !errors.Is(err, want) {
				t.Fatalf("got %v want %v", err, want)
			}
			if mode != "emit" && emits != 0 {
				t.Fatal("emitted rejected chunk")
			}
			if mode == "emit" && emits != 1 {
				t.Fatal(emits)
			}
			if mode == "corrupt" || mode == "cache corrupt" {
				if f.c.puts.Load() != 0 || f.verified.Load() != 1 {
					t.Fatal("cached rejected bytes")
				}
			}
			if mode == "cache corrupt" && len(f.s.calls()) != 0 {
				t.Fatal("cache hit fetched backend")
			}
			if mode == "cancel" && (f.c.gets.Load() != 0 || len(f.s.calls()) != 0 || f.verified.Load() != 0) {
				t.Fatal("work after cancellation")
			}
			if mode == "short" || mode == "oversized" || mode == "backend" {
				if f.verified.Load() != 0 || f.c.puts.Load() != 0 {
					t.Fatal("verified incomplete fetch")
				}
			}
			if mode == "conflict" {
				_, hit, err := f.c.Cache.Get(context.Background(), key)
				if hit || err != nil {
					t.Fatal("conflict not evicted", err)
				}
			}
			if len(f.s.calls()) > 0 && mode != "backend" && f.s.closes.Load() != 1 {
				t.Fatal("stream leak")
			}
			drained(t, f.a)
		})
	}
}

func TestReaderIntegrationAuthorizationBeforeWarmCache(t *testing.T) {
	for _, mode := range []string{"denied", "no read", "incarnation"} {
		t.Run(mode, func(t *testing.T) {
			f := newIntegration(t, &storage.ByteRange{Length: 3}, 0)
			if err := f.run(context.Background(), func(context.Context, int, []byte) error { return nil }); err != nil {
				t.Fatal(err)
			}
			gets, verify, calls := f.c.gets.Load(), f.verified.Load(), len(f.s.calls())
			switch mode {
			case "denied":
				f.m.SetCredential(storage.Credential{ID: "credential", UserID: "reader", Active: false})
			case "no read":
				f.m.SetGrant(storage.Grant{BucketID: "bucket-1", UserID: "reader", Write: true})
			case "incarnation":
				f.m.SetBucket(storage.Bucket{Name: "demo", ID: "bucket-2", OwnerID: "reader", Domain: "default", Active: true})
			}
			auth, authErr := f.m.Authorize(context.Background(), "credential", "demo")
			if mode == "no read" && (authErr != nil || auth.Read) {
				t.Fatal("fixture must return success without Read")
			}
			if mode == "incarnation" && (authErr != nil || !auth.Read) {
				t.Fatal("must isolate incarnation mismatch")
			}
			err := f.run(context.Background(), func(context.Context, int, []byte) error { t.Error("unauthorized emit"); return nil })
			if !errors.Is(err, storage.ErrAccessDenied) || f.c.gets.Load() != gets || f.verified.Load() != verify || len(f.s.calls()) != calls {
				t.Fatal("authorization bypass", err)
			}
			drained(t, f.a)
		})
	}
}

func TestReaderIntegrationCancelBlockedFetch(t *testing.T) {
	f := newIntegration(t, &storage.ByteRange{Length: 3}, 0)
	f.s.mode = "blocked"
	f.s.entered = make(chan struct{})
	f.s.unblock = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- f.run(ctx, func(context.Context, int, []byte) error { t.Error("emit canceled work"); return nil })
	}()
	<-f.s.entered
	cancel()
	f.a.mu.Lock()
	slots := f.a.used.total[0]
	f.a.mu.Unlock()
	if slots != 1 || f.s.closes.Load() != 0 {
		t.Error("released before stream quiescence")
	}
	select {
	case err := <-done:
		t.Errorf("early return %v", err)
	default:
	}
	close(f.s.unblock)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if f.s.closes.Load() != 1 || f.verified.Load() != 0 {
		t.Fatal("cleanup/verification")
	}
	drained(t, f.a)
}

func TestReaderIntegrationCompletedPrefetchRetainsBudget(t *testing.T) {
	f := newIntegration(t, nil, 2)
	ready := make(chan int, 3)
	release := make(chan struct{})
	emitting := make(chan struct{})
	consume := make(chan struct{})
	verify := f.e.verify
	f.e.verify = func(ctx context.Context, i int, c storage.Chunk, b []byte) ([]byte, error) {
		raw, err := verify(ctx, i, c, b)
		ready <- i
		if i == 0 {
			<-release
		}
		return raw, err
	}
	// Signal at executor return, after verification AND cache admission. No
	// scheduler assumption is needed: all payload work is finished at this seam.
	finished := make(chan int, 3)
	f.e.finished = func(i int) { finished <- i }
	var out []byte
	done := make(chan error, 1)
	go func() {
		done <- f.run(context.Background(), func(_ context.Context, i int, b []byte) error {
			if i == 0 {
				close(emitting)
				<-consume
			}
			out = append(out, b...)
			return nil
		})
	}()
	for i := 0; i < 3; i++ {
		<-ready
	}
	for i := 0; i < 2; i++ {
		if index := <-finished; index == 0 {
			t.Error("demand completed before release")
		}
	}
	f.a.mu.Lock()
	used := f.a.used.total
	f.a.mu.Unlock()
	if used[0] != 3 || used[4] != 9 {
		t.Error("prefetch result lost reservation", used)
	}
	close(release)
	<-emitting
	f.a.mu.Lock()
	used = f.a.used.total
	f.a.mu.Unlock()
	if used[0] != 3 || used[4] != 9 {
		t.Error("emitter buffer uncharged", used)
	}
	close(consume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, []byte("abcDEFabc")) {
		t.Fatal(string(out))
	}
	drained(t, f.a)
}

func TestReaderIntegrationBestEffortAdmission(t *testing.T) {
	f := newIntegration(t, &storage.ByteRange{Length: 3}, 0)
	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 2, MaxEntries: 1, MaxCopyBytes: 2})
	if err != nil {
		t.Fatal(err)
	}
	f.c.Cache = lru
	for i := 0; i < 2; i++ {
		var out string
		if err := f.run(context.Background(), func(_ context.Context, _ int, b []byte) error { out += string(b); return nil }); err != nil || out != "abc" {
			t.Fatal(out, err)
		}
		drained(t, f.a)
	}
	if len(f.s.calls()) != 2 || f.c.puts.Load() != 2 {
		t.Fatal("nil Put incorrectly treated as residency")
	}
}

func TestReaderIntegrationBudgetRejectionBeforeIO(t *testing.T) {
	for _, scope := range []string{"node", "bucket", "request"} {
		t.Run(scope, func(t *testing.T) {
			f := newIntegration(t, &storage.ByteRange{Length: 3}, 0)
			rb := budget(256)
			switch scope {
			case "node":
				f.a.limits.Node.Budget = budget(2)
			case "bucket":
				f.a.limits.Bucket.Budget = budget(2)
			case "request":
				rb = budget(2)
			}
			err := integrationRead(context.Background(), f.m, "credential", f.a, f.b, f.p, rb, 1, f.e, func(context.Context, int, []byte) error { t.Error("emit despite exhaustion"); return nil })
			var exhausted *ExhaustionError
			if !errors.As(err, &exhausted) || exhausted.Scope != scope {
				t.Fatal(err)
			}
			if f.c.gets.Load() != 0 || len(f.s.calls()) != 0 || f.verified.Load() != 0 {
				t.Fatal("work before reservation")
			}
			drained(t, f.a)
		})
	}
}

// Corrupt response injection, not an alternative metadata implementation.
type integrationMetadataFault struct {
	storage.MetadataStore
	mutate func(*storage.Chunk)
}

func (m integrationMetadataFault) GetChunks(ctx context.Context, keys []storage.ChunkKey, consistent bool) (map[storage.ChunkKey]storage.Chunk, error) {
	values, err := m.MetadataStore.GetChunks(ctx, keys, consistent)
	if err != nil {
		return nil, err
	}
	for k, v := range values {
		m.mutate(&v)
		values[k] = v
	}
	return values, nil
}
func TestReaderIntegrationRejectsMalformedBindings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*storage.Chunk)
		want   error
	}{
		{"identity", func(c *storage.Chunk) { c.Key.Domain = "other" }, storage.ErrInvalid},
		{"raw length", func(c *storage.Chunk) { c.RawBytes = 2 }, storage.ErrInvalid},
		{"revision", func(c *storage.Chunk) { c.LocationRevision = 0 }, storage.ErrInvalid},
		{"generation", func(c *storage.Chunk) { c.Location.Pack.Generation = "missing" }, storage.ErrNotFound},
		{"pack bounds", func(c *storage.Chunk) { c.Location.Range.Offset = 7 }, storage.ErrInvalid},
		{"overflow", func(c *storage.Chunk) { c.Location.Range.Offset = 1<<63 - 1 }, storage.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIntegration(t, &storage.ByteRange{Length: 3}, 0)
			err := integrationRead(context.Background(), integrationMetadataFault{f.m, tc.mutate}, "credential", f.a, f.b, f.p, budget(256), 1, f.e, func(context.Context, int, []byte) error { t.Error("invalid binding emitted"); return nil })
			if !errors.Is(err, tc.want) || f.c.gets.Load() != 0 || len(f.s.calls()) != 0 || f.verified.Load() != 0 {
				t.Fatal(err)
			}
			drained(t, f.a)
		})
	}
}
