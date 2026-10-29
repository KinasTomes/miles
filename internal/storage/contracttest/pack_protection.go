package contracttest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

func runPackProtection(t *testing.T, factory MetadataFactory) {
	t.Run("atomic-max-protection-fence-binding-and-state", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		f.Packs[0].StateRevision = 1
		f.Packs[0].ProtectedUntil = f.Now.Add(time.Minute)
		h := factory(t, f)
		store := h.Store.(s.PackProtectionStore)
		row := f.Chunk("default")
		if _, _, e := h.Store.PutChunkIfAbsent(ctx, row, f.Lease.Fence); e != nil {
			t.Fatal(e)
		}
		r := s.PackProtectionUpdate{Chunk: row, ExpectedStateRevision: 1, RequiredUntil: f.Now.Add(2 * time.Minute), Writer: f.Lease.Fence}
		out, e := store.ExtendPackProtection(ctx, r)
		if e != nil || out.StateRevision != 1 || !out.ProtectedUntil.Equal(r.RequiredUntil) {
			t.Fatal(out, e)
		}
		if rows, e := h.Store.GetChunks(ctx, []s.ChunkKey{row.Key}, true); e != nil || rows[row.Key] != row {
			t.Fatal("protection changed chunk identity/location", rows, e)
		}
		r.ExpectedStateRevision = 1
		r.RequiredUntil = f.Now.Add(90 * time.Second)
		unchanged, e := store.ExtendPackProtection(ctx, r)
		if e != nil || !s.SamePackInfo(unchanged, out) {
			t.Fatal("protection decreased", unchanged, e)
		}
		for _, tc := range []struct {
			name string
			edit func(*s.PackProtectionUpdate)
			want error
		}{
			{"stale fence", func(r *s.PackProtectionUpdate) { r.Writer.Epoch++ }, s.ErrFenced},
			{"incarnation", func(r *s.PackProtectionUpdate) { r.Writer.ServiceIncarnation = "stale" }, s.ErrFenced},
			{"state revision", func(r *s.PackProtectionUpdate) { r.ExpectedStateRevision = 2 }, s.ErrConflict},
			{"location revision", func(r *s.PackProtectionUpdate) { r.Chunk.LocationRevision++ }, s.ErrConflict},
			{"missing generation", func(r *s.PackProtectionUpdate) { r.Chunk.Location.Pack.Generation = "other" }, s.ErrNotFound},
			{"wrong range", func(r *s.PackProtectionUpdate) { r.Chunk.Location.Range.Offset++ }, s.ErrConflict},
			{"missing target", func(r *s.PackProtectionUpdate) { r.RequiredUntil = time.Time{} }, s.ErrInvalid},
		} {
			bad := r
			tc.edit(&bad)
			_, e := store.ExtendPackProtection(ctx, bad)
			if !errors.Is(e, tc.want) {
				t.Fatal(tc.name, e)
			}
			packs, _ := h.Store.GetPacks(ctx, []s.PackRef{out.Ref})
			if !s.SamePackInfo(packs[out.Ref], out) {
				t.Fatal("failed operation mutated pack", tc.name)
			}
		}
		for _, state := range []string{"Transitioning", "Retiring", "Restored", "Deleted"} {
			blocked := out
			blocked.State = state
			blocked.Class = s.Glacier
			h.SetPack(blocked)
			if _, e := store.ExtendPackProtection(ctx, r); !errors.Is(e, s.ErrConflict) {
				t.Fatal("unsafe state", state, e)
			}
		}
		h.SetPack(out)
		// Both requests observe revision2. One may conflict; explicit re-read and
		// revalidation of that losing proposal must never reduce a larger grant.
		start := make(chan struct{})
		type result struct {
			request s.PackProtectionUpdate
			err     error
		}
		results := make(chan result, 2)
		var wg sync.WaitGroup
		for _, d := range []time.Duration{3 * time.Minute, 4 * time.Minute} {
			candidate := r
			candidate.RequiredUntil = f.Now.Add(d)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, e := store.ExtendPackProtection(ctx, candidate)
				results <- result{candidate, e}
			}()
		}
		close(start)
		wg.Wait()
		close(results)
		for result := range results {
			if result.err == nil {
				continue
			}
			if !errors.Is(result.err, s.ErrConflict) {
				t.Fatal(result.err)
			}
			packs, _ := h.Store.GetPacks(ctx, []s.PackRef{out.Ref})
			result.request.ExpectedStateRevision = packs[out.Ref].StateRevision
			if _, e := store.ExtendPackProtection(ctx, result.request); e != nil {
				t.Fatal(e)
			}
		}
		packs, _ := h.Store.GetPacks(ctx, []s.PackRef{out.Ref})
		if !packs[out.Ref].ProtectedUntil.Equal(f.Now.Add(4 * time.Minute)) {
			t.Fatal("lost max", packs)
		}
		rows, _ := h.Store.GetChunks(ctx, []s.ChunkKey{row.Key}, true)
		if !reflect.DeepEqual(rows[row.Key], row) {
			t.Fatal("location changed")
		}
	})
}
