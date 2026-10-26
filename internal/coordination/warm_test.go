package coordination

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestWarmGatePinRetireLifetimeAndCopiedHandles(t *testing.T) {
	ctx := context.Background()
	c, _ := clock.NewVirtual(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), 20)
	g, _ := NewWarmGate(c)
	d, _ := c.Now().Instant.Add(time.Minute)
	object := storage.ObjectKey{Bucket: "demo", BucketID: "bucket-1", Key: "file"}
	a := storage.PackRef{ID: "pack-a", Generation: "1"}
	b := storage.PackRef{ID: "pack-a", Generation: "2"}
	v, err := g.Acquire(ctx, object, d)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(g.Retire(b), storage.ErrConflict) {
		t.Fatal("resolving gap")
	}
	if err = v.Bind(ctx, storage.ObjectHead{Object: object, ManifestID: "root", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if err = v.Pin(ctx, []storage.PackRef{a, a}); err != nil {
		t.Fatal(err)
	}
	if err = v.Resolved(ctx); err != nil {
		t.Fatal(err)
	}
	if err = g.Retire(b); err != nil {
		t.Fatal("unrelated generation blocked", err)
	}
	if !errors.Is(g.Retire(a), storage.ErrConflict) {
		t.Fatal("pinned generation retired")
	}
	copy := *v
	copy.Release()
	v.Release()
	if len(g.pins) != 0 || g.resolving != 0 {
		t.Fatal("double decrement", g.pins, g.resolving)
	}
	if err = g.Retire(a); err != nil {
		t.Fatal(err)
	}
	next, err := g.Acquire(ctx, object, d)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if err = next.Bind(ctx, storage.ObjectHead{Object: object, ManifestID: "root", Revision: 1}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(next.Pin(ctx, []storage.PackRef{a}), storage.ErrConflict) {
		t.Fatal("retired generation admitted")
	}
}
func TestWarmGateDeadlineAndForeignClock(t *testing.T) {
	ctx := context.Background()
	c, _ := clock.NewVirtual(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), 20)
	g, _ := NewWarmGate(c)
	object := storage.ObjectKey{Bucket: "demo", BucketID: "bucket-1", Key: "file"}
	if _, err := g.Acquire(ctx, object, c.Now().Instant); !errors.Is(err, storage.ErrFenced) {
		t.Fatal(err)
	}
	other, _ := clock.NewVirtual(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC), 20)
	d, _ := other.Now().Instant.Add(time.Minute)
	if _, err := g.Acquire(ctx, object, d); !errors.Is(err, clock.ErrDomain) {
		t.Fatal(err)
	}
	d, _ = c.Now().Instant.Add(time.Second)
	v, err := g.Acquire(ctx, object, d)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(v.Check(ctx), storage.ErrFenced) {
		t.Fatal("deadline equality")
	}
	v.Release()
	if g.resolving != 0 {
		t.Fatal("lost view leaked guard")
	}
	if err = (&WarmView{}).Check(ctx); !errors.Is(err, storage.ErrInvalid) {
		t.Fatal(err)
	}
}
