package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
)

func testKey(id, g string) storage.CacheKey {
	return storage.CacheKey{Pack: storage.PackRef{ID: id, Generation: g}, Range: storage.ByteRange{Length: 2}}
}
func testNode(t *testing.T) *Node {
	t.Helper()
	m, _ := NewLRU(Limits{2, 1, 4})
	d, err := NewDisk(t.TempDir(), Limits{6, 3, 4})
	if err != nil {
		t.Fatal(err)
	}
	n, err := NewNode("node-1", m, d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := n.Close(); err != nil {
			t.Error(err)
		}
	})
	return n
}
func TestNodeSharedCacheContract(t *testing.T) {
	contracttest.RunCache(t, func(t *testing.T) storage.Cache {
		m, _ := NewLRU(Limits{1024, 128, 1024})
		d, err := NewDisk(t.TempDir(), Limits{1024, 128, 1024})
		if err != nil {
			t.Fatal(err)
		}
		n, _ := NewNode("node", m, d)
		t.Cleanup(func() { _ = n.Close() })
		return n
	})
}
func TestDiskBoundedLRUAndCorruption(t *testing.T) {
	ctx := context.Background()
	d, err := NewDisk(t.TempDir(), Limits{4, 2, 4})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	a, b, c := testKey("a", "g"), testKey("b", "g"), testKey("c", "g")
	for _, k := range []storage.CacheKey{a, b} {
		if err = d.Put(ctx, k, []byte("aa")); err != nil {
			t.Fatal(err)
		}
	}
	if _, hit, err := d.Get(ctx, a); err != nil || !hit {
		t.Fatal(hit, err)
	}
	if err = d.Put(ctx, c, []byte("cc")); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := d.Get(ctx, b); err != nil || hit {
		t.Fatal("LRU victim", hit, err)
	}
	used, count, evictions := d.Occupancy()
	if used != 4 || count != 2 || evictions != 1 {
		t.Fatal(used, count, evictions)
	}
	large := a
	large.Range.Length = 5
	if err = d.Put(ctx, large, []byte("12345")); err != nil {
		t.Fatal(err)
	}
	if _, hit, _ := d.Get(ctx, a); !hit {
		t.Fatal("oversize evicted useful entry")
	}
	if err = os.WriteFile(d.path(a), []byte("xx"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = d.Get(ctx, a); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("corruption accepted", err)
	}
}
func TestNodePromotionStatsGenerationAndPurge(t *testing.T) {
	ctx := context.Background()
	n := testNode(t)
	a, b := testKey("p", "g1"), testKey("p", "g2")
	for _, k := range []storage.CacheKey{a, b} {
		if err := n.Put(ctx, k, []byte("aa")); err != nil {
			t.Fatal(err)
		}
	}
	if _, hit, err := n.Get(ctx, a); err != nil || !hit {
		t.Fatal("disk promotion", err)
	}
	if err := n.Evict(ctx, a.Pack); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := n.Get(ctx, a); err != nil || hit {
		t.Fatal("generation survived eviction", err)
	}
	if _, hit, err := n.Get(ctx, b); err != nil || !hit {
		t.Fatal("other generation evicted", err)
	}
	s := n.Stats()
	if s.Scope != "node" || s.NodeID != "node-1" || s.Lookups != 3 || s.Hits != 2 || s.Misses != 1 || s.HitRatio != float64(2)/3 || s.Memory.UsedBytes != 2 || s.Disk.UsedBytes != 2 {
		t.Fatalf("stats %+v", s)
	}
	if err := n.PurgeNode(ctx, "other"); !errors.Is(err, storage.ErrInvalid) {
		t.Fatal("wrong scope", err)
	}
	if err := n.PurgeNode(ctx, "node-1"); err != nil {
		t.Fatal(err)
	}
	s = n.Stats()
	if s.Memory.UsedBytes != 0 || s.Disk.UsedBytes != 0 || s.Hits != 2 {
		t.Fatal("purge/reset methodology", s)
	}
	// Purge is resident-only. A new valid fill may re-admit the same generation.
	if err := n.Put(ctx, b, []byte("aa")); err != nil {
		t.Fatal(err)
	}
	if _, hit, _ := n.Get(ctx, b); !hit {
		t.Fatal("purge incorrectly blacklisted generation")
	}
}
func TestNodeConflictEvictsBothTiers(t *testing.T) {
	ctx := context.Background()
	n := testNode(t)
	a := testKey("p", "g")
	b := a
	b.Range.Offset = 2
	_ = n.Put(ctx, a, []byte("aa"))
	_ = n.Put(ctx, b, []byte("bb"))
	if err := n.Put(ctx, a, []byte("xx")); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	for _, k := range []storage.CacheKey{a, b} {
		if _, hit, err := n.Get(ctx, k); err != nil || hit {
			t.Fatal("conflicting generation survived", hit, err)
		}
	}
}
func TestDiskOwnershipAndClose(t *testing.T) {
	root := t.TempDir()
	d, err := NewDisk(root, Limits{4, 2, 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = NewDisk(root, Limits{4, 2, 4}); err == nil {
		t.Fatal("concurrent directory owner")
	}
	if err = d.Put(context.Background(), testKey("a", "g"), []byte("aa")); err != nil {
		t.Fatal(err)
	}
	if err = d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = NewDisk(root, Limits{4, 2, 4})
	if err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	if err = os.WriteFile(filepath.Join(root, "user-file"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = NewDisk(root, Limits{4, 2, 4}); !errors.Is(err, storage.ErrInvalid) {
		t.Fatal("foreign files accepted", err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "user-file")); string(b) != "keep" {
		t.Fatal("foreign file removed")
	}
}
func TestNodeConcurrentPurgeAndFillBudgets(t *testing.T) {
	n := testNode(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 30; i++ {
				k := testKey(string(rune('a'+i%7)), "g")
				_ = n.Put(ctx, k, []byte("aa"))
				_, _, _ = n.Get(ctx, k)
				if i%5 == 0 {
					_ = n.PurgeNode(ctx, "node-1")
				}
			}
		}()
	}
	wg.Wait()
	s := n.Stats()
	if s.Memory.UsedBytes > 2 || s.Memory.Entries > 1 || s.Disk.UsedBytes > 6 || s.Disk.Entries > 3 {
		t.Fatal("budget exceeded", s)
	}
}

type purgeMock struct{ calls []string }

func TestNodeCancellationAndClose(t *testing.T) {
	n := testNode(t)
	k := testKey("a", "g")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := n.Put(ctx, k, []byte("aa")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := n.Get(ctx, k); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := n.PurgeNode(ctx, "node-1"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s := n.Stats()
	if s.Puts != 0 || s.Lookups != 0 || s.Memory.UsedBytes != 0 || s.Disk.UsedBytes != 0 {
		t.Fatal("canceled operation mutated cache", s)
	}
	if err := n.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := n.Get(context.Background(), k); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal("closed node served", err)
	}
}

func (m *purgeMock) PurgeNode(_ context.Context, id string) error {
	m.calls = append(m.calls, id)
	if id == "failed" {
		return storage.ErrUnavailable
	}
	return nil
}
func TestFanoutPurgePartialFailureAndCancellation(t *testing.T) {
	m := &purgeMock{}
	results, err := FanoutPurge(context.Background(), []string{"a", "failed", "c"}, m)
	if err != nil || len(results) != 3 || results[0].Err != nil || !errors.Is(results[1].Err, storage.ErrUnavailable) || results[2].Err != nil {
		t.Fatal(results, err)
	}
	if _, err = FanoutPurge(context.Background(), []string{"a", "a"}, m); !errors.Is(err, storage.ErrInvalid) || len(m.calls) != 3 {
		t.Fatal("duplicate dispatched", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	results, err = FanoutPurge(ctx, []string{"a", "c"}, m)
	if err != nil || len(m.calls) != 3 || !errors.Is(results[0].Err, context.Canceled) || !errors.Is(results[1].Err, context.Canceled) {
		t.Fatal("canceled targets missing", results, err)
	}
}
