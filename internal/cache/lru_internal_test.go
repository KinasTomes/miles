package cache

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func checkState(t *testing.T, c *LRU) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var total int64
	seen := map[storage.CacheKey]bool{}
	for e := c.order.Front(); e != nil; e = e.Next() {
		v := e.Value.(*entry)
		if seen[v.key] || c.entries[v.key] != e {
			t.Fatal("list/map disagreement")
		}
		seen[v.key] = true
		total += int64(len(v.data))
	}
	if total != c.residentBytes || total > c.limits.CapacityBytes || len(seen) != len(c.entries) || len(seen) > c.limits.MaxEntries {
		t.Fatalf("accounting: bytes=%d/%d count=%d", total, c.residentBytes, len(seen))
	}
}

// Signal the first Err call, then let the operation block on the held mutex.
// Cancellation is established before unlocking, independently of scheduling.
type observedContext struct {
	context.Context
	once    sync.Once
	checked chan struct{}
}

func (c *observedContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}
func TestCancellationAtLockBoundary(t *testing.T) {
	for _, op := range []string{"get", "put", "evict"} {
		t.Run(op, func(t *testing.T) {
			c, _ := NewLRU(Limits{8, 4, 8})
			k := storage.CacheKey{Pack: storage.PackRef{ID: "p", Generation: "g"}, Range: storage.ByteRange{Length: 2}}
			_ = c.Put(context.Background(), k, []byte("aa"))
			base, cancel := context.WithCancel(context.Background())
			ctx := &observedContext{Context: base, checked: make(chan struct{})}
			done := make(chan error, 1)
			c.mu.Lock()
			go func() {
				switch op {
				case "get":
					_, _, e := c.Get(ctx, k)
					done <- e
				case "put":
					done <- c.Put(ctx, k, []byte("xx"))
				case "evict":
					done <- c.Evict(ctx, k.Pack)
				}
			}()
			<-ctx.checked
			cancel()
			c.mu.Unlock()
			if e := <-done; !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
			b, h, e := c.Get(context.Background(), k)
			if e != nil || !h || string(b) != "aa" {
				t.Fatal(string(b), h, e)
			}
			checkState(t, c)
		})
	}
}
func TestConcurrentAccounting(t *testing.T) {
	c, _ := NewLRU(Limits{17, 5, 17})
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				k := storage.CacheKey{Pack: storage.PackRef{ID: string(rune('a' + w)), Generation: "g"}, Range: storage.ByteRange{Offset: int64(i % 7), Length: 3}}
				_ = c.Put(context.Background(), k, []byte("abc"))
				_, _, _ = c.Get(context.Background(), k)
				if i%3 == 0 {
					_ = c.Evict(context.Background(), k.Pack)
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	checkState(t, c)
}

func TestIntegrityConflictAccounting(t *testing.T) {
	c, err := NewLRU(Limits{CapacityBytes: 6, MaxEntries: 8, MaxCopyBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	g1 := storage.PackRef{ID: "pack", Generation: "g1"}
	g2 := storage.PackRef{ID: "pack", Generation: "g2"}
	a := storage.CacheKey{Pack: g1, Range: storage.ByteRange{Length: 2}}
	b := storage.CacheKey{Pack: g1, Range: storage.ByteRange{Offset: 2, Length: 2}}
	keep := storage.CacheKey{Pack: g2, Range: storage.ByteRange{Length: 2}}
	for _, item := range []struct {
		key  storage.CacheKey
		body string
	}{{a, "aa"}, {b, "bb"}, {keep, "cc"}} {
		if err := c.Put(ctx, item.key, []byte(item.body)); err != nil {
			t.Fatal(err)
		}
	}
	checkState(t, c)
	if err := c.Put(ctx, a, []byte("xx")); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("conflict = %v", err)
	}
	for _, key := range []storage.CacheKey{a, b} {
		if data, hit, err := c.Get(ctx, key); err != nil || hit || data != nil {
			t.Fatalf("G1 survived conflict: %q, %v, %v", data, hit, err)
		}
	}
	if data, hit, err := c.Get(ctx, keep); err != nil || !hit || string(data) != "cc" {
		t.Fatalf("G2 after conflict: %q, %v, %v", data, hit, err)
	}
	checkState(t, c)
	added := storage.CacheKey{Pack: g2, Range: storage.ByteRange{Offset: 2, Length: 4}}
	if err := c.Put(ctx, added, []byte("dddd")); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		key  storage.CacheKey
		body string
	}{{keep, "cc"}, {added, "dddd"}} {
		if data, hit, err := c.Get(ctx, item.key); err != nil || !hit || string(data) != item.body {
			t.Fatalf("capacity reuse: %q, %v, %v; want %q", data, hit, err, item.body)
		}
	}
	c.mu.Lock()
	resident, count := c.residentBytes, len(c.entries)
	c.mu.Unlock()
	if resident != 6 || count != 2 {
		t.Fatalf("after refill: bytes=%d entries=%d; want 6, 2", resident, count)
	}
	checkState(t, c)
}
