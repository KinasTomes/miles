package readpath

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/storage"
)

// This executor tests scheduling only; it does not verify CRC/SHA/zstd or pins.
type scheduleExecutor struct {
	estimate func(int, engine.PlannedChunk) (Cost, error)
	execute  func(context.Context, int, engine.PlannedChunk) ([]byte, error)
}

func (e scheduleExecutor) Estimate(i int, c engine.PlannedChunk) (Cost, error) {
	if e.estimate != nil {
		return e.estimate(i, c)
	}
	return Cost{ChunkSlots: 1, StoredBytes: 3, IOSlots: 1, IOBytes: 3, RawBytes: 3, WorkspaceBytes: 1, CopyBytes: 3}, nil
}
func (e scheduleExecutor) Execute(ctx context.Context, i int, c engine.PlannedChunk) ([]byte, error) {
	return e.execute(ctx, i, c)
}
func scheduleBinding(t *testing.T, w int64) BucketBinding {
	t.Helper()
	v, e := config.NewPolicyValidator(config.DSGPolicyLimits())
	if e != nil {
		t.Fatal(e)
	}
	p, e := v.NewBucketSnapshot("bucket-1", 1, config.CachePolicy{ReadaheadChunks: w})
	if e != nil {
		t.Fatal(e)
	}
	return BucketBinding{Name: "demo", Policy: p}
}
func schedulePlan(t *testing.T, n int, span *storage.ByteRange) engine.ReadPlan {
	t.Helper()
	h := storage.ObjectHead{Version: storage.ModelVersion, Object: storage.ObjectKey{Bucket: "demo", BucketID: "bucket-1", Key: "object"}, Revision: 1, Domain: "default", ManifestID: "m", Size: int64(n) * 3}
	var pages []storage.ManifestPage
	if n > 0 {
		p := storage.ManifestPage{Version: storage.ModelVersion, Domain: h.Domain, ManifestID: h.ManifestID}
		for i := 0; i < n; i++ {
			p.Chunks = append(p.Chunks, storage.ChunkRef{Key: storage.ChunkKey{Domain: h.Domain, Hash: fmt.Sprintf("%x", sha256.Sum256([]byte("abc")))}, RawBytes: 3, LogicalOffset: int64(i) * 3})
		}
		pages = append(pages, p)
	}
	p, e := engine.PlanRead(h.Object, h, pages, span)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestReadAheadWindowAndOrderedCompletion(t *testing.T) {
	for _, tc := range []struct {
		w    int64
		k, n int
	}{{0, 8, 6}, {1, 8, 6}, {4, 3, 9}, {4, 8, 9}, {64, 1000, 70}} {
		t.Run(fmt.Sprintf("W%d_K%d", tc.w, tc.k), func(t *testing.T) {
			a := admission(t, 4096, 4096)
			p := schedulePlan(t, tc.n, nil)
			limit := min(tc.k, int(tc.w)+1, tc.n)
			started := make(chan int, tc.n)
			emitting := make(chan int)
			consume := make(chan struct{})
			done := make(chan error, 1)
			gates := make([]chan struct{}, tc.n)
			for i := range gates {
				gates[i] = make(chan struct{})
			}
			var consumed atomic.Int64
			ex := scheduleExecutor{execute: func(ctx context.Context, i int, c engine.PlannedChunk) ([]byte, error) {
				if int64(i) > consumed.Load()+tc.w {
					t.Errorf("outside window: %d", i)
				}
				a.mu.Lock()
				slots := a.used.total[0]
				a.mu.Unlock()
				if slots > int64(tc.k) {
					t.Errorf("K exceeded: %d", slots)
				}
				started <- i
				select {
				case <-gates[i]:
					return []byte("abc"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}}
			go func() {
				done <- RunReadAhead(context.Background(), a, scheduleBinding(t, tc.w), budget(4096), p, tc.k, ex, func(ctx context.Context, i int, b []byte) error {
					if string(b) != "abc" {
						t.Errorf("bad output %q", b)
					}
					emitting <- i
					<-consume
					consumed.Add(1)
					return nil
				})
			}()
			seen := make(map[int]bool)
			for j := 0; j < limit; j++ {
				i := <-started
				if seen[i] || i >= limit {
					t.Fatalf("unexpected start %d", i)
				}
				seen[i] = true
			}
			// Later completions cannot slide the window or emit before occurrence 0.
			for j := limit - 1; j >= 1; j-- {
				close(gates[j])
			}
			a.mu.Lock()
			slots := a.used.total[0]
			a.mu.Unlock()
			if slots != int64(limit) {
				t.Fatal("released ahead of consumption")
			}
			select {
			case i := <-emitting:
				t.Fatalf("early emit %d", i)
			default:
			}
			close(gates[0])
			for i := 0; i < tc.n; i++ {
				if got := <-emitting; got != i {
					t.Fatalf("emit %d want %d", got, i)
				}
				a.mu.Lock()
				slots = a.used.total[0]
				a.mu.Unlock()
				if slots != int64(min(limit, tc.n-i)) {
					t.Fatalf("buffer accounting at %d: %d", i, slots)
				}
				consume <- struct{}{}
				if next := i + limit; next < tc.n {
					if got := <-started; got != next {
						t.Fatalf("start %d want %d", got, next)
					}
					seen[next] = true
					close(gates[next])
				}
			}
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			if len(seen) != tc.n {
				t.Fatal("lost repeated occurrence")
			}
			drained(t, a)
		})
	}
}
func TestReadAheadLogicalRangeAndEmpty(t *testing.T) {
	for _, span := range []*storage.ByteRange{nil, {Offset: 1, Length: 7}, {Offset: 3, Length: 3}} {
		a := admission(t, 100, 100)
		p := schedulePlan(t, 3, span)
		before := append([]engine.PlannedChunk(nil), p.Chunks...)
		var body []byte
		var calls atomic.Int64
		e := RunReadAhead(context.Background(), a, scheduleBinding(t, 4), budget(100), p, 3, scheduleExecutor{execute: func(_ context.Context, i int, c engine.PlannedChunk) ([]byte, error) {
			calls.Add(1)
			if c != p.Chunks[i] {
				t.Error("occurrence changed")
			}
			return []byte("abc"), nil
		}}, func(_ context.Context, _ int, b []byte) error { body = append(body, b...); return nil })
		if e != nil {
			t.Fatal(e)
		}
		want := "abcabcabc"
		want = want[p.Range.Offset : p.Range.Offset+p.Range.Length]
		if string(body) != want || calls.Load() != int64(len(p.Chunks)) || !reflect.DeepEqual(before, p.Chunks) {
			t.Fatal("range or ownership mismatch")
		}
		drained(t, a)
	}
	a := admission(t, 10, 10)
	e := RunReadAhead(context.Background(), a, scheduleBinding(t, 0), budget(10), schedulePlan(t, 0, nil), 1, scheduleExecutor{execute: func(context.Context, int, engine.PlannedChunk) ([]byte, error) {
		t.Error("empty fetch")
		return nil, nil
	}}, func(context.Context, int, []byte) error { t.Error("empty emit"); return nil })
	if e != nil {
		t.Fatal(e)
	}
	drained(t, a)
}
func TestReadAheadPrefetchExhaustionDemandProgress(t *testing.T) {
	a := admission(t, 10, 10)
	rb := budget(10)
	rb.Prefetch = Cost{}
	var calls atomic.Int64
	var emits int
	e := RunReadAhead(context.Background(), a, scheduleBinding(t, 4), rb, schedulePlan(t, 8, nil), 5, scheduleExecutor{execute: func(context.Context, int, engine.PlannedChunk) ([]byte, error) {
		calls.Add(1)
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.used.total[0] != 1 {
			t.Error("prefetch admitted")
		}
		return []byte("abc"), nil
	}}, func(context.Context, int, []byte) error { emits++; return nil })
	if e != nil || emits != 8 || calls.Load() != 8 {
		t.Fatalf("demand progress: %v %d %d", e, emits, calls.Load())
	}
	drained(t, a)
}
func TestReadAheadNoSkippingDeniedPrefetch(t *testing.T) {
	a := admission(t, 12, 12)
	var order []int
	var mu sync.Mutex
	ex := scheduleExecutor{estimate: func(i int, _ engine.PlannedChunk) (Cost, error) {
		c := Cost{ChunkSlots: 1, RawBytes: 3}
		if i == 1 {
			c.RawBytes = 12
		}
		return c, nil
	}, execute: func(_ context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
		mu.Lock()
		order = append(order, i)
		mu.Unlock()
		return []byte("abc"), nil
	}}
	e := RunReadAhead(context.Background(), a, scheduleBinding(t, 4), budget(12), schedulePlan(t, 3, nil), 3, ex, func(context.Context, int, []byte) error { return nil })
	if e != nil || !reflect.DeepEqual(order, []int{0, 1, 2}) {
		t.Fatalf("later speculation blocked demand: %v %v", order, e)
	}
	drained(t, a)
}
func TestReadAheadCancelJoinsWorkersBeforeRelease(t *testing.T) {
	for _, completed := range []bool{false, true} {
		t.Run(fmt.Sprintf("completed=%t", completed), func(t *testing.T) {
			a := admission(t, 100, 100)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan int, 2)
			prefetchReturned := make(chan struct{})
			stopped := make(chan struct{})
			exit := make(chan struct{})
			done := make(chan error, 1)
			ex := scheduleExecutor{execute: func(ctx context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
				started <- i
				if completed && i == 1 {
					defer close(prefetchReturned)
					return []byte("abc"), nil
				}
				<-ctx.Done()
				if i == 0 {
					close(stopped)
				}
				<-exit
				return nil, ctx.Err()
			}}
			go func() {
				done <- RunReadAhead(ctx, a, scheduleBinding(t, 1), budget(100), schedulePlan(t, 2, nil), 2, ex, func(context.Context, int, []byte) error { t.Error("emit after cancel"); return nil })
			}()
			<-started
			<-started
			if completed {
				<-prefetchReturned
			}
			cancel()
			<-stopped
			a.mu.Lock()
			slots := a.used.total[0]
			active := a.active
			a.mu.Unlock()
			if slots != 2 || active != 1 {
				t.Fatal("released before quiescence")
			}
			select {
			case <-done:
				t.Fatal("returned before join")
			default:
			}
			close(exit)
			if e := <-done; !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
			drained(t, a)
		})
	}
}
func TestReadAheadExecutorFailureCancelsOtherWorkers(t *testing.T) {
	for _, failIndex := range []int{0, 1} {
		t.Run(fmt.Sprint(failIndex), func(t *testing.T) {
			a := admission(t, 100, 100)
			started := make(chan int, 2)
			fail := make(chan struct{})
			observedCancel := make(chan struct{})
			exit := make(chan struct{})
			done := make(chan error, 1)
			bad := errors.New("verification failed")
			ex := scheduleExecutor{execute: func(ctx context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
				started <- i
				if i == failIndex {
					<-fail
					return []byte("bad"), bad
				}
				<-ctx.Done()
				close(observedCancel)
				<-exit
				return nil, ctx.Err()
			}}
			go func() {
				done <- RunReadAhead(context.Background(), a, scheduleBinding(t, 1), budget(100), schedulePlan(t, 2, nil), 2, ex, func(context.Context, int, []byte) error { t.Error("unverified emit"); return nil })
			}()
			<-started
			<-started
			close(fail)
			<-observedCancel
			a.mu.Lock()
			slots := a.used.total[0]
			a.mu.Unlock()
			if slots < 1 {
				t.Fatal("released live worker")
			}
			select {
			case <-done:
				t.Fatal("did not join")
			default:
			}
			close(exit)
			if e := <-done; !errors.Is(e, bad) {
				t.Fatal(e)
			}
			drained(t, a)
		})
	}
}
func TestReadAheadErrorsAndValidation(t *testing.T) {
	boom := errors.New("injected")
	for _, name := range []string{"cancel", "deadline", "estimate", "emit", "short", "oversized", "backing", "cost", "demand", "range", "overflow", "K", "binding"} {
		t.Run(name, func(t *testing.T) {
			a := admission(t, 100, 100)
			p := schedulePlan(t, 2, nil)
			b := scheduleBinding(t, 0)
			k := 1
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := error(ErrReadAheadInvalid)
			emits := 0
			ex := scheduleExecutor{execute: func(context.Context, int, engine.PlannedChunk) ([]byte, error) { return []byte("abc"), nil }}
			switch name {
			case "cancel":
				cancel()
				want = context.Canceled
			case "deadline":
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(ctx, time.Unix(0, 0))
				defer stop()
				want = context.DeadlineExceeded
			case "estimate":
				ex.estimate = func(int, engine.PlannedChunk) (Cost, error) { return Cost{}, boom }
				want = boom
			case "emit":
				want = boom
			case "short":
				ex.execute = func(context.Context, int, engine.PlannedChunk) ([]byte, error) { return []byte("a"), nil }
			case "oversized":
				ex.execute = func(context.Context, int, engine.PlannedChunk) ([]byte, error) { return []byte("abcd"), nil }
			case "backing":
				ex.execute = func(context.Context, int, engine.PlannedChunk) ([]byte, error) { return make([]byte, 3, 4), nil }
			case "cost":
				ex.estimate = func(int, engine.PlannedChunk) (Cost, error) { return Cost{ChunkSlots: 1, RawBytes: 2}, nil }
			case "demand":
				ex.estimate = func(int, engine.PlannedChunk) (Cost, error) { return Cost{ChunkSlots: 1, RawBytes: 101}, nil }
				want = ErrExhausted
			case "range":
				p.Chunks[1].Slice.Offset = 1
			case "overflow":
				p.Range.Offset = 1<<63 - 1
			case "K":
				k = 0
			case "binding":
				b.Name = "other"
			}
			e := RunReadAhead(ctx, a, b, budget(100), p, k, ex, func(context.Context, int, []byte) error {
				emits++
				if name == "emit" {
					return boom
				}
				return nil
			})
			if !errors.Is(e, want) {
				t.Fatalf("got %v want %v", e, want)
			}
			if name != "emit" && emits != 0 {
				t.Fatal("emitted on failure")
			}
			drained(t, a)
		})
	}
}

func TestReadAheadDistinctBytesOutOfOrder(t *testing.T) {
	a := admission(t, 100, 100)
	p := schedulePlan(t, 3, &storage.ByteRange{Offset: 1, Length: 7})
	data := []string{"abc", "def", "ghi"}
	for i := range p.Chunks {
		p.Chunks[i].Ref.Key.Hash = fmt.Sprintf("%x", sha256.Sum256([]byte(data[i])))
	}
	gates := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	returned := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}
	started := make(chan int, 3)
	done := make(chan error, 1)
	var body []byte
	ex := scheduleExecutor{execute: func(ctx context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
		defer close(returned[i])
		started <- i
		select {
		case <-gates[i]:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		// Dynamic string-to-byte conversion can round capacity above length.
		// Allocate the exact declared buffer budget for this positive fixture.
		raw := make([]byte, len(data[i]))
		copy(raw, data[i])
		return raw, nil
	}}
	go func() {
		done <- RunReadAhead(context.Background(), a, scheduleBinding(t, 4), budget(100), p, 3, ex, func(_ context.Context, _ int, b []byte) error { body = append(body, b...); return nil })
	}()
	for i := 0; i < 3; i++ {
		<-started
	}
	// Finish executor calls 2, then 1, then 0. No host scheduling assumption.
	for i := 2; i >= 0; i-- {
		close(gates[i])
		<-returned[i]
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if string(body) != "bcdefgh" {
		t.Fatalf("reordered or wrong slices: %q", body)
	}
	drained(t, a)
}
func TestReadAheadConcurrentRequests(t *testing.T) {
	a := admission(t, 100, 100)
	p := schedulePlan(t, 2, nil)
	b := scheduleBinding(t, 0)
	entered := make(chan struct{}, 2)
	resume := make(chan struct{})
	var wg sync.WaitGroup
	for j := 0; j < 2; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e := RunReadAhead(context.Background(), a, b, budget(100), p, 1, scheduleExecutor{execute: func(_ context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
				if i == 0 {
					entered <- struct{}{}
					<-resume
				}
				return []byte("abc"), nil
			}}, func(context.Context, int, []byte) error { return nil })
			if e != nil {
				t.Error(e)
			}
		}()
	}
	<-entered
	<-entered
	a.mu.Lock()
	active := a.active
	slots := a.used.total[0]
	buckets := len(a.buckets)
	a.mu.Unlock()
	if active != 2 || slots != 2 || buckets != 1 {
		t.Fatal("concurrent bucket accounting")
	}
	close(resume)
	wg.Wait()
	drained(t, a)
}
