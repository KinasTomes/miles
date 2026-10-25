package recovery_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	coord "github.com/KinasTomes/miles/internal/coordination/fake"
)

func TestEveryJournalWriteFaultWindow(t *testing.T) {
	for nth := 1; nth <= 5; nth++ {
		for _, phase := range []coord.Phase{coord.Before, coord.After} {
			t.Run(fmt.Sprintf("write-%d/%s", nth, phase), func(t *testing.T) {
				m := newExport(t)
				calls := 0
				crash := errors.New("write crash")
				f := coord.Faults{Hook: func(_ context.Context, p coord.Point, at coord.Phase) error {
					if p == coord.S3Write && at == phase {
						calls++
						if calls == nth {
							return crash
						}
					}
					return nil
				}}
				if e := m.run(context.Background(), f); !errors.Is(e, crash) {
					t.Fatal("fault not reached", e)
				}
				if m.published != "" {
					t.Fatal("partial export sealed")
				}
				if e := m.run(context.Background(), coord.Faults{}); e != nil {
					t.Fatal("resume", e)
				}
				if len(m.durable.Objects) != 5 {
					t.Fatal("duplicate or missing immutable writes", len(m.durable.Objects))
				}
			})
		}
	}
}

// The immutable object map is a fake S3 journal sink, not the pack adapter or a
// production export service. Every effect is real in this executable model.
type exportModel struct {
	source    journal
	durable   journal
	published string
	producers *coord.Producers
	deadline  clock.Instant
}

func newExport(t *testing.T) *exportModel {
	t.Helper()
	c, e := clock.NewVirtual(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), 100)
	if e != nil {
		t.Fatal(e)
	}
	p, e := coord.NewProducers(c)
	if e != nil {
		t.Fatal(e)
	}
	d, e := c.Now().Instant.Add(time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	j := journalFixture(t)
	return &exportModel{source: j, durable: journal{History: j.History, Objects: map[string]string{}}, producers: p, deadline: d}
}
func (m *exportModel) put(ref reference) error {
	body, ok := m.source.Objects[ref.SHA]
	if !ok || digest(body) != ref.SHA || len(body) != ref.Length {
		return invalid
	}
	if old, ok := m.durable.Objects[ref.SHA]; ok && old != body {
		return invalid
	}
	m.durable.Objects[ref.SHA] = body
	return nil
}
func (m *exportModel) publish(ref reference) error {
	if m.published != "" {
		if m.published == ref.SHA {
			return nil
		}
		return invalid
	}
	members, e := m.producers.ClosedMembers()
	if e != nil {
		return e
	}
	var s epochSeal
	if e = m.durable.read(ref, &s); e != nil {
		return e
	}
	if s.History != m.source.History || s.Epoch != 0 || s.Previous != "genesis" || s.Version != 1 {
		return invalid
	}
	var membership memberPage
	if e = m.durable.read(s.Membership, &membership); e != nil {
		return e
	}
	if membership.History != s.History || membership.Epoch != s.Epoch || membership.Version != 1 {
		return invalid
	}
	expected := []string{}
	for _, ids := range members {
		expected = append(expected, ids...)
	}
	sort.Strings(expected)
	got := []string{}
	last := ""
	for _, event := range membership.Events {
		if event.ID <= last {
			return invalid
		}
		last = event.ID
		if _, _, e = m.durable.bundle(event, 0); e != nil {
			return e
		}
		got = append(got, event.ID)
	}
	if !reflect.DeepEqual(expected, got) {
		return invalid
	}
	m.published = ref.SHA
	return nil
}
func (m *exportModel) run(ctx context.Context, f coord.Faults) error {
	var s epochSeal
	if e := m.source.read(m.source.Seals[0], &s); e != nil {
		return e
	}
	var membership memberPage
	if e := m.source.read(s.Membership, &membership); e != nil {
		return e
	}
	event := membership.Events[0]
	var p coord.Producer
	if e := f.Run(ctx, coord.Registration, func() error { var e error; p, e = m.producers.Register("producer", m.deadline); return e }); e != nil {
		return e
	}
	x, e := m.producers.Dispatch(p, event.ID, event.Descriptor.SHA)
	if e != nil {
		return e
	}
	// Only model durable prepare/export objects. No cloud call or real outbox
	// is asserted, and prepare order is not presented as a synchronous journal
	// requirement for production object commit.
	d, _, e := m.source.bundle(event, 0)
	if e != nil {
		return e
	}
	for _, ref := range append(append([]reference{}, d.Pages...), event.Descriptor) {
		r := ref
		if e = f.Run(ctx, coord.S3Write, func() error { return m.put(r) }); e != nil {
			return e
		}
	}
	if e = f.Run(ctx, coord.Publication, func() error { return m.producers.Commit(x) }); e != nil {
		return e
	}
	if e = m.producers.BeginClose(); e != nil { // retry after confirmed Closed is allowed
		if _, closed := m.producers.ClosedMembers(); closed != nil {
			return e
		}
	}
	if e = m.producers.CloseProducer(p); e != nil {
		return e
	}
	if _, e = m.producers.Reconcile(x); e != nil {
		return e
	}
	if e = f.Run(ctx, coord.EpochClose, m.producers.FinishClose); e != nil {
		return e
	}
	for _, ref := range []reference{s.Membership, m.source.Seals[0]} {
		r := ref
		if e = f.Run(ctx, coord.S3Write, func() error { return m.put(r) }); e != nil {
			return e
		}
	}
	return f.Run(ctx, coord.SealPublish, func() error { return m.publish(m.source.Seals[0]) })
}

func TestRegistrationExportCommitCloseSealFaultRecovery(t *testing.T) {
	for _, point := range []coord.Point{coord.Registration, coord.S3Write, coord.Publication, coord.EpochClose, coord.SealPublish} {
		for _, phase := range []coord.Phase{coord.Before, coord.After} {
			t.Run(string(point)+"/"+string(phase), func(t *testing.T) {
				m := newExport(t)
				crash := errors.New("crash")
				injected := false
				f := coord.Faults{Hook: func(_ context.Context, p coord.Point, at coord.Phase) error {
					if !injected && p == point && at == phase {
						injected = true
						return crash
					}
					return nil
				}}
				e := m.run(context.Background(), f)
				if !errors.Is(e, crash) || !injected {
					t.Fatal("fault not reached", e)
				}
				if phase == coord.After && !errors.Is(e, coord.ErrUnknown) {
					t.Fatal("lost ACK misclassified", e)
				}
				if point == coord.SealPublish && phase == coord.After {
					if m.published == "" {
						t.Fatal("effect rolled back")
					}
				} else if m.published != "" {
					t.Fatal("early seal")
				}
				if e = m.run(context.Background(), coord.Faults{}); e != nil {
					t.Fatal("resume", e)
				}
				if m.published != m.source.Seals[0].SHA {
					t.Fatal("wrong seal")
				}
				members, e := m.producers.ClosedMembers()
				if e != nil || len(members["producer"]) != 1 {
					t.Fatal("duplicate/missing publication", members, e)
				}
			})
		}
	}
}
func TestSealRequiresClosedExactMembershipAndDurablePages(t *testing.T) {
	m := newExport(t)
	for hash, body := range m.source.Objects {
		m.durable.Objects[hash] = body
	}
	if m.publish(m.source.Seals[0]) == nil {
		t.Fatal("open epoch sealed")
	}
	if e := m.producers.BeginClose(); e != nil {
		t.Fatal(e)
	}
	if e := m.producers.FinishClose(); e != nil {
		t.Fatal(e)
	}
	if m.publish(m.source.Seals[0]) == nil {
		t.Fatal("wrong exact membership sealed")
	}
	m = newExport(t)
	stop := errors.New("stop before seal")
	e := m.run(context.Background(), coord.Faults{Hook: func(_ context.Context, p coord.Point, phase coord.Phase) error {
		if p == coord.SealPublish && phase == coord.Before {
			return stop
		}
		return nil
	}})
	if !errors.Is(e, stop) {
		t.Fatal(e)
	}
	var s epochSeal
	_ = m.source.read(m.source.Seals[0], &s)
	var membership memberPage
	_ = m.source.read(s.Membership, &membership)
	d, _, e := m.source.bundle(membership.Events[0], 0)
	if e != nil {
		t.Fatal(e)
	}
	delete(m.durable.Objects, d.Pages[1].SHA)
	if m.publish(m.source.Seals[0]) == nil || m.published != "" {
		t.Fatal("missing page sealed")
	}
}

func TestExplicitEmptyEpochAndImmutableWriteConflict(t *testing.T) {
	m := newExport(t)
	membership := saveNode(t, &m.source, memberPage{Epoch: 0, History: m.source.History, Version: 1, Events: []member{}})
	ref := saveNode(t, &m.source, epochSeal{Epoch: 0, History: m.source.History, Membership: membership, Previous: "genesis", Version: 1})
	if e := m.put(membership); e != nil {
		t.Fatal(e)
	}
	if e := m.put(ref); e != nil {
		t.Fatal(e)
	}
	if m.publish(ref) == nil {
		t.Fatal("empty but not closed")
	}
	if e := m.producers.BeginClose(); e != nil {
		t.Fatal(e)
	}
	if e := m.producers.FinishClose(); e != nil {
		t.Fatal(e)
	}
	if e := m.publish(ref); e != nil {
		t.Fatal(e)
	}
	m.durable.Objects[ref.SHA] = "corrupt"
	if m.put(ref) == nil || m.durable.Objects[ref.SHA] != "corrupt" {
		t.Fatal("silently overwritten immutable conflict")
	}
}
