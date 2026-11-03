package readpath

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
)

func TestCacheNodeLossRefillsFromDurableOrigin(t *testing.T) {
	f, old, _, root := twoTierFixture(t, 0)
	ctx := context.Background()
	if _, out, err := f.read(ctx, f.request); err != nil || !bytes.Equal(out, f.body) {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	memory, _ := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 16, MaxCopyBytes: 1 << 20})
	disk, err := cache.NewDisk(root, cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 16, MaxCopyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := cache.NewNode("replacement-node", memory, disk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fresh.Close() })
	f.reader.options.Cache = fresh
	before := f.backend.count()
	if _, out, err := f.read(ctx, f.request); err != nil || !bytes.Equal(out, f.body) || f.backend.count() == before {
		t.Fatal("node loss affected durable data/refill", err)
	}
	before = f.backend.count()
	if _, out, err := f.read(ctx, f.request); err != nil || !bytes.Equal(out, f.body) || f.backend.count() != before {
		t.Fatal("replacement did not cache verified bytes", err)
	}
}

func TestCacheGenerationAfterCompactionPublication(t *testing.T) {
	f, _, _, _ := twoTierFixture(t, 0)
	ctx := context.Background()
	if _, out, err := f.read(ctx, f.request); err != nil || !bytes.Equal(out, f.body) {
		t.Fatal(err)
	}
	seed := contracttest.NewFixture()
	next := f.ref
	next.Generation = "compacted-generation"
	info := storage.PackInfo{Version: 1, Ref: next, State: "Uploading", Class: storage.Standard, Size: int64(len(f.backend.body)), StateRevision: 1, Writer: seed.Lease.Fence, ProtectedUntil: seed.Now.Add(time.Hour)}
	if err := f.metadata.RegisterUploadingPack(ctx, info, seed.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	if _, err := f.metadata.MarkPackWarm(ctx, next, 1, seed.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	// Model the compactor's already verified destination and real canonical
	// location CAS. The physical seam serves the same immutable entry bytes;
	// this tests the cache consumer, not the downstream compaction algorithm.
	for _, entry := range f.entries[:2] {
		if _, err := f.metadata.CompareAndSwapChunkLocation(ctx, entry.Key, 1, entry.Location(next), seed.Lease.Fence); err != nil {
			t.Fatal(err)
		}
	}
	before := f.backend.count()
	if _, out, err := f.read(ctx, f.request); err != nil || !bytes.Equal(out, f.body) || f.backend.count() == before {
		t.Fatal("old cached generation satisfied new location", err)
	}
	before = f.backend.count()
	if _, out, err := f.read(ctx, f.request); err != nil || !bytes.Equal(out, f.body) || f.backend.count() != before {
		t.Fatal("new generation did not cache", err)
	}
}
