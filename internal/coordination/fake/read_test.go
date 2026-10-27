package fake

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

func fixture(t *testing.T) (*Coordinator, *clock.Virtual, storage.ObjectHead, storage.Chunk, storage.PackRef) {
	t.Helper()
	c, e := clock.NewVirtual(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), 100)
	if e != nil {
		t.Fatal(e)
	}
	h := storage.ObjectHead{Version: 1, Object: storage.ObjectKey{Bucket: "b", BucketID: "bucket-1", Key: "k"}, Revision: 1, Domain: "default", ManifestID: "m1"}
	x := storage.Chunk{Version: 1, Key: storage.ChunkKey{Domain: "default", Hash: strings.Repeat("a", 64)}, RawBytes: 1, LocationRevision: 1, Location: storage.ChunkLocation{Pack: storage.PackRef{ID: "p", Generation: "g1"}, Range: storage.ByteRange{Length: 1}, Codec: "raw"}}
	p := storage.PackRef{ID: "p", Generation: "g2"}
	m, e := New(c, "service-1", 2, Seed{Heads: []storage.ObjectHead{h}, Roots: map[string][]storage.ChunkKey{"m1": {x.Key}, "m2": {x.Key}}, Chunks: []storage.Chunk{x}, Packs: []storage.PackRef{x.Location.Pack, p}})
	if e != nil {
		t.Fatal(e)
	}
	return m, c, h, x, p
}
func acquire(t *testing.T, m *Coordinator, c *clock.Virtual, h storage.ObjectHead, id string) View {
	t.Helper()
	d, e := c.Now().Instant.Add(time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	v, e := m.Acquire(id, h.Object, d)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func want(t *testing.T, e, target error) {
	t.Helper()
	if !errors.Is(e, target) {
		t.Fatalf("got %v want %v", e, target)
	}
}
func pin(t *testing.T, m *Coordinator, v View, x storage.Chunk) {
	t.Helper()
	must(t, m.Pin(v, "batch", []Location{{x.Key, x.LocationRevision, x.Location.Pack}}))
}

func TestGuardRootPinAndRelease(t *testing.T) {
	m, c, h, x, _ := fixture(t)
	v := acquire(t, m, c, h, "v")
	want(t, m.Retire(x.Location.Pack), ErrBusy)
	must(t, m.ChangeHead(h.Object, 1, "m2", false))
	want(t, m.BindRoot(v, 1, "m1"), ErrConflict)
	want(t, m.ReclaimRoot("m1"), ErrBusy) // failed bind retains guard
	must(t, m.BindRoot(v, 2, "m2"))
	want(t, m.BindRoot(v, 1, "m2"), ErrConflict)
	pin(t, m, v, x)
	w, e := m.BeginWork(v, x.Location.Pack)
	must(t, e)
	must(t, m.ChangeHead(h.Object, 2, "", true))
	must(t, m.ValidateWork(w)) // bound view survives DELETE
	must(t, m.BeginRelease(v))
	want(t, m.FinishRelease(v), ErrBusy)
	want(t, m.Retire(x.Location.Pack), ErrBusy)
	_, e = m.BeginWork(v, x.Location.Pack)
	want(t, e, ErrLost)
	copyWork := *w
	must(t, m.FinishWork(w))
	must(t, m.FinishWork(&copyWork))
	must(t, m.FinishRelease(v))
	must(t, m.FinishRelease(v))
	must(t, m.Retire(x.Location.Pack))
	must(t, m.ReclaimRoot("m2"))
}

func TestPinRetireOrderings(t *testing.T) {
	for _, first := range []string{"pin", "retire"} {
		t.Run(first, func(t *testing.T) {
			m, c, h, x, _ := fixture(t)
			v := acquire(t, m, c, h, "v")
			must(t, m.BindRoot(v, 1, "m1"))
			// The barrier determines LP order, not scheduler timing or wall sleeps.
			done := make(chan struct{})
			var a, b error
			var wg sync.WaitGroup
			wg.Add(2)
			p := func() error { return m.Pin(v, "batch", []Location{{x.Key, 1, x.Location.Pack}}) }
			go func() {
				defer wg.Done()
				if first == "pin" {
					a = p()
				} else {
					a = m.Retire(x.Location.Pack)
				}
				close(done)
			}()
			go func() {
				defer wg.Done()
				<-done
				if first == "pin" {
					b = m.Retire(x.Location.Pack)
				} else {
					b = p()
				}
			}()
			wg.Wait()
			must(t, a)
			if first == "pin" {
				want(t, b, ErrBusy)
			} else {
				want(t, b, ErrConflict)
			}
		})
	}
}

func TestCompactionAndAtomicBatch(t *testing.T) {
	m, c, h, x, next := fixture(t)
	v := acquire(t, m, c, h, "v")
	must(t, m.BindRoot(v, 1, "m1"))
	bad := Location{x.Key, 2, x.Location.Pack}
	want(t, m.Pin(v, "bad", []Location{{x.Key, 1, x.Location.Pack}, bad}), ErrConflict)
	if len(v.state.pins) != 0 {
		t.Fatal("partial pin batch")
	}
	pin(t, m, v, x)
	pin(t, m, v, x)
	l := x.Location
	l.Pack = next
	must(t, m.Move(x.Key, 1, l))
	w, e := m.BeginWork(v, x.Location.Pack)
	must(t, e)
	must(t, m.FinishWork(w))
	want(t, m.Retire(x.Location.Pack), ErrBusy)
	v2 := acquire(t, m, c, h, "v2")
	must(t, m.BindRoot(v2, 1, "m1"))
	want(t, m.Pin(v2, "stale", []Location{{x.Key, 1, x.Location.Pack}}), ErrConflict)
	must(t, m.Pin(v2, "fresh", []Location{{x.Key, 2, next}}))
	must(t, m.BeginRelease(v))
	must(t, m.FinishRelease(v))
	must(t, m.Retire(x.Location.Pack))
	want(t, m.Retire(next), ErrBusy)
}

func TestLostAuthorityRetainsProtection(t *testing.T) {
	for _, reason := range []string{"deadline", "restart", "bucket"} {
		t.Run(reason, func(t *testing.T) {
			m, c, h, x, _ := fixture(t)
			v := acquire(t, m, c, h, "v")
			must(t, m.BindRoot(v, 1, "m1"))
			pin(t, m, v, x)
			w, e := m.BeginWork(v, x.Location.Pack)
			must(t, e)
			switch reason {
			case "deadline":
				must(t, c.Advance(time.Minute))
			case "restart":
				must(t, m.Restart("service-2"))
			case "bucket":
				m.InvalidateBucket(h.Object.BucketID)
			}
			want(t, m.ValidateWork(w), ErrLost)
			want(t, m.Retire(x.Location.Pack), ErrBusy)
			must(t, m.BeginRelease(v))
			want(t, m.FinishRelease(v), ErrBusy)
			must(t, m.FinishWork(w))
			must(t, m.FinishRelease(v))
			must(t, m.Retire(x.Location.Pack))
		})
	}
}

func TestForeignHandleDomainAndConcurrentRelease(t *testing.T) {
	m, c, h, x, _ := fixture(t)
	other, oc, _, _, _ := fixture(t)
	v := acquire(t, m, c, h, "v")
	want(t, other.BindRoot(v, 1, "m1"), ErrInvalid)
	d, _ := oc.Now().Instant.Add(time.Minute)
	_, e := m.Acquire("foreign-clock", h.Object, d)
	want(t, e, clock.ErrDomain)
	must(t, m.BindRoot(v, 1, "m1"))
	pin(t, m, v, x)
	w, e := m.BeginWork(v, x.Location.Pack)
	must(t, e)
	must(t, m.BeginRelease(v))
	want(t, m.Pin(v, "late", []Location{{x.Key, 1, x.Location.Pack}}), ErrLost)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			copy := *w
			if e := m.FinishWork(&copy); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	must(t, m.FinishRelease(v))
	if v.state.work != 0 {
		t.Fatal("double decrement")
	}
}
