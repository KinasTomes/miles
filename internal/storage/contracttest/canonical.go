package contracttest

import (
	"context"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
)

func runCanonicalDestinations(t *testing.T, factory MetadataFactory) {
	ctx := context.Background()
	t.Run("canonical destination preserves errors", func(t *testing.T) {
		for _, name := range []string{"missing-pack", "invalid-range", "lost-fence"} {
			t.Run(name, func(t *testing.T) {
				f := NewFixture()
				h := factory(t, f)
				c := f.Chunk("default")
				if _, _, err := h.Store.PutChunkIfAbsent(ctx, c, f.Lease.Fence); err != nil {
					t.Fatal(err)
				}
				next := c.Location
				next.Pack = f.Packs[1].Ref
				writer := f.Lease.Fence
				want := s.ErrNotFound
				switch name {
				case "missing-pack":
					next.Pack.ID = "missing"
				case "invalid-range":
					next.Range.Offset = f.Packs[1].Size
					want = s.ErrInvalid
				case "lost-fence":
					writer.Epoch++
					want = s.ErrFenced
				}
				candidate := c
				candidate.Key.Domain = "new-domain"
				candidate.Location = next
				_, won, err := h.Store.PutChunkIfAbsent(ctx, candidate, writer)
				requireError(t, err, want)
				if won {
					t.Fatal("failed insert won")
				}
				_, err = h.Store.CompareAndSwapChunkLocation(ctx, c.Key, 1, next, writer)
				requireError(t, err, want)
				got, err := h.Store.GetChunks(ctx, []s.ChunkKey{c.Key, candidate.Key}, true)
				if err != nil || len(got) != 1 || got[c.Key] != c {
					t.Fatalf("rejected mutations changed records: %+v %v", got, err)
				}
			})
		}
	})
	t.Run("canonical destination requires Warm", func(t *testing.T) {
		for _, state := range []string{"Uploading", "Cold", "Restoring", "Restored", "Compacting", "Transitioning", "Retiring", "Retired", "Deleting", "Deleted", "", "Unknown"} {
			t.Run(state, func(t *testing.T) {
				f := NewFixture()
				f.Packs[1].State = state
				h := factory(t, f)
				candidate := f.Chunk("default")
				candidate.Location.Pack = f.Packs[1].Ref
				_, won, err := h.Store.PutChunkIfAbsent(ctx, candidate, f.Lease.Fence)
				requireError(t, err, s.ErrConflict)
				if won {
					t.Fatal("rejected insert won")
				}
				got, err := h.Store.GetChunks(ctx, []s.ChunkKey{candidate.Key}, true)
				if err != nil || len(got) != 0 {
					t.Fatalf("rejected insert persisted: %+v %v", got, err)
				}
				original := f.Chunk("default")
				if _, won, err := h.Store.PutChunkIfAbsent(ctx, original, f.Lease.Fence); err != nil || !won {
					t.Fatalf("Warm insert: %v %v", won, err)
				}
				_, err = h.Store.CompareAndSwapChunkLocation(ctx, original.Key, 1, candidate.Location, f.Lease.Fence)
				requireError(t, err, s.ErrConflict)
				got, err = h.Store.GetChunks(ctx, []s.ChunkKey{original.Key}, true)
				if err != nil || got[original.Key] != original {
					t.Fatalf("rejected CAS changed canonical: %+v %v", got, err)
				}
			})
		}
	})
	t.Run("canonical Cold winner remains available for revival", func(t *testing.T) {
		f := NewFixture()
		h := factory(t, f)
		original := f.Chunk("default")
		if _, _, err := h.Store.PutChunkIfAbsent(ctx, original, f.Lease.Fence); err != nil {
			t.Fatal(err)
		}
		cold := f.Packs[0]
		cold.State = "Cold"
		cold.Class = s.GlacierIR
		h.SetPack(cold)
		// Both the existing winner and the proposed destination are Cold. No new
		// pointer is published; destination admission must not precede this return.
		winner, won, err := h.Store.PutChunkIfAbsent(ctx, original, f.Lease.Fence)
		if err != nil || won || winner != original {
			t.Fatalf("Cold winner hidden: %+v %v %v", winner, won, err)
		}
		got, err := h.Store.GetChunks(ctx, []s.ChunkKey{original.Key}, true)
		if err != nil || got[original.Key] != original {
			t.Fatalf("Cold lookup: %+v %v", got, err)
		}
		next := original.Location
		next.Pack = f.Packs[1].Ref
		revived, err := h.Store.CompareAndSwapChunkLocation(ctx, original.Key, 1, next, f.Lease.Fence)
		if err != nil || revived.Location != next || revived.LocationRevision != 2 {
			t.Fatalf("Warm revival: %+v %v", revived, err)
		}
	})
}
