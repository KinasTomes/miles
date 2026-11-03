package recovery

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/metadata/operations"
	s "github.com/KinasTomes/miles/internal/storage"
)

func checkpointFixture(t *testing.T) (Checkpointer, AttachmentReference) {
	t.Helper()
	o := &memoryObjects{objects: map[string][]byte{}}
	h := strings.Repeat("1", 32)
	base := Snapshot{Version: 1, Mode: "historical", History: h, BeforeFirst: true, Pages: []AttachmentReference{}, Dependencies: []Dependency{}}
	b, e := Encode(base)
	if e != nil {
		t.Fatal(e)
	}
	c := Catalog{Version: 1, History: h, Incarnation: strings.Repeat("2", 32), Genesis: ref(b)}
	o.objects[genesisKey(c)] = b
	b, e = Encode(c)
	if e != nil {
		t.Fatal(e)
	}
	o.objects[catalogKey(c, b)] = b
	o.objects["recovery/active-history.json"] = b
	return Checkpointer{Objects: o, Catalog: c}, AttachmentReference{Key: genesisKey(c), Reference: c.Genesis}
}
func fixtureSeal(t *testing.T, w Checkpointer, epoch uint64, previous string, bundles ...[]Effect) AttachmentReference {
	t.Helper()
	ctx := context.Background()
	o := w.Objects.(*memoryObjects)
	builder := membershipBuilder{objects: o, prefix: epochPrefix(w.Catalog.History, epoch), history: w.Catalog.History, epoch: epoch}
	chain := ""
	for n, rows := range bundles {
		mutation := fmt.Sprintf("%032x", n+int(epoch)*1000+1)
		id, e := s.EventID(w.Catalog.History, mutation, "Users", "entity_put")
		if e != nil {
			t.Fatal(e)
		}
		source := s.OutboxEvent{Version: 1, HistoryID: w.Catalog.History, Epoch: epoch, ID: id, OpID: mutation, Entity: "Users", EntityID: "user", Action: "entity_put", Revision: 1, PayloadPointer: "retained/" + id, PayloadSHA256: strings.Repeat("a", 64)}
		p := EventPage{Version: 1, History: source.HistoryID, Epoch: epoch, Event: id, Effects: rows}
		b, e := Encode(p)
		if e != nil {
			t.Fatal(e)
		}
		o.objects[eventRoot(source)+"pages/0.json"] = b
		d := Descriptor{Version: 1, History: source.HistoryID, Epoch: epoch, ID: id, Logical: mutation, Mutation: mutation, Entity: source.Entity, Action: source.Action, Source: source, Pages: []Reference{ref(b)}}
		if raw, ok := o.objects[fmt.Sprintf("fixture/dependencies/%d/%d", epoch, n)]; ok {
			if Decode(raw, &d.Dependencies) != nil {
				t.Fatal("fixture dependency")
			}
		}
		b, e = Encode(d)
		if e != nil {
			t.Fatal(e)
		}
		o.objects[eventRoot(source)+"descriptor.json"] = b
		// membership IDs must be sorted independently of event creation order.
	}
	var members []Member
	for key, body := range o.objects {
		if strings.HasPrefix(key, "journal/"+w.Catalog.History+fmt.Sprintf("/%d/", epoch)) && strings.HasSuffix(key, "descriptor.json") {
			var d Descriptor
			if Decode(body, &d) != nil {
				t.Fatal("descriptor")
			}
			members = append(members, Member{ID: d.ID, Descriptor: ref(body)})
		}
	}
	sortMembers(members)
	for _, m := range members {
		if e := builder.add(ctx, m); e != nil {
			t.Fatal(e)
		}
		event, e := verifiedEvent(ctx, o, m, w.Catalog.History, epoch)
		if e != nil {
			t.Fatal(e)
		}
		chain, e = s.RecoveryDigest(chain, event)
		if e != nil {
			t.Fatal(e)
		}
	}
	root, e := builder.finish(ctx)
	if e != nil {
		t.Fatal(e)
	}
	seal := Seal{Version: 1, History: w.Catalog.History, Epoch: epoch, CloseID: strings.Repeat("3", 32), CutAt: time.Date(2026, 11, 4, 0, int(epoch), 0, 0, time.UTC).Format(time.RFC3339Nano), Events: uint64(len(members)), EventsSHA256: chain, Membership: root, PreviousSHA256: previous}
	b, e := Encode(seal)
	if e != nil {
		t.Fatal(e)
	}
	r := AttachmentReference{Key: epochPrefix(seal.History, epoch) + "seal-" + digest(b) + ".json", Reference: ref(b)}
	o.objects[r.Key] = b
	b, e = Encode(Completion{Version: 1, History: seal.History, Epoch: epoch, Seal: r})
	if e != nil {
		t.Fatal(e)
	}
	o.objects[epochPrefix(seal.History, epoch)+"complete.json"] = b
	return r
}
func sortMembers(m []Member) {
	for i := range m {
		for j := i + 1; j < len(m); j++ {
			if m[j].ID < m[i].ID {
				m[i], m[j] = m[j], m[i]
			}
		}
	}
}
func TestCheckpointContiguousExactCutAndRestart(t *testing.T) {
	w, base := checkpointFixture(t)
	s0 := fixtureSeal(t, w, 0, w.Catalog.Genesis.SHA256, []Effect{{Key: "row", Revision: 1, Value: "before"}})
	fixtureSeal(t, w, 1, s0.Reference.SHA256, []Effect{{Key: "row", Revision: 2, Value: "target"}})
	// A later corrupt epoch must not even be read when selecting epoch 1.
	w.Objects.(*memoryObjects).objects[epochPrefix(w.Catalog.History, 2)+"complete.json"] = []byte("corrupt")
	cp0, e := w.Build(t.Context(), base, 0)
	if e != nil {
		t.Fatal(e)
	}
	cp1, e := w.Build(t.Context(), cp0, 1)
	if e != nil {
		t.Fatal(e)
	}
	image, e := w.Load(t.Context(), cp1)
	if e != nil || image.Rows["row"].Value != "target" {
		t.Fatal(image, e)
	}
	again, e := w.Build(t.Context(), cp1, 1)
	if e != nil || again != cp1 {
		t.Fatal(again, e)
	}
	direct, e := w.Build(t.Context(), base, 1)
	if e != nil {
		t.Fatal(e)
	}
	got, e := w.Load(t.Context(), direct)
	if e != nil || !reflect.DeepEqual(got.Rows, image.Rows) {
		t.Fatal(got, e)
	}
	if _, e = w.Build(t.Context(), cp1, 0); !errors.Is(e, s.ErrInvalid) {
		t.Fatal(e)
	}
	delete(w.Objects.(*memoryObjects).objects, epochPrefix(w.Catalog.History, 0)+"complete.json")
	if _, e = w.Load(t.Context(), cp1); e == nil {
		t.Fatal("accepted missing chain anchor")
	}
}
func TestCheckpointRejectsConflictingEntityRevisionsAndPartialWrites(t *testing.T) {
	w, base := checkpointFixture(t)
	fixtureSeal(t, w, 0, w.Catalog.Genesis.SHA256, []Effect{{Key: "row", Revision: 1, Value: "a"}}, []Effect{{Key: "row", Revision: 1, Value: "b"}})
	if _, e := w.Build(t.Context(), base, 0); !errors.Is(e, s.ErrConflict) {
		t.Fatal(e)
	}
	w, base = checkpointFixture(t)
	fixtureSeal(t, w, 0, w.Catalog.Genesis.SHA256, []Effect{{Key: "row", Revision: 1, Value: "a"}})
	o := w.Objects.(*memoryObjects)
	o.fail = o.calls + 1
	if _, e := w.Build(t.Context(), base, 0); e == nil {
		t.Fatal("published partial checkpoint")
	}
	o.fail = 0
	if _, e := w.Build(t.Context(), base, 0); e != nil {
		t.Fatal(e)
	}
}
func TestCheckpointRecomputesUsageInsteadOfOrderingObjectLocalRevisions(t *testing.T) {
	w, base := checkpointFixture(t)
	a, e := operations.Projection("UsersUsage", "owner", 1, struct{ Used int }{1}, false)
	if e != nil {
		t.Fatal(e)
	}
	b := a
	b.Value = "different"
	fixtureSeal(t, w, 0, w.Catalog.Genesis.SHA256, []Effect{a}, []Effect{b})
	root, e := w.Build(t.Context(), base, 0)
	if e != nil {
		t.Fatal(e)
	}
	got, e := w.Load(t.Context(), root)
	if e != nil || len(got.Rows) != 0 {
		t.Fatal(got, e)
	}
}

func TestCheckpointPagedStateAndImmutablePolicyHistory(t *testing.T) {
	w, base := checkpointFixture(t)
	var bundles [][]Effect
	for page := 0; page < 5; page++ {
		rows := []Effect{}
		for n := 0; n < 128; n++ {
			rows = append(rows, Effect{Key: fmt.Sprintf("row/%04d", page*128+n), Revision: 1, Value: "retained"})
		}
		bundles = append(bundles, rows)
	}
	policy, e := operations.Projection("SystemConfig", "global-policy", 1, struct{ Version int }{1}, false)
	if e != nil {
		t.Fatal(e)
	}
	bundles = append(bundles, []Effect{policy})
	s0 := fixtureSeal(t, w, 0, w.Catalog.Genesis.SHA256, bundles...)
	first, e := w.Build(t.Context(), base, 0)
	if e != nil {
		t.Fatal(e)
	}
	policy.Revision = 2
	policy.Value = "next"
	fixtureSeal(t, w, 1, s0.Reference.SHA256, []Effect{policy})
	second, e := w.Build(t.Context(), first, 1)
	if e != nil {
		t.Fatal(e)
	}
	image, e := w.Load(t.Context(), second)
	if e != nil || len(image.Snapshot.Pages) < 5 {
		t.Fatal(image.Snapshot, e)
	}
	history := 0
	for key := range image.Rows {
		if strings.HasPrefix(key, "PolicyHistory/") {
			history++
		}
	}
	if history != 2 {
		t.Fatal("immutable policy revisions lost", history)
	}
}
