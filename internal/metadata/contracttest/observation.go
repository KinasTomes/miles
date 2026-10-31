package contracttest

import (
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/stats"
	s "github.com/KinasTomes/miles/internal/storage"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	"reflect"
	"testing"
	"time"
)

type ObservingStore interface {
	s.MetadataStore
	stats.Store
}
type ObservationFactory func(*testing.T, shared.Fixture) ObservingStore
type observationInventory struct {
	before    func()
	inventory s.PhysicalInventory
	err       error
	calls     int
}

func (i *observationInventory) CollectInventory(ctx context.Context, _ int) (s.PhysicalInventory, error) {
	i.calls++
	if i.before != nil {
		i.before()
	}
	if err := ctx.Err(); err != nil {
		return i.inventory, err
	}
	return i.inventory, i.err
}
func RunObservation(t *testing.T, factory ObservationFactory) {
	ctx := context.Background()
	f := shared.NewFixture()
	m := factory(t, f)
	r := f.Request("stats", 0)
	shared.Prepare(t, m, f, r, 4)
	if _, err := m.CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
	// A system-admin S3 principal without grants sees no buckets; trusted admin
	// enumeration must nevertheless observe the complete installation.
	accessible, err := m.ListAccessibleBuckets(ctx, s.BucketListRequest{PrincipalID: "admin", Limit: 10})
	if err != nil || len(accessible.Buckets) != 0 {
		t.Fatal(accessible, err)
	}
	for _, kind := range []string{"buckets", "heads", "packs"} {
		cursor := ""
		count := 0
		for {
			p, err := m.ObservePage(ctx, s.ObservationRequest{Kind: kind, Cursor: cursor, Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			n := len(p.Buckets) + len(p.Heads) + len(p.Packs)
			if n > 1 {
				t.Fatal(p)
			}
			count += n
			if p.Next == "" {
				break
			}
			if p.Next == cursor {
				t.Fatal("cursor did not advance")
			}
			cursor = p.Next
		}
		if count < 1 {
			t.Fatal("admin enumeration empty", kind)
		}
	}
	inv := &observationInventory{inventory: s.PhysicalInventory{Complete: true, VersionScope: "all-retained", Objects: []s.PhysicalObject{{Key: "a", Category: "pack", Pack: f.Packs[0].Ref, Current: true, Size: 64}, {Key: "b", Category: "pack", Pack: f.Packs[1].Ref, Current: true, Size: 64}, {Key: "orphan", Category: "other", Current: true, Size: 7}}}}
	now := f.Now
	svc, err := stats.New(stats.Options{Store: m, Inventory: inv, Now: func() time.Time { return now }, MaxRecords: 100, MaxReferences: 100, MaxPhysicalObjects: 100, Timeout: time.Minute, MaxAge: time.Minute, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Global(ctx); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("cold report fabricated", err)
	}
	before, err := m.GetQuota(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	g, err := svc.Global(ctx)
	if err != nil || g.BytesLogical != 4 || g.BytesUniqueRaw != 4 || g.BytesUniqueStored != 4 || g.BytesStored == nil || *g.BytesStored != 135 || g.DedupRatio == nil || *g.DedupRatio != 1 {
		t.Fatal(g, err)
	}
	b, err := svc.Bucket(ctx, "demo")
	if err != nil || b.BytesStored == nil || *b.BytesStored != 4 || b.SnapshotID != g.SnapshotID {
		t.Fatal(b, err)
	}
	l, err := svc.Layout(ctx, "demo", "file")
	if err != nil || l.Size != 4 || l.Shared != 0 || l.Occurrences != 1 || len(l.Packs) != 1 {
		t.Fatal(l, err)
	}
	g.Breakdown["pack"] = 0
	again, _ := svc.Global(ctx)
	if again.Breakdown["pack"] != 128 {
		t.Fatal("caller mutated cached report")
	}
	if inv.calls != 1 {
		t.Fatal("HTTP report reads collected inventory", inv.calls)
	}
	after, err := m.GetQuota(ctx, "owner")
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("stats changed logical quota", before, after, err)
	}
	late := f.Request("stats-late", 0)
	late.Head.Object.Key = "late"
	late.Event.Object = late.Head.Object
	shared.Prepare(t, m, f, late, 4)
	if _, err = m.CommitObject(ctx, late); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.Layout(ctx, "demo", "late"); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("new live object misreported as missing", err)
	}
	if err = svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	newLayout, err := svc.Layout(ctx, "demo", "late")
	if err != nil || newLayout.Shared != 1 {
		t.Fatal(newLayout, err)
	}
	inv.err = s.ErrUnavailable
	if err = svc.Refresh(ctx); err == nil {
		t.Fatal("failed inventory published")
	}
	now = now.Add(2 * time.Minute)
	if _, err = svc.Global(ctx); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("stale report served", err)
	}
}
