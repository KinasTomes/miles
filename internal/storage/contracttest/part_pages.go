package contracttest

import (
	"context"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
)

func runPartPages(t *testing.T, newStore MetadataFactory) {
	t.Run("part pages are isolated immutable fenced staging", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		h := newStore(t, f)
		parts, ok := h.Store.(s.PartPageStore)
		if !ok {
			t.Fatal("missing PartPageStore capability")
		}
		page := s.ManifestPage{Version: 1, ManifestID: "shared-owner", Domain: f.Bucket.Domain,
			Chunks: []s.ChunkRef{{Key: f.Chunk(f.Bucket.Domain).Key, RawBytes: 4}}}
		if err := h.Store.WriteManifestPages(ctx, []s.ManifestPage{page}, f.Lease.Fence); err != nil {
			t.Fatal(err)
		}
		page.Chunks[0].RawBytes = 5
		for range 2 {
			if err := parts.WritePartPages(ctx, []s.ManifestPage{page}, f.Lease.Fence); err != nil {
				t.Fatal(err)
			}
		}
		object, err := h.Store.GetManifest(ctx, page.ManifestID)
		if err != nil || object[0].Chunks[0].RawBytes != 4 {
			t.Fatal("object/part namespace collision", object, err)
		}
		got, err := parts.GetPartPage(ctx, page.ManifestID, 0)
		if err != nil || got.Chunks[0].RawBytes != 5 {
			t.Fatal(got, err)
		}
		got.Chunks[0].RawBytes = 6
		requireError(t, parts.WritePartPages(ctx, []s.ManifestPage{got}, f.Lease.Fence), s.ErrConflict)
		again, err := parts.GetPartPage(ctx, page.ManifestID, 0)
		if err != nil || again.Chunks[0].RawBytes != 5 {
			t.Fatal("returned page aliases state", again, err)
		}
		bad := page
		bad.ManifestID = "too-large-part"
		bad.Chunks = []s.ChunkRef{{Key: page.Chunks[0].Key, RawBytes: 4, LogicalOffset: 5 << 30}}
		requireError(t, parts.WritePartPages(ctx, []s.ManifestPage{bad}, f.Lease.Fence), s.ErrInvalid)
		writer := f.Lease
		writer.Fence.Epoch++
		h.SetWriter(writer)
		requireError(t, parts.WritePartPages(ctx, []s.ManifestPage{page}, f.Lease.Fence), s.ErrFenced)
		_, err = h.Store.GetObject(ctx, f.Request("unpublished-part", 0).Head.Object)
		requireError(t, err, s.ErrNotFound)
	})
}
