package contracttest

import (
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/stats"
	s "github.com/KinasTomes/miles/internal/storage"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func RunObservationCAS(t *testing.T, factory ObservationFactory) {
	ctx := context.Background()
	f := shared.NewFixture()
	m := factory(t, f)
	start := make(chan struct{})
	var wins atomic.Int32
	var wg sync.WaitGroup
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			c := f.Chunk("default")
			c.Location.Pack = f.Packs[n].Ref
			_, inserted, err := m.PutChunkIfAbsent(ctx, c, f.Lease.Fence)
			if err != nil {
				t.Error(err)
			}
			if inserted {
				wins.Add(1)
			}
		}(n)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("insert winners", wins.Load())
	}
	rows, err := m.GetChunks(ctx, []s.ChunkKey{f.Chunk("default").Key}, true)
	if err != nil {
		t.Fatal(err)
	}
	canonical := rows[f.Chunk("default").Key]
	wins.Store(0)
	start = make(chan struct{})
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			<-start
			next := canonical.Location
			next.Pack = f.Packs[n].Ref
			next.Range.Offset = int64(8 + n*8)
			_, err := m.CompareAndSwapChunkLocation(ctx, canonical.Key, 1, next, f.Lease.Fence)
			if err == nil {
				wins.Add(1)
			} else if !errors.Is(err, s.ErrConflict) {
				t.Error(err)
			}
		}(n)
	}
	close(start)
	wg.Wait()
	if wins.Load() != 1 {
		t.Fatal("CAS winners", wins.Load())
	}
	r := f.Request("stats-cas", 0)
	shared.Prepare(t, m, f, r, 4)
	if _, err = m.CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
	inv := &observationInventory{inventory: s.PhysicalInventory{Complete: true, VersionScope: "all-retained", Objects: []s.PhysicalObject{{Key: "pack-a", Category: "pack", Pack: f.Packs[0].Ref, Current: true, Size: 64}, {Key: "pack-b", Category: "pack", Pack: f.Packs[1].Ref, Current: true, Size: 64}}}}
	svc, err := stats.New(stats.Options{Store: m, Inventory: inv, Now: func() time.Time { return f.Now }, MaxRecords: 100, MaxReferences: 100, MaxPhysicalObjects: 100, Timeout: time.Minute, MaxAge: time.Minute, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	q0, _ := m.GetQuota(ctx, "owner")
	if err = svc.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	g, err := svc.Global(ctx)
	if err != nil || g.BytesUniqueRaw != 4 || g.BytesUniqueStored != 4 || *g.BytesStored != 128 || *g.Unallocated != 124 {
		t.Fatal("loser physical lost or unique double-counted", g, err)
	}
	q1, _ := m.GetQuota(ctx, "owner")
	if q0 != q1 {
		t.Fatal("physical bytes changed logical quota", q0, q1)
	}
	// Move canonical metadata DURING physical collection. The in-progress
	// observation must fail, retaining the last good report rather than mixing
	// old stored ranges with new physical generation facts.
	inv.before = func() {
		next := canonical.Location
		next.Pack = f.Packs[0].Ref
		next.Range.Offset = 32
		if _, err := m.CompareAndSwapChunkLocation(ctx, canonical.Key, 2, next, f.Lease.Fence); err != nil {
			t.Fatal(err)
		}
	}
	if err = svc.Refresh(ctx); !errors.Is(err, s.ErrConflict) {
		t.Fatal("canonical movement was silently measured", err)
	}
	retained, err := svc.Global(ctx)
	if err != nil || retained.SnapshotID != g.SnapshotID {
		t.Fatal("failed refresh replaced valid report", retained, err)
	}
	inv.before = nil
	if err = svc.Refresh(ctx); err != nil {
		t.Fatal("stable post-CAS collection", err)
	}
}
