package recovery_test

import (
	"encoding/json"
	"reflect"
	"testing"
)

func saveNode(t *testing.T, j *journal, v any) reference {
	t.Helper()
	b, e := json.Marshal(v)
	if e != nil {
		t.Fatal(e)
	}
	if e = canonical(string(b)); e != nil {
		t.Fatal("noncanonical test mutation", string(b), e)
	}
	r := reference{len(b), digest(string(b))}
	j.Objects[r.SHA] = string(b)
	return r
}

// Rehash ALL ancestors after semantic mutation: failures cannot be explained by
// stale checksums. The checkpoint effect oracle remains independent and fixed.
func mutateChain(t *testing.T, j *journal, change func(*eventPage, *eventDescriptor, *memberPage, *epochSeal, *checkpointPage)) {
	t.Helper()
	var s epochSeal
	var m memberPage
	var d eventDescriptor
	var p eventPage
	var cp checkpointPage
	for _, x := range []struct {
		r reference
		v any
	}{{j.Seals[0], &s}, {j.Checkpoint, &cp}} {
		if e := j.read(x.r, x.v); e != nil {
			t.Fatal(e)
		}
	}
	if e := j.read(s.Membership, &m); e != nil {
		t.Fatal(e)
	}
	if e := j.read(m.Events[0].Descriptor, &d); e != nil {
		t.Fatal(e)
	}
	if e := j.read(d.Pages[0], &p); e != nil {
		t.Fatal(e)
	}
	change(&p, &d, &m, &s, &cp)
	d.Pages[0] = saveNode(t, j, p)
	m.Events[0].Descriptor = saveNode(t, j, d)
	s.Membership = saveNode(t, j, m)
	j.Seals[0] = saveNode(t, j, s)
	var next epochSeal
	if e := j.read(j.Seals[1], &next); e != nil {
		t.Fatal(e)
	}
	next.Previous = j.Seals[0].SHA
	j.Seals[1] = saveNode(t, j, next)
	cp.Seal = j.Seals[1]
	j.Checkpoint = saveNode(t, j, cp)
}
func TestJournalSemanticCorruptionWithValidDigests(t *testing.T) {
	for _, name := range []string{"page order", "page history", "page event", "duplicate effects", "revision gap", "descriptor identity", "descriptor version", "membership epoch", "duplicate membership", "seal epoch", "checkpoint projection", "checkpoint state"} {
		t.Run(name, func(t *testing.T) {
			j := journalFixture(t)
			mutateChain(t, &j, func(p *eventPage, d *eventDescriptor, m *memberPage, s *epochSeal, cp *checkpointPage) {
				switch name {
				case "page order":
					p.Number = 1
				case "page history":
					p.History = "wrong"
				case "page event":
					p.Event = "wrong"
				case "duplicate effects":
					p.Effects = append(p.Effects, p.Effects[0])
				case "revision gap":
					p.Effects[0].Revision = 2
				case "descriptor identity":
					d.Mutation = "33333333333333333333333333333333"
				case "descriptor version":
					d.Version = 2
				case "membership epoch":
					m.Epoch = 1
				case "duplicate membership":
					m.Events = append(m.Events, m.Events[0])
				case "seal epoch":
					s.Epoch = 1
				case "checkpoint projection":
					cp.Projection = "recovery_genesis"
				case "checkpoint state":
					cp.State[0].Deleted = false
				}
			})
			p, e := j.rebuild()
			if e == nil || p.rows != nil {
				t.Fatal("accepted/partial", p, e)
			}
		})
	}
}
func TestJournalUnknownMissingFieldsAndChainOrder(t *testing.T) {
	j := journalFixture(t)
	for _, body := range []string{`{"epoch":0,"history":"h","membership":{"length":1,"sha256":"x"},"previous":"genesis","r2_version":1,"unexpected":0}`, `{"history":"h","membership":{"length":1,"sha256":"x"},"previous":"genesis","r2_version":1}`} {
		r := reference{len(body), digest(body)}
		j.Objects[r.SHA] = body
		var s epochSeal
		if j.read(r, &s) == nil {
			t.Fatal("accepted unknown/missing field")
		}
	}
	j = journalFixture(t)
	j.Seals[0], j.Seals[1] = j.Seals[1], j.Seals[0]
	if _, e := j.rebuild(); e == nil {
		t.Fatal("out-of-order chain")
	}
}
func TestHistoricalCheckpointKeepsTombstones(t *testing.T) {
	j := journalFixture(t)
	p, e := j.rebuild()
	if e != nil {
		t.Fatal(e)
	}
	object := p.rows["object/bucket-1/k"]
	if !object.Deleted || object.Revision != 2 || object.Value != "" {
		t.Fatal("resurrected object", object)
	}
	before := ordered(p)
	if p.apply("stale", "other", []row{{Key: object.Key, Revision: 1, Value: "old"}}) == nil {
		t.Fatal("stale replay")
	}
	if !reflect.DeepEqual(before, ordered(p)) {
		t.Fatal("changed checkpoint")
	}
}
