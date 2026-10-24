package readpath

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/KinasTomes/miles/internal/config"
)

func all(n int64) Cost      { return Cost{n, n, n, n, n, n, n} }
func budget(n int64) Budget { return Budget{Capacity: all(n), Prefetch: all(n / 2)} }
func binding(t *testing.T, id string, rev uint64) BucketBinding {
	t.Helper()
	v, e := config.NewPolicyValidator(config.DSGPolicyLimits())
	if e != nil {
		t.Fatal(e)
	}
	p, e := v.NewBucketSnapshot(id, rev, config.DefaultCachePolicy())
	if e != nil {
		t.Fatal(e)
	}
	return BucketBinding{Name: "same-name", Policy: p}
}
func admission(t *testing.T, node, bucketCap int64) *Admission {
	t.Helper()
	a, e := NewAdmission(Limits{Node: ScopeLimits{100, budget(node)}, Bucket: ScopeLimits{100, budget(bucketCap)}})
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func ticket(t *testing.T, a *Admission, id string, rev uint64, cap int64) *RequestTicket {
	t.Helper()
	r, e := a.AdmitRequest(context.Background(), binding(t, id, rev), budget(cap))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func grant(t *testing.T, a *Admission, r *RequestTicket, c Cost) *Reservation {
	t.Helper()
	v, e := a.ReserveDemand(context.Background(), r, c)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func drained(t *testing.T, a *Admission) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.active != 0 || len(a.buckets) != 0 || a.used != (usage{}) {
		t.Fatalf("leaked accounting: active=%d buckets=%d usage=%+v", a.active, len(a.buckets), a.used)
	}
}
func TestAdmissionScopesAndAtomicFailure(t *testing.T) {
	for _, scope := range []string{"node", "bucket", "request"} {
		t.Run(scope, func(t *testing.T) {
			n, b, r := int64(100), int64(100), int64(100)
			switch scope {
			case "node":
				n = 4
			case "bucket":
				b = 4
			case "request":
				r = 4
			}
			a := admission(t, n, b)
			q := ticket(t, a, "id", 1, r)
			x := grant(t, a, q, all(3))
			before := a.used
			_, err := a.ReserveDemand(context.Background(), q, Cost{ChunkSlots: 1, CopyBytes: 2})
			var ex *ExhaustionError
			if !errors.As(err, &ex) || ex.Scope != scope || ex.Resource != "copy" || ex.Impossible {
				t.Fatalf("wrong exhaustion: %v", err)
			}
			if a.used != before || q.state.used != before || q.state.bucket.used != before {
				t.Fatal("partial grant")
			}
			x.Release()
			q.Close()
			drained(t, a)
		})
	}
}
func TestAdmissionIdentityAndActiveLimits(t *testing.T) {
	a := admission(t, 10, 10)
	a.limits.Node.ActiveRequests = 3
	a.limits.Bucket.ActiveRequests = 1
	q := ticket(t, a, "old", 1, 10)
	if _, e := a.AdmitRequest(context.Background(), binding(t, "old", 2), budget(10)); !errors.Is(e, ErrExhausted) {
		t.Fatalf("revision created pool: %v", e)
	}
	r := ticket(t, a, "new", 1, 10) // Same name, new incarnation, independent bucket.
	s := ticket(t, a, "third", 1, 10)
	if _, e := a.AdmitRequest(context.Background(), binding(t, "fourth", 1), budget(10)); !errors.Is(e, ErrExhausted) {
		t.Fatal(e)
	}
	x := grant(t, a, q, all(6))
	if _, e := a.ReserveDemand(context.Background(), r, all(5)); !errors.Is(e, ErrExhausted) {
		t.Fatal("node aggregate bypass")
	}
	x.Release()
	q.Close()
	r.Close()
	s.Close()
	drained(t, a)
}
func TestAdmissionSameBucketRevisionsShareResources(t *testing.T) {
	a := admission(t, 20, 6)
	q := ticket(t, a, "id", 1, 10)
	r := ticket(t, a, "id", 2, 10)
	x := grant(t, a, q, all(4))
	if _, e := a.ReserveDemand(context.Background(), r, all(3)); !errors.Is(e, ErrExhausted) {
		t.Fatal("bucket aggregate bypass")
	}
	x.Release()
	y := grant(t, a, r, all(6))
	y.Release()
	q.Close()
	r.Close()
	drained(t, a)
}
func TestAdmissionPrefetchHeadroom(t *testing.T) {
	a := admission(t, 10, 10)
	q := ticket(t, a, "id", 1, 10)
	p, e := a.TryReservePrefetch(q, all(5))
	if e != nil {
		t.Fatal(e)
	}
	if _, e = a.TryReservePrefetch(q, all(1)); !errors.Is(e, ErrExhausted) {
		t.Fatal("prefetch did not skip")
	}
	d := grant(t, a, q, all(5))
	d.Release()
	p.Release()
	q.Close()
	drained(t, a)
}
func TestAdmissionOverflowAndImpossible(t *testing.T) {
	a := admission(t, math.MaxInt64, math.MaxInt64)
	q := ticket(t, a, "id", 1, math.MaxInt64)
	r := grant(t, a, q, all(math.MaxInt64))
	if _, e := a.ReserveDemand(context.Background(), q, all(1)); !errors.Is(e, ErrExhausted) {
		t.Fatal("overflow accepted")
	}
	r.Release()
	q.Close()
	drained(t, a)
	a = admission(t, 10, 10)
	q = ticket(t, a, "id", 1, 10)
	_, e := a.ReserveDemand(context.Background(), q, all(11))
	var ex *ExhaustionError
	if !errors.As(e, &ex) || !ex.Impossible {
		t.Fatal(e)
	}
	q.Close()
	drained(t, a)
}
func TestAdmissionInvalidLimitsAndCosts(t *testing.T) {
	for i := 0; i < 7; i++ {
		for _, n := range []int64{0, -1} {
			b := budget(10)
			p := []*int64{&b.Capacity.ChunkSlots, &b.Capacity.StoredBytes, &b.Capacity.IOSlots, &b.Capacity.IOBytes, &b.Capacity.RawBytes, &b.Capacity.WorkspaceBytes, &b.Capacity.CopyBytes}
			*p[i] = n
			for _, node := range []bool{true, false} {
				l := Limits{ScopeLimits{1, budget(10)}, ScopeLimits{1, budget(10)}}
				if node {
					l.Node.Budget = b
				} else {
					l.Bucket.Budget = b
				}
				if _, e := NewAdmission(l); !errors.Is(e, ErrAdmissionInvalid) {
					t.Fatal("invalid cap accepted")
				}
			}
		}
	}
	for _, b := range []Budget{{}, {all(10), all(10)}, {all(10), all(-1)}} {
		if _, e := NewAdmission(Limits{ScopeLimits{1, b}, ScopeLimits{1, budget(10)}}); !errors.Is(e, ErrAdmissionInvalid) {
			t.Fatal(e)
		}
	}
	if _, e := NewAdmission(Limits{}); !errors.Is(e, ErrAdmissionInvalid) {
		t.Fatal(e)
	}
	a := admission(t, 10, 10)
	q := ticket(t, a, "id", 1, 10)
	for _, c := range []Cost{{}, {ChunkSlots: -1}, {ChunkSlots: 1, RawBytes: -1}} {
		if _, e := a.ReserveDemand(context.Background(), q, c); !errors.Is(e, ErrAdmissionInvalid) {
			t.Fatal(e)
		}
	}
	if _, e := a.AdmitRequest(context.Background(), binding(t, "id", 1), Budget{}); !errors.Is(e, ErrAdmissionInvalid) {
		t.Fatal(e)
	}
	if _, e := a.AdmitRequest(context.Background(), BucketBinding{}, budget(10)); !errors.Is(e, ErrAdmissionInvalid) {
		t.Fatal(e)
	}
	foreign := admission(t, 10, 10)
	if _, e := foreign.ReserveDemand(context.Background(), q, all(1)); !errors.Is(e, ErrAdmissionInvalid) {
		t.Fatal(e)
	}
	q.Close()
	drained(t, a)
}
func TestAdmissionCancellationAndCloseDrain(t *testing.T) {
	a := admission(t, 10, 10)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := a.AdmitRequest(ctx, binding(t, "id", 1), budget(10)); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	drained(t, a)
	ctx, cancel = context.WithCancel(context.Background())
	q, e := a.AdmitRequest(ctx, binding(t, "id", 1), budget(10))
	if e != nil {
		t.Fatal(e)
	}
	x := grant(t, a, q, all(2))
	cancel()
	if _, e := a.TryReservePrefetch(q, all(1)); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if q.Close() || a.active != 1 || a.used.total != all(2).values() {
		t.Fatal("close released live work")
	}
	copyX := *x
	copyQ := *q
	x.Release()
	copyX.Release()
	if !copyQ.Close() {
		t.Fatal("not drained")
	}
	drained(t, a)
	q = ticket(t, a, "id", 1, 10)
	if _, e := a.ReserveDemand(ctx, q, all(1)); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	q.Close()
	if _, e := a.ReserveDemand(context.Background(), q, all(1)); !errors.Is(e, ErrTicketClosed) {
		t.Fatal(e)
	}
	drained(t, a)
}
func TestAdmissionConcurrentReserveReleaseAndClose(t *testing.T) {
	a := admission(t, 64, 64)
	q := ticket(t, a, "id", 1, 64)
	start := make(chan struct{})
	release := make(chan struct{})
	ready := make(chan struct{}, 32)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			r, e := a.ReserveDemand(context.Background(), q, all(1))
			if e != nil {
				t.Error(e)
				ready <- struct{}{}
				return
			}
			ready <- struct{}{}
			<-release
			r.Release()
			r.Release()
		}()
	}
	close(start)
	for i := 0; i < 32; i++ {
		<-ready
	}
	if q.Close() {
		t.Fatal("premature drain")
	}
	close(release)
	wg.Wait()
	if !q.Close() {
		t.Fatal("not drained")
	}
	drained(t, a)
}
func TestAdmissionCloseVersusGrant(t *testing.T) {
	for i := 0; i < 50; i++ {
		a := admission(t, 10, 10)
		q := ticket(t, a, "id", 1, 10)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r, e := a.ReserveDemand(context.Background(), q, all(1))
			if e == nil {
				r.Release()
			} else if !errors.Is(e, ErrTicketClosed) {
				t.Error(e)
			}
		}()
		go func() { defer wg.Done(); <-start; q.Close() }()
		close(start)
		wg.Wait()
		q.Close()
		drained(t, a)
	}
}

func TestAdmissionEveryDimension(t *testing.T) {
	for i, name := range resourceNames {
		t.Run(name, func(t *testing.T) {
			a := admission(t, 10, 10)
			q := ticket(t, a, "id", 1, 10)
			x := grant(t, a, q, all(9))
			c := Cost{ChunkSlots: 1}
			fields := []*int64{&c.ChunkSlots, &c.StoredBytes, &c.IOSlots, &c.IOBytes, &c.RawBytes, &c.WorkspaceBytes, &c.CopyBytes}
			*fields[i] = 2
			before := a.used
			_, err := a.ReserveDemand(context.Background(), q, c)
			var ex *ExhaustionError
			if !errors.As(err, &ex) || ex.Resource != name || a.used != before {
				t.Fatalf("dimension bypass or partial grant: %v", err)
			}
			x.Release()
			q.Close()
			drained(t, a)
		})
	}
}

func TestAdmissionConcurrentDoubleRelease(t *testing.T) {
	a := admission(t, 10, 10)
	q := ticket(t, a, "id", 1, 10)
	r := grant(t, a, q, all(10))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; r.Release(); q.Close() }()
	}
	close(start)
	wg.Wait()
	drained(t, a)
}
