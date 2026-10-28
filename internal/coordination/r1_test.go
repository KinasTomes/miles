package coordination

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

func r1Fixture(t *testing.T) (*clock.Virtual, *WarmGate, *WriterRegistry, storage.ObjectKey, storage.PackRef) {
	t.Helper()
	c, err := clock.NewVirtual(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), 100)
	if err != nil {
		t.Fatal(err)
	}
	l := LeaseLimits{Lifetime: 60 * time.Second, Heartbeat: 15 * time.Second}
	g, err := NewR1Gate(c, "service-1", l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewWriterRegistry(g, l, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	return c, g, r, storage.ObjectKey{Bucket: "demo", BucketID: "bucket-1", Key: "file"}, storage.PackRef{ID: "pack", Generation: "1"}
}
func readView(t *testing.T, c *clock.Virtual, g *WarmGate, o storage.ObjectKey, p storage.PackRef) *WarmView {
	t.Helper()
	ctx := context.Background()
	d, _ := c.Now().Instant.Add(2 * time.Minute)
	v, err := g.Acquire(ctx, o, d)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.Bind(ctx, storage.ObjectHead{Object: o, ManifestID: "root", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err = v.Pin(ctx, []storage.PackRef{p, p}); err != nil {
		t.Fatal(err)
	}
	if err = v.Resolved(ctx); err != nil {
		t.Fatal(err)
	}
	return v
}
func TestR1ReadLeaseHeartbeatAndHardDeadline(t *testing.T) {
	for _, heartbeat := range []bool{false, true} {
		t.Run(map[bool]string{false: "lease equality", true: "hard deadline equality"}[heartbeat], func(t *testing.T) {
			c, g, _, o, p := r1Fixture(t)
			v := readView(t, c, g, o, p)
			defer v.Release()
			if heartbeat {
				for i := 0; i < 7; i++ {
					if err := c.Advance(15 * time.Second); err != nil {
						t.Fatal(err)
					}
					if err := v.Heartbeat(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				if err := c.Advance(15 * time.Second); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := c.Advance(time.Minute); err != nil {
					t.Fatal(err)
				}
			}
			if !errors.Is(v.Check(context.Background()), storage.ErrFenced) {
				t.Fatal("equality granted execution")
			}
			if !errors.Is(v.Heartbeat(context.Background()), storage.ErrFenced) {
				t.Fatal("late heartbeat resurrected view")
			}
			if !errors.Is(g.Retire(p), storage.ErrConflict) {
				t.Fatal("lease loss unpinned live view")
			}
		})
	}
}
func TestR1ReleaseWaitsForWorkAndClosesAttachment(t *testing.T) {
	c, g, _, o, p := r1Fixture(t)
	v := readView(t, c, g, o, p)
	w, err := v.BeginWork(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	v.Release()
	if !errors.Is(g.Retire(p), storage.ErrConflict) {
		t.Fatal("released while work owns bytes")
	}
	if _, err := v.BeginWork(context.Background()); !errors.Is(err, storage.ErrFenced) {
		t.Fatal(err)
	}
	copy := *w
	copy.Finish()
	w.Finish()
	v.Release()
	if err := g.Retire(p); err != nil {
		t.Fatal(err)
	}
	if len(g.pins) != 0 || g.resolving != 0 {
		t.Fatal("double cleanup")
	}
}
func TestR1InvalidationKeepsReaderProtectionUntilQuiescent(t *testing.T) {
	c, g, r, o, p := r1Fixture(t)
	v := readView(t, c, g, o, p)
	w, err := v.BeginWork(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r.Invalidate()
	v.Release()
	if !errors.Is(v.Check(context.Background()), storage.ErrFenced) {
		t.Fatal("old reader alive")
	}
	if !errors.Is(g.Retire(p), storage.ErrConflict) {
		t.Fatal("invalidation released pin")
	}
	w.Finish()
	if err := g.Retire(p); err != nil {
		t.Fatal(err)
	}
	d, _ := c.Now().Instant.Add(time.Minute)
	if _, err := g.Acquire(context.Background(), o, d); !errors.Is(err, storage.ErrFenced) {
		t.Fatal(err)
	}
}
func registerWriter(t *testing.T, c *clock.Virtual, r *WriterRegistry) *Writer {
	t.Helper()
	d, _ := c.Now().Instant.Add(2 * time.Minute)
	w, err := r.Register(context.Background(), storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "service-1", Epoch: 1}, d)
	if err != nil {
		t.Fatal(err)
	}
	return w
}
func TestR1TwoWorkersClosureOrder(t *testing.T) {
	for _, closeFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "closure before dispatch", false: "dispatched call settles before closure"}[closeFirst], func(t *testing.T) {
			c, _, r, _, _ := r1Fixture(t)
			w := registerWriter(t, c, r)
			entered, proceed := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			if closeFirst {
				r.hooks.Before = func(context.Context, string, storage.WriterFence) error { close(entered); <-proceed; return nil }
			}
			done := make(chan error, 1)
			go func() {
				done <- w.Do(context.Background(), "commit/test", func(storage.WriterFence) error {
					calls.Add(1)
					if !closeFirst {
						close(entered)
						<-proceed
					}
					return nil
				})
			}()
			<-entered
			if closeFirst {
				w.CloseFence()
				close(proceed)
				if err := <-done; !errors.Is(err, storage.ErrFenced) {
					t.Fatal(err)
				}
				if calls.Load() != 0 {
					t.Fatal("callback after closure")
				}
			} else {
				// The call was already dispatched. Advancing past E cannot undo
				// its outcome; closure waits for synchronous adapter settlement.
				if err := c.Advance(time.Minute); err != nil {
					t.Fatal(err)
				}
				closed := make(chan struct{})
				go func() { w.CloseFence(); close(closed) }()
				close(proceed)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				<-closed
				if calls.Load() != 1 {
					t.Fatal("lost committed callback")
				}
			}
			r.hooks = Hooks{}
			if err := w.Do(context.Background(), "next", func(storage.WriterFence) error { t.Error("stale writer executed"); return nil }); !errors.Is(err, storage.ErrFenced) {
				t.Fatal(err)
			}
			if err := w.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestR1UnknownOutcomeBlocksReleaseAndTakeover(t *testing.T) {
	c, g, r, _, p := r1Fixture(t)
	w := registerWriter(t, c, r)
	if err := w.Pin(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := w.Do(context.Background(), "commit/op", func(storage.WriterFence) error { return storage.ErrUnavailable }); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	w.CloseFence()
	if !errors.Is(w.Release(), ErrReconcilePending) {
		t.Fatal("unknown released")
	}
	if !errors.Is(g.Retire(p), storage.ErrConflict) {
		t.Fatal("unknown lost protection")
	}
	d, _ := c.Now().Instant.Add(time.Minute)
	next := w.Fence()
	next.Epoch++
	if _, err := r.Register(context.Background(), next, d); !errors.Is(err, ErrReconcilePending) {
		t.Fatal("overlapping authority", err)
	}
}
func TestR1AfterACKWindowRetainsProtection(t *testing.T) {
	c, g, r, _, p := r1Fixture(t)
	w := registerWriter(t, c, r)
	if err := w.Pin(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	entered, proceed := make(chan struct{}), make(chan struct{})
	r.hooks.After = func(context.Context, string, storage.WriterFence) error {
		close(entered)
		<-proceed
		return errors.New("lost ack")
	}
	done := make(chan error, 1)
	go func() {
		done <- w.Do(context.Background(), "commit/op", func(storage.WriterFence) error { return nil })
	}()
	<-entered
	if !errors.Is(w.Release(), ErrWorkPending) {
		t.Fatal("ACK window released")
	}
	if !errors.Is(g.Retire(p), storage.ErrConflict) {
		t.Fatal("ACK window unpinned")
	}
	close(proceed)
	if err := <-done; !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	if !errors.Is(w.Release(), ErrReconcilePending) {
		t.Fatal("lost ACK dropped uncertainty")
	}
}

// Registration acknowledgement for Sleep allows tests to advance virtual time
// only after the timer exists. Host scheduling cannot decide a lease outcome.
type signaledClock struct {
	*clock.Virtual
	registered chan struct{}
}

func (c signaledClock) Sleep(ctx context.Context, d time.Duration) error {
	wake := make(chan struct{})
	e, err := c.AfterFunc(d, func() { close(wake) })
	if err != nil {
		return err
	}
	defer e.Cancel()
	c.registered <- struct{}{}
	select {
	case <-wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestR1HeartbeatLoopUsesInjectedClockAndJoins(t *testing.T) {
	v, err := clock.NewVirtual(time.Now().UTC(), 100)
	if err != nil {
		t.Fatal(err)
	}
	c := signaledClock{Virtual: v, registered: make(chan struct{}, 1)}
	g, err := NewR1Gate(c, "incarnation", LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	d, _ := v.Now().Instant.Add(2 * time.Minute)
	h, err := g.Acquire(context.Background(), storage.ObjectKey{Bucket: "demo", BucketID: "b", Key: "k"}, d)
	if err != nil {
		t.Fatal(err)
	}
	defer h.Release()
	_, stop, err := h.KeepAlive(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		<-c.registered
		if err := v.Advance(15 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	<-c.registered // next timer registration proves previous renewal finished
	if err := h.Check(context.Background()); err != nil {
		t.Fatal("heartbeat did not extend live lease", err)
	}
	stop()
	if v.Pending() != 0 {
		t.Fatal("heartbeat timer leaked")
	}
}
func TestR1WriterLeaseExpiryAndDomainChecks(t *testing.T) {
	c, _, r, _, _ := r1Fixture(t)
	w := registerWriter(t, c, r)
	if err := c.Advance(time.Minute); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(w.Heartbeat(context.Background()), storage.ErrFenced) {
		t.Fatal("late writer renewal resurrected")
	}
	if err := w.Do(context.Background(), "publish", func(storage.WriterFence) error { t.Error("expired worker published"); return nil }); !errors.Is(err, storage.ErrFenced) {
		t.Fatal(err)
	}
	if err := w.Release(); err != nil {
		t.Fatal(err)
	}
	other, err := clock.NewVirtual(time.Now().UTC(), 10)
	if err != nil {
		t.Fatal(err)
	}
	f := w.Fence()
	f.LeaseID = "other"
	if _, err := r.Register(context.Background(), f, other.Now().Instant); !errors.Is(err, clock.ErrDomain) {
		t.Fatal(err)
	}
	f.ServiceIncarnation = "old"
	if _, err := r.Register(context.Background(), f, c.Now().Instant); !errors.Is(err, storage.ErrInvalid) {
		t.Fatal(err)
	}
}

func TestR1RegistrationLostACKRetriesSameHandle(t *testing.T) {
	c, g, r, _, p := r1Fixture(t)
	once := true
	r.hooks.After = func(_ context.Context, stage string, _ storage.WriterFence) error {
		if stage == "register" && once {
			once = false
			return errors.New("lost registration ACK")
		}
		return nil
	}
	d, _ := c.Now().Instant.Add(time.Minute)
	f := storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "service-1", Epoch: 1}
	if _, err := r.Register(context.Background(), f, d); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	if !errors.Is(g.Retire(p), storage.ErrConflict) {
		t.Fatal("lost registration lost guard")
	}
	w, err := r.Register(context.Background(), f, d)
	if err != nil {
		t.Fatal(err)
	}
	if g.resolving != 1 || len(r.writers) != 1 {
		t.Fatal("registration retry duplicated protection")
	}
	if err := w.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Register(context.Background(), f, d); !errors.Is(err, storage.ErrFenced) {
		t.Fatal("terminal ID resurrected", err)
	}
}
func TestR1DispatchedCancellationRetainsUnknown(t *testing.T) {
	for _, failure := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(failure.Error(), func(t *testing.T) {
			c, g, r, _, p := r1Fixture(t)
			w := registerWriter(t, c, r)
			if err := w.Do(context.Background(), "commit/op", func(storage.WriterFence) error { return failure }); !errors.Is(err, storage.ErrUnavailable) || !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if !errors.Is(w.Release(), ErrReconcilePending) {
				t.Fatal("timeout became rollback proof")
			}
			if !errors.Is(g.Retire(p), storage.ErrConflict) {
				t.Fatal("timeout released guard")
			}
		})
	}
}
func TestR1WriterHeartbeatCannotMoveRequestDeadline(t *testing.T) {
	c, _, r, _, _ := r1Fixture(t)
	w := registerWriter(t, c, r)
	if err := c.Advance(45 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := w.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Advance(30 * time.Second); err != nil {
		t.Fatal(err)
	}
	if err := w.Heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Advance(45 * time.Second); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(w.Check(context.Background()), storage.ErrFenced) {
		t.Fatal("heartbeat extended hard deadline")
	}
	if err := w.Release(); err != nil {
		t.Fatal(err)
	}
}

type stallClock struct {
	*clock.Virtual
	remaining int
}

func (c *stallClock) Now() clock.Reading {
	now := c.Virtual.Now()
	if c.remaining > 0 {
		c.remaining--
		if c.remaining == 0 {
			if err := c.Virtual.Advance(2 * time.Second); err != nil {
				panic(err)
			}
		}
	}
	return now
}
func TestR1RenewalDoesNotExtendFromPostValidationStall(t *testing.T) {
	for _, writer := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "writer"}[writer], func(t *testing.T) {
			base, err := clock.NewVirtual(time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC), 100)
			if err != nil {
				t.Fatal(err)
			}
			c := &stallClock{Virtual: base}
			limits := LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
			g, err := NewR1Gate(c, "incarnation", limits)
			if err != nil {
				t.Fatal(err)
			}
			d, _ := base.Now().Instant.Add(2 * time.Minute)
			var renew func(context.Context) error
			if writer {
				r, err := NewWriterRegistry(g, limits, Hooks{})
				if err != nil {
					t.Fatal(err)
				}
				w, err := r.Register(context.Background(), storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "incarnation", Epoch: 1}, d)
				if err != nil {
					t.Fatal(err)
				}
				defer w.Release()
				renew = w.Heartbeat
			} else {
				v, err := g.Acquire(context.Background(), storage.ObjectKey{Bucket: "demo", BucketID: "b", Key: "k"}, d)
				if err != nil {
					t.Fatal(err)
				}
				defer v.Release()
				renew = v.Heartbeat
			}
			if err := base.Advance(59 * time.Second); err != nil {
				t.Fatal(err)
			}
			// A stall after the second sample crosses old E. Sampling a new
			// renewal end AFTER the validity check would resurrect the budget.
			c.remaining = 2
			if err := renew(context.Background()); !errors.Is(err, storage.ErrFenced) {
				t.Fatal("post-check stall granted a fresh budget", err)
			}
		})
	}
}

func TestR1WriterOperationWorkRetainsProtection(t *testing.T) {
	c, g, r, o, p := r1Fixture(t)
	w := registerWriter(t, c, r)
	work, err := w.BeginOperation(context.Background(), o, "op")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Pin(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if _, err := w.BeginOperation(context.Background(), o, "op"); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("duplicate executor", err)
	}
	if !errors.Is(w.Release(), ErrWorkPending) {
		t.Fatal("released CPU/body work")
	}
	if !errors.Is(g.Retire(p), storage.ErrConflict) {
		t.Fatal("live work lost pin")
	}
	copy := *work
	copy.Finish()
	work.Finish()
	if err := w.Release(); err != nil {
		t.Fatal(err)
	}
	if err := g.Retire(p); err != nil {
		t.Fatal(err)
	}
}
func TestR1AdapterOrACKPanicRetainsUnknownWithoutPoisonedLock(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "adapter", true: "ACK hook"}[after], func(t *testing.T) {
			c, g, r, _, p := r1Fixture(t)
			w := registerWriter(t, c, r)
			if after {
				r.hooks.After = func(context.Context, string, storage.WriterFence) error { panic("ACK panic") }
			}
			var caught any
			func() {
				defer func() { caught = recover() }()
				_ = w.Do(context.Background(), "commit/op", func(storage.WriterFence) error {
					if !after {
						panic("adapter panic")
					}
					return nil
				})
			}()
			if caught == nil {
				t.Fatal("panic unexpectedly swallowed")
			}
			if !errors.Is(w.Release(), ErrReconcilePending) {
				t.Fatal("panic lost original unknown outcome")
			}
			if !errors.Is(g.Retire(p), storage.ErrConflict) {
				t.Fatal("panic released admission")
			}
		})
	}
}
