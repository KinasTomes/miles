package readpath

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/storage"
)

func installR1Gate(t *testing.T, f *warmFixture) *coordination.WarmGate {
	t.Helper()
	return installR1GateClock(t, f, f.clock)
}
func installR1GateClock(t *testing.T, f *warmFixture, c clock.Clock) *coordination.WarmGate {
	t.Helper()
	g, err := coordination.NewR1Gate(c, "service-reader", coordination.LeaseLimits{Lifetime: 30 * time.Second, Heartbeat: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	f.reader.options.Gate = g
	return g
}
func TestWarmR1IncarnationLossStopsNextEmission(t *testing.T) {
	f := newWarmFixture(t, 0)
	g := installR1Gate(t, f)
	var body []byte
	_, err := f.reader.Read(context.Background(), f.request, func(_ context.Context, b []byte) error {
		body = append(body, b...)
		g.Invalidate()
		if !errors.Is(g.Retire(f.ref), storage.ErrConflict) {
			t.Fatal("pin released while emitter owns bytes")
		}
		return nil
	})
	if err == nil || string(body) != string(f.first) {
		t.Fatal("old incarnation continued stream", len(body), err)
	}
	drained(t, f.reader.options.Admission)
	if err := g.Retire(f.ref); err != nil {
		t.Fatal(err)
	}
}
func TestWarmR1InvalidationDuringBackendWaitRetainsPins(t *testing.T) {
	f := newWarmFixture(t, 0)
	g := installR1Gate(t, f)
	f.backend.entered = make(chan struct{}, 1)
	f.backend.unblock = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, body, err := f.read(context.Background(), f.request)
		if len(body) != 0 {
			err = errors.New("lost view emitted bytes")
		}
		done <- err
	}()
	<-f.backend.entered
	g.Invalidate()
	if !errors.Is(g.Retire(f.ref), storage.ErrConflict) {
		t.Fatal("invalidation proved quiescence")
	}
	close(f.backend.unblock)
	if err := <-done; err == nil {
		t.Fatal("lost view returned success")
	}
	drained(t, f.reader.options.Admission)
	if err := g.Retire(f.ref); err != nil {
		t.Fatal(err)
	}
}

// Heartbeat wakeups must be explicit: Virtual.Advance visits intermediate timer
// instants, but goroutines woken by Sleep have no deterministic execution order.
// This clock preserves the fixture's Instant domain and lets the test choose
// whether a renewal happens before advancing to the lease equality boundary.
type warmHeartbeatSleep struct {
	interval time.Duration
	resume   chan struct{}
}
type controlledWarmHeartbeatClock struct {
	*clock.Virtual
	sleeps chan warmHeartbeatSleep
}

func (c *controlledWarmHeartbeatClock) Sleep(ctx context.Context, d time.Duration) error {
	sleep := warmHeartbeatSleep{interval: d, resume: make(chan struct{})}
	select {
	case c.sleeps <- sleep:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-sleep.resume:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestWarmR1LeaseEqualityStopsNextChunk(t *testing.T) {
	for _, renewed := range []bool{false, true} {
		name := "without-renewal"
		if renewed {
			name = "after-renewal"
		}
		t.Run(name, func(t *testing.T) {
			f := newWarmFixture(t, 0)
			c := &controlledWarmHeartbeatClock{Virtual: f.clock, sleeps: make(chan warmHeartbeatSleep, 1)}
			g := installR1GateClock(t, f, c)
			var body []byte
			emissions := 0
			_, err := f.reader.Read(context.Background(), f.request, func(_ context.Context, b []byte) error {
				body = append(body, b...)
				emissions++
				if emissions == 1 {
					sleep := <-c.sleeps // registration proves the heartbeat is held
					if sleep.interval != 5*time.Second {
						t.Fatalf("heartbeat interval %s", sleep.interval)
					}
					if !renewed {
						return f.clock.Advance(30 * time.Second)
					}
					if err := f.clock.Advance(5 * time.Second); err != nil {
						return err
					}
					close(sleep.resume)
					<-c.sleeps                               // next Sleep proves renewal finished: lease now ends at t=35
					return f.clock.Advance(25 * time.Second) // original expiry t=30 is still live
				}
				return f.clock.Advance(5 * time.Second) // renewed lease equality t=35
			})
			want := f.first
			if renewed {
				want = f.body[:len(f.body)-len(f.first)]
			}
			if !errors.Is(err, storage.ErrFenced) || string(body) != string(want) {
				t.Fatal("lease equality continued streaming", len(body), err)
			}
			drained(t, f.reader.options.Admission)
			if err := g.Retire(f.ref); err != nil {
				t.Fatal(err)
			}
			if f.clock.Pending() != 0 {
				t.Fatal("heartbeat sleep leaked")
			}
		})
	}
}
