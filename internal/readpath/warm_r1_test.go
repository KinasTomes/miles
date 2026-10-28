package readpath

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/storage"
)

func installR1Gate(t *testing.T, f *warmFixture) *coordination.WarmGate {
	t.Helper()
	g, err := coordination.NewR1Gate(f.clock, "service-reader", coordination.LeaseLimits{Lifetime: 30 * time.Second, Heartbeat: 5 * time.Second})
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

func TestWarmR1LeaseEqualityStopsNextChunk(t *testing.T) {
	f := newWarmFixture(t, 0)
	g := installR1Gate(t, f)
	var body []byte
	_, err := f.reader.Read(context.Background(), f.request, func(_ context.Context, b []byte) error {
		body = append(body, b...)
		return f.clock.Advance(30 * time.Second)
	})
	if err == nil || string(body) != string(f.first) {
		t.Fatal("lease equality continued streaming", len(body), err)
	}
	drained(t, f.reader.options.Admission)
	if err := g.Retire(f.ref); err != nil {
		t.Fatal(err)
	}
	if f.clock.Pending() != 0 {
		t.Fatal("heartbeat sleep leaked")
	}
}
