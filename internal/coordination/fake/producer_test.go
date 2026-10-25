package fake

import (
	"testing"
	"time"
)

func TestClosingCohortAndStableOutcomes(t *testing.T) {
	_, c, _, _, _ := fixture(t)
	m, e := NewProducers(c)
	must(t, e)
	d, _ := c.Now().Instant.Add(time.Minute)
	p, e := m.Register("p", d)
	must(t, e)
	must(t, m.BeginClose())
	_, e = m.Register("late", d)
	want(t, e, ErrBusy)
	x, e := m.Dispatch(p, "m1", "input")
	must(t, e) // admitted producer may attach in Closing
	want(t, m.FinishClose(), ErrBusy)
	must(t, m.Commit(x))
	must(t, m.CloseProducer(p))
	must(t, m.FinishClose())
	o, e := m.Reconcile(x)
	must(t, e)
	if o != Committed {
		t.Fatal(o)
	}
	must(t, m.Commit(x)) // original committed receipt
}

func TestPublicationExpiryAndFenceOrder(t *testing.T) {
	for _, closeFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit before closure", true: "closure before commit"}[closeFirst], func(t *testing.T) {
			_, c, _, _, _ := fixture(t)
			m, e := NewProducers(c)
			must(t, e)
			d, _ := c.Now().Instant.Add(time.Second)
			p, e := m.Register("p", d)
			must(t, e)
			x, e := m.Dispatch(p, "m", "digest")
			must(t, e)
			must(t, c.Advance(time.Second))
			_, e = m.Dispatch(p, "new", "digest")
			want(t, e, ErrLost)
			if closeFirst {
				must(t, m.CloseProducer(p))
				want(t, m.Commit(x), ErrLost)
			} else {
				must(t, m.Commit(x))
				must(t, m.CloseProducer(p))
			}
			o, e := m.Reconcile(x)
			must(t, e)
			expected := Committed
			if closeFirst {
				expected = NoCommit
			}
			if o != expected {
				t.Fatal(o)
			}
		})
	}
}

func TestUnknownOutcomeCannotCloseEpoch(t *testing.T) {
	_, c, _, _, _ := fixture(t)
	m, e := NewProducers(c)
	must(t, e)
	d, _ := c.Now().Instant.Add(time.Minute)
	p, e := m.Register("p", d)
	must(t, e)
	x, e := m.Dispatch(p, "m", "digest")
	must(t, e)
	_, e = m.Dispatch(p, "m", "different")
	want(t, e, ErrConflict)
	must(t, m.BeginClose())
	_, e = m.Reconcile(x)
	want(t, e, ErrBusy)
	must(t, m.CloseProducer(p))
	want(t, m.FinishClose(), ErrBusy)
	o, e := m.Reconcile(x)
	must(t, e)
	if o != NoCommit {
		t.Fatal(o)
	}
	must(t, m.FinishClose())
	want(t, m.AbortClose(), ErrConflict)
}

func TestAbortDoesNotResurrectOldFence(t *testing.T) {
	_, c, _, _, _ := fixture(t)
	m, e := NewProducers(c)
	must(t, e)
	d, _ := c.Now().Instant.Add(time.Minute)
	p, e := m.Register("p", d)
	must(t, e)
	must(t, m.BeginClose())
	must(t, m.CloseProducer(p))
	must(t, m.AbortClose())
	_, e = m.Dispatch(p, "late", "input")
	want(t, e, ErrLost)
	_, e = m.Register("new", d)
	must(t, e)
}

func TestMutationIdentityCannotCollideAcrossProducers(t *testing.T) {
	_, c, _, _, _ := fixture(t)
	m, e := NewProducers(c)
	must(t, e)
	d, _ := c.Now().Instant.Add(time.Minute)
	a, e := m.Register("a", d)
	must(t, e)
	b, e := m.Register("b", d)
	must(t, e)
	x, e := m.Dispatch(a, "mutation", "same-input")
	must(t, e)
	_, e = m.Dispatch(b, "mutation", "same-input")
	want(t, e, ErrConflict)
	must(t, m.Commit(x))
}
