package fake

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFaultWindows(t *testing.T) {
	crash := errors.New("injected crash")
	for _, p := range []Point{Registration, S3Write, Publication, EpochClose, SealPublish, PinAttach, ReadRelease} {
		for _, phase := range []Phase{Before, After} {
			t.Run(string(p)+"/"+string(phase), func(t *testing.T) {
				applied := 0
				f := Faults{Hook: func(_ context.Context, got Point, at Phase) error {
					if got != p {
						t.Fatal(got)
					}
					if at == phase {
						return crash
					}
					return nil
				}}
				e := f.Run(context.Background(), p, func() error { applied++; return nil })
				want(t, e, crash)
				if phase == Before {
					if applied != 0 || errors.Is(e, ErrUnknown) {
						t.Fatal(applied, e)
					}
				} else {
					if applied != 1 {
						t.Fatal(applied)
					}
					want(t, e, ErrUnknown)
				}
			})
		}
	}
}
func TestLostCommitACKReconcilesOriginal(t *testing.T) {
	_, c, _, _, _ := fixture(t)
	m, e := NewProducers(c)
	must(t, e)
	d, _ := c.Now().Instant.Add(time.Minute)
	p, e := m.Register("p", d)
	must(t, e)
	x, e := m.Dispatch(p, "m", "input")
	must(t, e)
	f := Faults{Hook: func(_ context.Context, _ Point, phase Phase) error {
		if phase == After {
			return context.Canceled
		}
		return nil
	}}
	want(t, f.Run(context.Background(), Publication, func() error { return m.Commit(x) }), ErrUnknown)
	must(t, m.CloseProducer(p))
	out, e := m.Reconcile(x)
	must(t, e)
	if out != Committed {
		t.Fatal(out)
	}
}
func TestLostPinAndReleaseACK(t *testing.T) {
	m, c, h, x, _ := fixture(t)
	v := acquire(t, m, c, h, "v")
	must(t, m.BindRoot(v, 1, "m1"))
	f := Faults{Hook: func(_ context.Context, _ Point, p Phase) error {
		if p == After {
			return context.Canceled
		}
		return nil
	}}
	attach := func() error { return m.Pin(v, "batch", []Location{{x.Key, 1, x.Location.Pack}}) }
	want(t, f.Run(context.Background(), PinAttach, attach), ErrUnknown)
	want(t, m.Retire(x.Location.Pack), ErrBusy)
	must(t, attach()) // exact receipt replay resolves the unknown, not a new batch
	must(t, m.BeginRelease(v))
	want(t, f.Run(context.Background(), ReadRelease, func() error { return m.FinishRelease(v) }), ErrUnknown)
	must(t, m.FinishRelease(v))
	must(t, m.Retire(x.Location.Pack))
}
