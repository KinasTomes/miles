package recovery_test

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"

	coord "github.com/KinasTomes/miles/internal/coordination/fake"
)

func TestSealBindsCommittedDescriptor(t *testing.T) {
	for _, forged := range []bool{false, true} {
		t.Run(map[bool]string{false: "unchanged", true: "rehashed replacement"}[forged], func(t *testing.T) {
			m := newExport(t)
			stop := errors.New("before seal")
			err := m.run(context.Background(), coord.Faults{Hook: func(_ context.Context, p coord.Point, phase coord.Phase) error {
				if p == coord.SealPublish && phase == coord.Before {
					return stop
				}
				return nil
			}})
			if !errors.Is(err, stop) {
				t.Fatal(err)
			}
			if forged {
				mutateChain(t, &m.source, func(p *eventPage, _ *eventDescriptor, _ *memberPage, _ *epochSeal, _ *checkpointPage) {
					p.Effects[0].Value = "manifest-forged"
				})
				for hash, body := range m.source.Objects {
					m.durable.Objects[hash] = body
				}
			}
			err = m.publish(m.source.Seals[0])
			if forged {
				if err == nil || m.published != "" {
					t.Fatal("replacement descriptor sealed", err)
				}
			} else if err != nil || m.published != m.source.Seals[0].SHA {
				t.Fatal("unchanged payload rejected", err)
			}
		})
	}
}

// Rebind the two existing golden bundles into one epoch. Swap mutation IDs to
// exercise both hash orders without changing the event identity formula.
func sameEpochJournal(t *testing.T, swap bool) journal {
	t.Helper()
	j := journalFixture(t)
	ds := make([]eventDescriptor, 2)
	pages := make([][]eventPage, 2)
	for i, ref := range j.Seals {
		var s epochSeal
		var m memberPage
		if err := j.read(ref, &s); err != nil {
			t.Fatal(err)
		}
		if err := j.read(s.Membership, &m); err != nil {
			t.Fatal(err)
		}
		d, _, err := j.bundle(m.Events[0], uint64(i))
		if err != nil {
			t.Fatal(err)
		}
		ds[i] = d
		for _, r := range d.Pages {
			var p eventPage
			if err := j.read(r, &p); err != nil {
				t.Fatal(err)
			}
			pages[i] = append(pages[i], p)
		}
	}
	if swap {
		ds[0].Mutation, ds[1].Mutation = ds[1].Mutation, ds[0].Mutation
	}
	members := []member{}
	for i := range ds {
		d := ds[i]
		d.Epoch = 0
		d.ID = eventIdentity(d)
		d.Pages = nil
		for _, p := range pages[i] {
			p.Epoch = 0
			p.Event = d.ID
			d.Pages = append(d.Pages, saveNode(t, &j, p))
		}
		members = append(members, member{Descriptor: saveNode(t, &j, d), ID: d.ID})
	}
	sort.Slice(members, func(i, k int) bool { return members[i].ID < members[k].ID })
	mr := saveNode(t, &j, memberPage{Epoch: 0, Events: members, History: j.History, Version: 1})
	sr := saveNode(t, &j, epochSeal{Epoch: 0, History: j.History, Membership: mr, Previous: "genesis", Version: 1})
	j.Seals = []reference{sr}
	j.Checkpoint = saveNode(t, &j, checkpointPage{History: j.History, Projection: "historical", Version: 1, Seal: sr, State: j.Expected, Target: 0})
	return j
}

func TestReplayIndependentOfMembershipHashOrder(t *testing.T) {
	orders := map[uint64]bool{}
	for _, swap := range []bool{false, true} {
		j := sameEpochJournal(t, swap)
		var s epochSeal
		var m memberPage
		if err := j.read(j.Seals[0], &s); err != nil {
			t.Fatal(err)
		}
		if err := j.read(s.Membership, &m); err != nil {
			t.Fatal(err)
		}
		_, rows, err := j.bundle(m.Events[0], 0)
		if err != nil {
			t.Fatal(err)
		}
		orders[rows[0].Revision] = true
		p, err := j.rebuild()
		if err != nil {
			t.Errorf("swap=%v first revision=%d: %v", swap, rows[0].Revision, err)
			continue
		}
		if !reflect.DeepEqual(ordered(p), j.Expected) || len(p.applied) != 2 {
			t.Fatal("wrong/partial projection", p)
		}
	}
	if !orders[1] || !orders[2] {
		t.Fatal("did not exercise both hash orders", orders)
	}
}

func TestReplayUnresolvableDependenciesFailWithoutPartialProjection(t *testing.T) {
	for _, mode := range []string{"gap", "conflict", "cycle"} {
		t.Run(mode, func(t *testing.T) {
			j := sameEpochJournal(t, true)
			var s epochSeal
			var m memberPage
			if err := j.read(j.Seals[0], &s); err != nil {
				t.Fatal(err)
			}
			if err := j.read(s.Membership, &m); err != nil {
				t.Fatal(err)
			}
			for i, event := range m.Events {
				d, rows, err := j.bundle(event, 0)
				if err != nil {
					t.Fatal(err)
				}
				rev := rows[0].Revision
				for k, ref := range d.Pages {
					var p eventPage
					if err := j.read(ref, &p); err != nil {
						t.Fatal(err)
					}
					for n := range p.Effects {
						switch mode {
						case "gap":
							if rev == 2 {
								p.Effects[n].Revision = 3
							}
						case "conflict":
							p.Effects[n].Revision = 1
						case "cycle":
							if k == 1 {
								p.Effects[n].Revision = 3 - rev
							}
						}
					}
					d.Pages[k] = saveNode(t, &j, p)
				}
				m.Events[i].Descriptor = saveNode(t, &j, d)
			}
			s.Membership = saveNode(t, &j, m)
			j.Seals[0] = saveNode(t, &j, s)
			// Keep the checkpoint reference consistent: rejection must precede
			// checkpoint comparison, not be caused by a stale seal hash.
			var cp checkpointPage
			if err := j.read(j.Checkpoint, &cp); err != nil {
				t.Fatal(err)
			}
			cp.Seal = j.Seals[0]
			j.Checkpoint = saveNode(t, &j, cp)
			p, err := j.rebuild()
			if err == nil || p.rows != nil || p.applied != nil {
				t.Fatal("accepted/partial", p, err)
			}
		})
	}
}
