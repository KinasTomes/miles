package contracttest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

type PackHarness struct {
	CompleteRestore func(s.PackRef, time.Time) error
	Store           s.PackStore
	SidecarInfo     func(s.PackRef) (s.PackInfo, bool)
}
type PackFactory func(*testing.T, func() time.Time) PackHarness

func RunPacks(t *testing.T, newStore PackFactory) {
	ctx := context.Background()
	t.Run("immutable pack range and generation", func(t *testing.T) {
		now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
		h := newStore(t, func() time.Time { return now })
		info := NewFixture().Packs[0]
		info.Size = 8
		body := []byte("abcdefgh")
		if err := h.Store.PutPack(ctx, info, bytes.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		if err := h.Store.PutPack(ctx, info, bytes.NewReader(body)); err != nil {
			t.Fatal("retry", err)
		}
		requireError(t, h.Store.PutPack(ctx, info, strings.NewReader("changed!")), s.ErrConflict)
		reader, err := h.Store.RangeGet(ctx, info.Ref, s.ByteRange{Offset: 2, Length: 3})
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
		if string(got) != "cde" {
			t.Fatalf("range=%q", got)
		}
		unknown := info.Ref
		unknown.Generation = "other"
		_, err = h.Store.RangeGet(ctx, unknown, s.ByteRange{Length: 1})
		requireError(t, err, s.ErrNotFound)
		for _, r := range []s.ByteRange{{Offset: -1, Length: 1}, {Offset: 0, Length: 0}, {Offset: 7, Length: 2}, {Offset: 1 << 62, Length: 1 << 62}} {
			_, err := h.Store.RangeGet(ctx, info.Ref, r)
			requireError(t, err, s.ErrInvalid)
		}
		other := info
		other.Ref.Generation = "other"
		if err := h.Store.PutPack(ctx, other, strings.NewReader("ijklmnop")); err != nil {
			t.Fatal(err)
		}
		if err := h.Store.DeletePack(ctx, info.Ref); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Store.HeadPack(ctx, other.Ref); err != nil {
			t.Fatal("deleting old generation removed new generation", err)
		}
	})
	t.Run("body errors and cancellation leave no pack", func(t *testing.T) {
		now := NewFixture().Now
		h := newStore(t, func() time.Time { return now })
		info := NewFixture().Packs[0]
		info.Size = 4
		for _, body := range []string{"abc", "abcde"} {
			requireError(t, h.Store.PutPack(ctx, info, strings.NewReader(body)), s.ErrInvalid)
			_, err := h.Store.HeadPack(ctx, info.Ref)
			requireError(t, err, s.ErrNotFound)
		}
		injected := errors.New("broken client stream")
		err := h.Store.PutPack(ctx, info, failingReader{injected})
		requireError(t, err, injected)
		_, err = h.Store.HeadPack(ctx, info.Ref)
		requireError(t, err, s.ErrNotFound)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		requireError(t, h.Store.PutPack(cancelled, info, strings.NewReader("data")), context.Canceled)
		_, err = h.Store.HeadPack(ctx, info.Ref)
		requireError(t, err, s.ErrNotFound)
	})
	t.Run("instant retrieval restore expiry and Standard sidecar", func(t *testing.T) {
		now := NewFixture().Now
		h := newStore(t, func() time.Time { return now })
		info := NewFixture().Packs[0]
		info.Size = 4
		if err := h.Store.PutPack(ctx, info, strings.NewReader("data")); err != nil {
			t.Fatal(err)
		}
		if err := h.Store.PutSidecar(ctx, info.Ref, 5, strings.NewReader("index")); err != nil {
			t.Fatal(err)
		}
		if err := h.Store.ChangeStorageClass(ctx, info.Ref, s.GlacierIR); err != nil {
			t.Fatal(err)
		}
		r, err := h.Store.RangeGet(ctx, info.Ref, s.ByteRange{Length: 4})
		if err != nil {
			t.Fatal("GLACIER_IR must read immediately", err)
		}
		r.Close()
		if err := h.Store.ChangeStorageClass(ctx, info.Ref, s.Glacier); err != nil {
			t.Fatal(err)
		}
		_, err = h.Store.RangeGet(ctx, info.Ref, s.ByteRange{Length: 4})
		requireError(t, err, s.ErrCold)
		if err := h.Store.Restore(ctx, info.Ref, s.RestoreRequest{Days: 1, Tier: "Standard"}); err != nil {
			t.Fatal(err)
		}
		_, err = h.Store.RangeGet(ctx, info.Ref, s.ByteRange{Length: 4})
		requireError(t, err, s.ErrCold)
		if err := h.CompleteRestore(info.Ref, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		r, err = h.Store.RangeGet(ctx, info.Ref, s.ByteRange{Length: 4})
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(r)
		r.Close()
		if err != nil || string(got) != "data" {
			t.Fatal("restore changed bytes", err)
		}
		now = now.Add(time.Hour)
		_, err = h.Store.RangeGet(ctx, info.Ref, s.ByteRange{Length: 4})
		requireError(t, err, s.ErrCold)
		sidecar, ok := h.SidecarInfo(info.Ref)
		if !ok || sidecar.Class != s.Standard {
			t.Fatal("sidecar followed pack into archive")
		}
		head, err := h.Store.HeadPack(ctx, info.Ref)
		if err != nil || head.Ref != info.Ref {
			t.Fatal("class change altered generation", err)
		}
	})
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

type CacheFactory func(*testing.T) s.Cache

func RunCache(t *testing.T, newCache CacheFactory) {
	ctx := context.Background()
	t.Run("cache identity ownership and eviction", func(t *testing.T) {
		cache := newCache(t)
		key := s.CacheKey{Pack: s.PackRef{ID: "pack", Generation: "old"}, Range: s.ByteRange{Offset: 1, Length: 4}}
		if _, hit, err := cache.Get(ctx, key); err != nil || hit {
			t.Fatal("empty cache hit", err)
		}
		body := []byte("data")
		if err := cache.Put(ctx, key, body); err != nil {
			t.Fatal(err)
		}
		body[0] = 'X'
		got, hit, err := cache.Get(ctx, key)
		if err != nil || !hit || string(got) != "data" {
			t.Fatal("cache aliases caller input", err)
		}
		got[0] = 'Y'
		again, _, _ := cache.Get(ctx, key)
		if string(again) != "data" {
			t.Fatal("cache aliases returned data")
		}
		next := key
		next.Pack.Generation = "new"
		offset := key
		offset.Range.Offset++
		for _, other := range []s.CacheKey{next, offset} {
			if _, hit, err := cache.Get(ctx, other); err != nil || hit {
				t.Fatal("cache key lost generation/range", err)
			}
		}
		if err := cache.Put(ctx, next, []byte("more")); err != nil {
			t.Fatal(err)
		}
		if err := cache.Evict(ctx, key.Pack); err != nil {
			t.Fatal(err)
		}
		if _, hit, _ := cache.Get(ctx, key); hit {
			t.Fatal("old generation not evicted")
		}
		if _, hit, _ := cache.Get(ctx, next); !hit {
			t.Fatal("eviction removed another generation")
		}
	})
	t.Run("invalid or cancelled cache writes rejected", func(t *testing.T) {
		cache := newCache(t)
		key := s.CacheKey{Pack: s.PackRef{ID: "pack", Generation: "old"}, Range: s.ByteRange{Length: 4}}
		requireError(t, cache.Put(ctx, key, []byte("bad")), s.ErrInvalid)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		requireError(t, cache.Put(cancelled, key, []byte("data")), context.Canceled)
		if _, hit, _ := cache.Get(ctx, key); hit {
			t.Fatal("cancelled cache write published")
		}
	})
}
