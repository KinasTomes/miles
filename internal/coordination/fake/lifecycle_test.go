package fake

import (
	"context"
	"testing"
	"time"
)

func TestPausedPublicationLosesToClosure(t *testing.T) {
	_, c, _, _, _ := fixture(t)
	m, e := NewProducers(c)
	must(t, e)
	d, _ := c.Now().Instant.Add(time.Minute)
	p, e := m.Register("p", d)
	must(t, e)
	x, e := m.Dispatch(p, "m", "input")
	must(t, e)
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	f := Faults{Hook: func(_ context.Context, _ Point, phase Phase) error {
		if phase == Before {
			close(entered)
			<-release
		}
		return nil
	}}
	go func() { result <- f.Run(context.Background(), Publication, func() error { return m.Commit(x) }) }()
	<-entered
	m.InvalidateAuthority()
	close(release)
	want(t, <-result, ErrLost)
	o, e := m.Reconcile(x)
	must(t, e)
	if o != NoCommit {
		t.Fatal(o)
	}
}

func TestRenewDeadlineAndLostEquality(t *testing.T) {
	m, c, h, _, _ := fixture(t)
	now := c.Now().Instant
	hard, _ := now.Add(10 * time.Second)
	lease, _ := now.Add(time.Second)
	v, e := m.AcquireWithLease("v", h.Object, hard, lease)
	must(t, e)
	next, _ := now.Add(2 * time.Second)
	rev, e := m.Renew(v, 1, next)
	must(t, e)
	if rev != 2 {
		t.Fatal(rev)
	}
	_, e = m.Renew(v, 1, hard)
	want(t, e, ErrConflict)
	tooLate, _ := hard.Add(time.Nanosecond)
	_, e = m.Renew(v, 2, tooLate)
	want(t, e, ErrInvalid)
	must(t, c.SetWall(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)))
	must(t, c.Advance(2*time.Second))
	_, e = m.Renew(v, 2, hard)
	want(t, e, ErrLost)
	must(t, m.BeginRelease(v))
	must(t, m.FinishRelease(v))
}

func TestTakeoverRequiresClosureReconciliationAndGrace(t *testing.T) {
	_, c, _, _, _ := fixture(t)
	m, e := NewProducers(c)
	must(t, e)
	now := c.Now().Instant
	lease, _ := now.Add(time.Second)
	next, _ := now.Add(time.Minute)
	p, e := m.Register("p", lease)
	must(t, e)
	x, e := m.Dispatch(p, "unknown", "input")
	must(t, e)
	_, e = m.Takeover(p, next, time.Second)
	want(t, e, ErrBusy)
	must(t, m.CloseProducer(p))
	must(t, c.Advance(2*time.Second))
	_, e = m.Takeover(p, next, time.Second)
	want(t, e, ErrBusy) // expiry is not outcome proof
	o, e := m.Reconcile(x)
	must(t, e)
	if o != NoCommit {
		t.Fatal(o)
	}
	n, e := m.Takeover(p, next, time.Second)
	must(t, e)
	_, e = m.Takeover(p, next, time.Second)
	want(t, e, ErrConflict)
	_, e = m.Dispatch(p, "old-returned", "input")
	want(t, e, ErrLost)
	want(t, m.Commit(x), ErrLost)
	y, e := m.Dispatch(n, "new", "input")
	must(t, e)
	must(t, m.Commit(y))
	must(t, m.BeginClose())
	must(t, m.CloseProducer(n))
	must(t, m.FinishClose())
	members, e := m.ClosedMembers()
	must(t, e)
	if len(members["p"]) != 1 || members["p"][0] != "new" {
		t.Fatal(members)
	}
	members["p"][0] = "mutated"
	again, e := m.ClosedMembers()
	must(t, e)
	if again["p"][0] != "new" {
		t.Fatal("alias")
	}
}

func TestTakeoverBeforeGraceAndDuringClosing(t *testing.T) {
	_, c, _, _, _ := fixture(t)
	m, e := NewProducers(c)
	must(t, e)
	now := c.Now().Instant
	lease, _ := now.Add(time.Second)
	next, _ := now.Add(time.Minute)
	p, e := m.Register("p", lease)
	must(t, e)
	must(t, m.CloseProducer(p))
	must(t, c.Advance(2*time.Second-time.Nanosecond))
	_, e = m.Takeover(p, next, time.Second)
	want(t, e, ErrBusy)
	must(t, c.Advance(time.Nanosecond))
	must(t, m.BeginClose())
	_, e = m.Takeover(p, next, time.Second)
	want(t, e, ErrBusy)
}
