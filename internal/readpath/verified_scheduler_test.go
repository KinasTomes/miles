package readpath

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

// This executor exists ONLY in tests. Entry bindings come from Builder.Finish,
// never unchecked canonical metadata. No read authority or pin is fabricated.
// Workspace allowance is deliberately generous for these small fixtures, NOT a
// proven decoder upper bound. Do not promote this Estimate to production: byte
// counts exclude allocator/GC overhead and do not prove RSS or peak live memory.
type verifiedScheduleExecutor struct {
	ref      storage.PackRef
	entries  []pack.Entry
	store    storage.PackStore
	cache    storage.Cache
	hook     func(context.Context, int, string) error
	verified atomic.Int32
}

func (e *verifiedScheduleExecutor) Estimate(i int, c engine.PlannedChunk) (Cost, error) {
	v := e.entries[i]
	if v.Key != c.Ref.Key || int64(v.RawLength) != c.Ref.RawBytes {
		return Cost{}, storage.ErrInvalid
	}
	s, r := int64(v.StoredLen), int64(v.RawLength)
	return Cost{ChunkSlots: 1, StoredBytes: 2*s + 64, IOSlots: 1, IOBytes: s + 1, RawBytes: 2*r + 64, WorkspaceBytes: 1 << 20, CopyBytes: 4*(s+r) + 128}, nil
}
func (e *verifiedScheduleExecutor) at(ctx context.Context, i int, stage string) error {
	if e.hook != nil {
		if err := e.hook(ctx, i, stage); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (e *verifiedScheduleExecutor) Execute(ctx context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
	if err := e.at(ctx, i, "before fetch"); err != nil {
		return nil, err
	}
	v := e.entries[i]
	key := storage.CacheKey{Pack: e.ref, Range: storage.ByteRange{Offset: int64(v.Offset), Length: int64(v.StoredLen)}}
	stored, hit, err := e.cache.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if !hit {
		stored, err = FetchRange(ctx, e.store, e.ref, key.Range, 8192)
		if err != nil {
			return nil, err
		}
	}
	if err = e.at(ctx, i, "before decode"); err != nil {
		return nil, err
	}
	raw, err := VerifyPackEntry(ctx, v, stored)
	if err != nil {
		if hit {
			return nil, errors.Join(err, e.cache.Evict(ctx, e.ref))
		}
		return nil, err
	}
	e.verified.Add(1)
	if err = e.at(ctx, i, "after decode"); err != nil {
		return nil, err
	}
	if !hit {
		if err = e.cache.Put(ctx, key, stored); err != nil {
			return nil, err
		}
	}
	if err = e.at(ctx, i, "complete"); err != nil {
		return nil, err
	}
	return raw, nil
}

type verifiedScheduleFixture struct {
	a    *Admission
	b    BucketBinding
	p    engine.ReadPlan
	e    *verifiedScheduleExecutor
	s    *integrationPacks
	c    *cache.LRU
	want []byte
}

func newVerifiedSchedule(t *testing.T, w int64) *verifiedScheduleFixture {
	t.Helper()
	ref := storage.PackRef{ID: "writer-pack", Generation: "writer-generation"}
	b, err := pack.NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	raws := [][]byte{[]byte("abc"), bytes.Repeat([]byte("compressible\n"), 100)}
	compressor, err := engine.NewCompressor(engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)})
	if err != nil {
		t.Fatal(err)
	}
	defer compressor.Close()
	for i, raw := range raws {
		key, err := engine.HashChunk("default", raw)
		if err != nil {
			t.Fatal(err)
		}
		blob := pack.Blob{Key: key, Codec: "raw", RawLength: uint32(len(raw)), Stored: raw}
		if i == 1 {
			encoded, err := compressor.Encode(engine.Chunk{Key: key, Bytes: raw})
			if err != nil || encoded.Codec != "zstd" {
				t.Fatal(err)
			}
			blob.Codec = encoded.Codec
			blob.Stored = encoded.Bytes
		}
		if err := b.Add(blob); err != nil {
			t.Fatal(err)
		}
	}
	data, _, entries, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	store := fake.NewPacks(nil)
	if err := store.PutPack(context.Background(), storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: int64(len(data)), State: "Warm", Class: storage.Standard}, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	c, err := cache.NewLRU(cache.Limits{CapacityBytes: 8192, MaxEntries: 8, MaxCopyBytes: 8192})
	if err != nil {
		t.Fatal(err)
	}
	page := storage.ManifestPage{Version: storage.ModelVersion, Domain: "default", ManifestID: "mixed"}
	ordered := []pack.Entry{entries[0], entries[1], entries[0]}
	var offset int64
	for _, e := range ordered {
		page.Chunks = append(page.Chunks, storage.ChunkRef{Key: e.Key, RawBytes: int64(e.RawLength), LogicalOffset: offset})
		offset += int64(e.RawLength)
	}
	head := storage.ObjectHead{Version: storage.ModelVersion, Object: storage.ObjectKey{Bucket: "demo", BucketID: "bucket-1", Key: "mixed"}, Domain: "default", ManifestID: "mixed", Size: offset, Revision: 1}
	p, err := engine.PlanRead(head.Object, head, []storage.ManifestPage{page}, &storage.ByteRange{Offset: 1, Length: offset - 2})
	if err != nil {
		t.Fatal(err)
	}
	s := &integrationPacks{PackStore: store}
	want := append(append(append([]byte{}, raws[0]...), raws[1]...), raws[0]...)
	return &verifiedScheduleFixture{a: admission(t, 16<<20, 16<<20), b: scheduleBinding(t, w), p: p, e: &verifiedScheduleExecutor{ref: ref, entries: ordered, store: s, cache: c}, s: s, c: c, want: want[1 : len(want)-1]}
}
func (f *verifiedScheduleFixture) run(ctx context.Context, k int, emit ChunkEmitter) error {
	return RunReadAhead(ctx, f.a, f.b, budget(16<<20), f.p, k, f.e, emit)
}

func TestVerifiedSchedulerMixedRangeCache(t *testing.T) {
	for _, w := range []int64{0, 2} {
		t.Run(string(rune('0'+w)), func(t *testing.T) {
			f := newVerifiedSchedule(t, w)
			for pass := 0; pass < 2; pass++ {
				before := len(f.s.calls())
				var out []byte
				if err := f.run(context.Background(), 2, func(_ context.Context, _ int, b []byte) error { out = append(out, b...); return nil }); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(out, f.want) {
					t.Fatal("wrong ordered range")
				}
				if pass == 1 && len(f.s.calls()) != before {
					t.Fatal("cache hit fetched backend")
				}
				drained(t, f.a)
			}
			if f.e.verified.Load() != 6 {
				t.Fatal("cache hit skipped production verification")
			}
			for _, call := range f.s.calls() {
				valid := false
				for _, v := range f.e.entries {
					if call.Pack == f.e.ref && call.Range == (storage.ByteRange{Offset: int64(v.Offset), Length: int64(v.StoredLen)}) {
						valid = true
					}
				}
				if !valid {
					t.Fatal(call)
				}
			}
		})
	}
}

func TestVerifiedSchedulerOrderingAndSaturation(t *testing.T) {
	f := newVerifiedSchedule(t, 2)
	started := make(chan int, 3)
	completed := make(chan int, 3)
	gates := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	f.e.hook = func(ctx context.Context, i int, stage string) error {
		if stage == "before fetch" {
			started <- i
			select {
			case <-gates[i]:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if stage == "complete" {
			completed <- i
		}
		return nil
	}
	done := make(chan error, 1)
	var out []byte
	go func() {
		done <- f.run(context.Background(), 2, func(_ context.Context, _ int, b []byte) error { out = append(out, b...); return nil })
	}()
	seen := map[int]bool{<-started: true}
	seen[<-started] = true
	if !seen[0] || !seen[1] {
		t.Error("K window", seen)
	}
	close(gates[1])
	if i := <-completed; i != 1 {
		t.Error(i)
	}
	f.a.mu.Lock()
	slots := f.a.used.total[0]
	f.a.mu.Unlock()
	if slots != 2 {
		t.Error("completed speculative buffer released")
	}
	select {
	case i := <-started:
		t.Error("K exceeded", i)
	default:
	}
	close(gates[0])
	<-completed
	if i := <-started; i != 2 {
		t.Error(i)
	}
	close(gates[2])
	<-completed
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, f.want) {
		t.Fatal("out of order output")
	}
	drained(t, f.a)
}

func TestVerifiedSchedulerFailures(t *testing.T) {
	for _, mode := range []string{"CRC", "SHA", "short", "oversized", "backend corrupt", "cache corrupt", "emitter"} {
		t.Run(mode, func(t *testing.T) {
			f := newVerifiedSchedule(t, 0)
			v := f.e.entries[0]
			key := storage.CacheKey{Pack: f.e.ref, Range: storage.ByteRange{Offset: int64(v.Offset), Length: int64(v.StoredLen)}}
			var want error
			stage := "blob_checksum"
			switch mode {
			case "CRC":
				f.e.entries[0].CRC32C ^= 1
			case "SHA":
				f.e.entries[0].Key.Hash = "0000000000000000000000000000000000000000000000000000000000000000"
				f.p.Chunks[0].Ref.Key = f.e.entries[0].Key
				stage = "content"
			case "short":
				f.s.mode = mode
				want = ErrShortRange
			case "oversized":
				f.s.mode = mode
				want = ErrOversizedRange
			case "backend corrupt":
				f.s.mode = "corrupt"
			case "cache corrupt":
				if err := f.c.Put(context.Background(), key, []byte("BAD")); err != nil {
					t.Fatal(err)
				}
			case "emitter":
				want = io.ErrClosedPipe
			}
			emits := 0
			err := f.run(context.Background(), 1, func(context.Context, int, []byte) error { emits++; return io.ErrClosedPipe })
			if want != nil {
				if !errors.Is(err, want) {
					t.Fatal(err)
				}
			} else {
				var format *pack.FormatError
				if !errors.As(err, &format) || format.Stage != stage {
					t.Fatal(err)
				}
			}
			if mode != "emitter" {
				if emits != 0 {
					t.Fatal("unverified emit")
				}
				if _, hit, err := f.c.Get(context.Background(), key); hit || err != nil {
					t.Fatal("corrupt/unverified resident", err)
				}
			}
			if mode == "cache corrupt" && len(f.s.calls()) != 0 {
				t.Fatal("fallback fetch")
			}
			drained(t, f.a)
		})
	}
}

func TestVerifiedSchedulerCancellationBoundaries(t *testing.T) {
	for _, stage := range []string{"before fetch", "before decode", "after decode", "emitter"} {
		t.Run(stage, func(t *testing.T) {
			f := newVerifiedSchedule(t, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			wait := func() { close(entered); <-release }
			f.e.hook = func(_ context.Context, i int, s string) error {
				if i == 0 && s == stage {
					wait()
				}
				return nil
			}
			done := make(chan error, 1)
			go func() {
				done <- f.run(ctx, 1, func(ctx context.Context, i int, _ []byte) error {
					if stage != "emitter" {
						t.Error("unexpected emit")
					} else if i == 0 {
						wait()
					}
					return ctx.Err()
				})
			}()
			<-entered
			cancel()
			f.a.mu.Lock()
			slots := f.a.used.total[0]
			f.a.mu.Unlock()
			if slots != 1 {
				t.Error("released before worker/emitter quiescence")
			}
			select {
			case err := <-done:
				t.Error("early return", err)
			default:
			}
			close(release)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if stage == "before fetch" && len(f.s.calls()) != 0 {
				t.Fatal("fetch after cancel")
			}
			if stage == "before decode" && f.e.verified.Load() != 0 {
				t.Fatal("decode after cancel")
			}
			if stage == "after decode" && f.e.verified.Load() != 1 {
				t.Fatal("did not execute real decode")
			}
			drained(t, f.a)
		})
	}
}
