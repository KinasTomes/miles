package cache_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
)

var ctx = context.Background()

func key(id string, n int64) storage.CacheKey {
	return storage.CacheKey{Pack: storage.PackRef{ID: id, Generation: "g"}, Range: storage.ByteRange{Length: n}}
}
func newCache(t *testing.T, bytes int64, count int, copyBytes int64) *cache.LRU {
	t.Helper()
	c, e := cache.NewLRU(cache.Limits{CapacityBytes: bytes, MaxEntries: count, MaxCopyBytes: copyBytes})
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func put(t *testing.T, c *cache.LRU, k storage.CacheKey, b string) {
	t.Helper()
	if e := c.Put(ctx, k, []byte(b)); e != nil {
		t.Fatal(e)
	}
}
func hit(t *testing.T, c *cache.LRU, k storage.CacheKey, want string, found bool) {
	t.Helper()
	b, h, e := c.Get(ctx, k)
	if e != nil || h != found || string(b) != want {
		t.Fatalf("Get(%v)=%q,%v,%v", k, b, h, e)
	}
}
func TestSharedContract(t *testing.T) {
	contracttest.RunCache(t, func(t *testing.T) storage.Cache { return newCache(t, 64, 16, 64) })
}
func TestLimits(t *testing.T) {
	for _, l := range []cache.Limits{{}, {CapacityBytes: -1, MaxEntries: 1, MaxCopyBytes: 1}, {CapacityBytes: 1, MaxEntries: 0, MaxCopyBytes: 1}, {CapacityBytes: 1, MaxEntries: 1, MaxCopyBytes: 0}} {
		if _, e := cache.NewLRU(l); !errors.Is(e, storage.ErrInvalid) {
			t.Fatal(l, e)
		}
	}
}
func TestLRUAndBudgets(t *testing.T) {
	for _, mode := range []string{"get touch", "duplicate touch", "entry limit", "byte limit"} {
		t.Run(mode, func(t *testing.T) {
			c := newCache(t, 6, 3, 6)
			a, b, d := key("a", 2), key("b", 2), key("d", 2)
			put(t, c, a, "aa")
			put(t, c, b, "bb")
			put(t, c, d, "dd")
			if mode == "get touch" {
				hit(t, c, a, "aa", true)
			}
			if mode == "duplicate touch" {
				put(t, c, a, "aa")
			}
			put(t, c, key("e", 2), "ee")
			if mode == "get touch" || mode == "duplicate touch" {
				hit(t, c, b, "", false)
				hit(t, c, a, "aa", true)
			} else {
				hit(t, c, a, "", false)
			}
		})
	}
	t.Run("multiple evictions and exact capacity", func(t *testing.T) {
		c := newCache(t, 6, 10, 6)
		for _, s := range []string{"a", "b", "c"} {
			put(t, c, key(s, 2), s+s)
		}
		put(t, c, key("large", 5), "12345")
		for _, s := range []string{"a", "b", "c"} {
			hit(t, c, key(s, 2), "", false)
		}
		put(t, c, key("one", 1), "x")
		hit(t, c, key("large", 5), "12345", true)
	})
	t.Run("entry count independent of bytes", func(t *testing.T) {
		c := newCache(t, 100, 1, 100)
		put(t, c, key("a", 1), "a")
		put(t, c, key("b", 1), "b")
		hit(t, c, key("a", 1), "", false)
	})
	for _, limit := range []struct{ capacity, copy int64 }{{4, 8}, {8, 4}} {
		c := newCache(t, limit.capacity, 3, limit.copy)
		put(t, c, key("a", 1), "a")
		put(t, c, key("big", 5), "12345")
		hit(t, c, key("big", 5), "", false)
		hit(t, c, key("a", 1), "a", true)
	}
	c := newCache(t, math.MaxInt64, 2, math.MaxInt64)
	put(t, c, key("a", 1), "a")
	hit(t, c, key("a", 1), "a", true)
}
func TestConflictOwnershipAndRefill(t *testing.T) {
	c := newCache(t, 100, 20, 100)
	a := key("a", 2)
	other := a
	other.Range.Offset = 2
	next := a
	next.Pack.Generation = "next"
	input := []byte("aa")
	if e := c.Put(ctx, a, input); e != nil {
		t.Fatal(e)
	}
	input[0] = 'x'
	owned, _, _ := c.Get(ctx, a)
	owned[0] = 'z'
	hit(t, c, a, "aa", true)
	put(t, c, other, "bb")
	put(t, c, next, "cc")
	if e := c.Put(ctx, a, []byte("xx")); !errors.Is(e, storage.ErrConflict) {
		t.Fatal(e)
	}
	hit(t, c, a, "", false)
	hit(t, c, other, "", false)
	hit(t, c, next, "cc", true)
	if string(owned) != "za" {
		t.Fatal("owned copy changed")
	}
	if e := c.Evict(ctx, next.Pack); e != nil {
		t.Fatal(e)
	}
	put(t, c, next, "cc")
	hit(t, c, next, "cc", true)
}
func TestExactIdentityAndInvalid(t *testing.T) {
	c := newCache(t, 100, 20, 100)
	a := key("a", 4)
	put(t, c, a, "abcd")
	for _, alter := range []func(*storage.CacheKey){func(k *storage.CacheKey) { k.Pack.ID = "b" }, func(k *storage.CacheKey) { k.Pack.Generation = "h" }, func(k *storage.CacheKey) { k.Range.Offset = 1 }, func(k *storage.CacheKey) { k.Range.Length = 3 }} {
		k := a
		alter(&k)
		hit(t, c, k, "", false)
	}
	for _, k := range []storage.CacheKey{{}, key("a", 0), key("a", -1), {Pack: a.Pack, Range: storage.ByteRange{Offset: -1, Length: 1}}, {Pack: a.Pack, Range: storage.ByteRange{Offset: math.MaxInt64, Length: 1}}} {
		if _, _, e := c.Get(ctx, k); !errors.Is(e, storage.ErrInvalid) {
			t.Fatal(k, e)
		}
		if e := c.Put(ctx, k, nil); !errors.Is(e, storage.ErrInvalid) {
			t.Fatal(k, e)
		}
	}
	if e := c.Put(ctx, a, []byte("bad")); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	hit(t, c, a, "abcd", true)
	if e := c.Evict(ctx, storage.PackRef{}); !errors.Is(e, storage.ErrInvalid) {
		t.Fatal(e)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if e := c.Put(cancelled, a, []byte("xxxx")); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, _, e := c.Get(cancelled, a); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e := c.Evict(cancelled, a.Pack); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	hit(t, c, a, "abcd", true)
}
func TestConcurrentOperations(t *testing.T) {
	c := newCache(t, 64, 16, 64)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < 100; i++ {
				k := key(string(rune('a'+w%4)), 4)
				switch w % 3 {
				case 0:
					if e := c.Put(ctx, k, []byte("data")); e != nil {
						t.Error(e)
					}
				case 1:
					b, h, e := c.Get(ctx, k)
					if e != nil || (h && string(b) != "data") {
						t.Errorf("%q %v %v", b, h, e)
					}
				case 2:
					if e := c.Evict(ctx, k.Pack); e != nil {
						t.Error(e)
					}
				}
			}
		}(worker)
	}
	close(start)
	wg.Wait()
}
func TestConcurrentConflictingFills(t *testing.T) {
	c := newCache(t, 20, 4, 20)
	k := key("a", 2)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, s := range []string{"aa", "bb"} {
		go func(s string) { <-start; results <- c.Put(ctx, k, []byte(s)) }(s)
	}
	close(start)
	a, b := <-results, <-results
	if !((a == nil && errors.Is(b, storage.ErrConflict)) || (b == nil && errors.Is(a, storage.ErrConflict))) {
		t.Fatal(a, b)
	}
	hit(t, c, k, "", false)
}

func TestDuplicateFillAccounting(t *testing.T) {
	c := newCache(t, 4, 4, 4)
	k := key("a", 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if e := c.Put(ctx, k, []byte("aa")); e != nil {
				t.Error(e)
			}
		}()
	}
	close(start)
	wg.Wait()
	put(t, c, key("b", 2), "bb")
	hit(t, c, k, "aa", true)
	hit(t, c, key("b", 2), "bb", true)
}

func TestGetEvictOwnedCopy(t *testing.T) {
	c := newCache(t, 8, 4, 8)
	k := key("a", 2)
	put(t, c, k, "aa")
	ready := make(chan []byte, 1)
	evicted := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		b, h, e := c.Get(ctx, k)
		if e != nil || !h {
			t.Error("missing initial entry", e)
		}
		ready <- b
		<-evicted
		if string(b) != "aa" {
			t.Error("eviction changed owned bytes")
		}
	}()
	<-ready
	if e := c.Evict(ctx, k.Pack); e != nil {
		t.Fatal(e)
	}
	close(evicted)
	<-done
	hit(t, c, k, "", false)
}
