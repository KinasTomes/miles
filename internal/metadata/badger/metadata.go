package badger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

func (m *Store) GetBucket(ctx context.Context, name string) (s.Bucket, error) {
	v, err := read[s.Bucket](m, ctx, key("buckets", name))
	if err == nil && !v.Active {
		err = s.ErrNotFound
	}
	return v, err
}
func (m *Store) GetCredential(ctx context.Context, id string) (s.Credential, error) {
	return read[s.Credential](m, ctx, key("credentials", id))
}
func (m *Store) GetQuota(ctx context.Context, id string) (s.LogicalQuota, error) {
	return read[s.LogicalQuota](m, ctx, key("quotas", id))
}
func (m *Store) GetReservation(ctx context.Context, id string) (s.Reservation, error) {
	return read[s.Reservation](m, ctx, key("reservations", id))
}
func (m *Store) GetOutbox(ctx context.Context, id string) (s.OutboxEvent, error) {
	return read[s.OutboxEvent](m, ctx, key("events", id))
}
func (m *Store) GetCompletionReceipt(ctx context.Context, id string) (s.CompletedReceipt, error) {
	return read[s.CompletedReceipt](m, ctx, key("receipts", id))
}
func (m *Store) Authorize(ctx context.Context, c, b string) (s.Authorization, error) {
	var v s.Authorization
	err := m.view(ctx, func(t *db.Txn) error { var e error; v, e = m.authorize(t, c, b); return e })
	return v, err
}
func (m *Store) GetChunks(ctx context.Context, keys []s.ChunkKey, _ bool) (map[s.ChunkKey]s.Chunk, error) {
	out := make(map[s.ChunkKey]s.Chunk)
	err := m.view(ctx, func(t *db.Txn) error {
		for _, k := range keys {
			if err := k.Validate(); err != nil {
				return err
			}
			v, err := get[s.Chunk](t, chunkKey(k))
			if errors.Is(err, s.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			out[k] = v
		}
		return nil
	})
	return out, err
}
func location(t *db.Txn, l s.ChunkLocation) error {
	if err := l.Validate(); err != nil {
		return err
	}
	p, err := get[s.PackInfo](t, packKey(l.Pack))
	if err != nil {
		return err
	}
	if p.State == "Retiring" || p.State == "Deleting" || p.State == "Transitioning" {
		return s.ErrConflict
	}
	return l.Range.Validate(p.Size)
}
func (m *Store) PutChunkIfAbsent(ctx context.Context, c s.Chunk, w s.WriterFence) (s.Chunk, bool, error) {
	var out s.Chunk
	won := false
	err := m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, w); err != nil {
			return err
		}
		if err := c.Validate(); err != nil {
			return err
		}
		old, err := get[s.Chunk](t, chunkKey(c.Key))
		if err == nil {
			out = old
			return nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		if c.LocationRevision != 1 {
			return s.ErrInvalid
		}
		if err := location(t, c.Location); err != nil {
			return err
		}
		if err := put(t, chunkKey(c.Key), c); err != nil {
			return err
		}
		out = c
		won = true
		return nil
	})
	if err != nil {
		return s.Chunk{}, false, err
	}
	return out, won, nil
}
func (m *Store) CompareAndSwapChunkLocation(ctx context.Context, k s.ChunkKey, revision uint64, next s.ChunkLocation, w s.WriterFence) (s.Chunk, error) {
	var out s.Chunk
	err := m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, w); err != nil {
			return err
		}
		if err := k.Validate(); err != nil {
			return err
		}
		v, err := get[s.Chunk](t, chunkKey(k))
		if err != nil {
			return err
		}
		if revision == ^uint64(0) || v.LocationRevision != revision {
			return s.ErrConflict
		}
		if err := location(t, next); err != nil {
			return err
		}
		v.Location = next
		v.LocationRevision++
		out = v
		return put(t, chunkKey(k), v)
	})
	return out, err
}
func (m *Store) GetPacks(ctx context.Context, refs []s.PackRef) (map[s.PackRef]s.PackInfo, error) {
	out := make(map[s.PackRef]s.PackInfo)
	err := m.view(ctx, func(t *db.Txn) error {
		for _, r := range refs {
			if err := r.Validate(); err != nil {
				return err
			}
			v, err := get[s.PackInfo](t, packKey(r))
			if errors.Is(err, s.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			out[r] = v
		}
		return nil
	})
	return out, err
}
func pageKey(id string, n uint32) string { return fmt.Sprintf("%s/%08x", key("pages", id), n) }
func pageBytes(t *db.Txn, k string) ([]byte, error) {
	item, err := t.Get([]byte(k))
	if err != nil {
		return nil, backend(err)
	}
	b, err := item.ValueCopy(nil)
	return b, backend(err)
}
func (m *Store) WriteManifestPages(ctx context.Context, pages []s.ManifestPage, w s.WriterFence) error {
	for _, p := range pages {
		b, err := manifest.Encode(manifest.ObjectPage, p)
		if err != nil {
			return err
		}
		err = m.update(ctx, func(t *db.Txn) error {
			if err := m.fence(t, w); err != nil {
				return err
			}
			k := pageKey(p.ManifestID, p.Number)
			old, err := pageBytes(t, k)
			if err == nil {
				if !bytes.Equal(old, b) {
					return s.ErrConflict
				}
				return nil
			}
			if !errors.Is(err, s.ErrNotFound) {
				return err
			}
			return backend(t.Set([]byte(k), b))
		})
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
func walkPages(ctx context.Context, t *db.Txn, id string, fn func(s.ManifestPage) error) error {
	count := 0
	err := scan(t, key("pages", id)+"/", func(k string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count >= manifest.MaxPages {
			return s.ErrInvalid
		}
		var number uint32
		if _, err := fmt.Sscanf(k[len(key("pages", id))+1:], "%08x", &number); err != nil {
			return s.ErrInvalid
		}
		b, err := pageBytes(t, k)
		if err != nil {
			return err
		}
		p, err := manifest.Decode(b, manifest.ObjectPage, id, number)
		if err != nil {
			return err
		}
		count++
		return fn(p)
	})
	if err == nil && count == 0 {
		return s.ErrNotFound
	}
	return err
}
func (m *Store) GetManifest(ctx context.Context, id string) ([]s.ManifestPage, error) {
	var out []s.ManifestPage
	err := m.view(ctx, func(t *db.Txn) error {
		return walkPages(ctx, t, id, func(p s.ManifestPage) error { out = append(out, p); return nil })
	})
	return out, err
}

// GetManifestPage is a bounded-memory read path for callers that do not need
// the legacy whole-manifest MetadataStore method.
func (m *Store) GetManifestPage(ctx context.Context, id string, n uint32) (s.ManifestPage, error) {
	var p s.ManifestPage
	err := m.view(ctx, func(t *db.Txn) error {
		b, err := pageBytes(t, pageKey(id, n))
		if err != nil {
			return err
		}
		p, err = manifest.Decode(b, manifest.ObjectPage, id, n)
		return err
	})
	return p, err
}
func (m *Store) GetObject(ctx context.Context, k s.ObjectKey) (s.ObjectHead, error) {
	var v s.ObjectHead
	err := m.view(ctx, func(t *db.Txn) error {
		if err := k.Validate(); err != nil {
			return err
		}
		b, err := get[s.Bucket](t, key("buckets", k.Bucket))
		if err != nil {
			return err
		}
		if !b.Active || b.ID != k.BucketID {
			return s.ErrNotFound
		}
		v, err = get[s.ObjectHead](t, headKey(k))
		return err
	})
	return v, err
}
func validateManifest(ctx context.Context, t *db.Txn, h *s.ObjectHead) error {
	var offset int64
	var number uint32
	var chunks uint64
	var index []uint64
	err := walkPages(ctx, t, h.ManifestID, func(p s.ManifestPage) error {
		if p.Number != number || p.Domain != h.Domain {
			return s.ErrInvalid
		}
		number++
		index = append(index, uint64(offset))
		for _, r := range p.Chunks {
			chunks++
			c, err := get[s.Chunk](t, chunkKey(r.Key))
			if err != nil {
				return err
			}
			if c.RawBytes != r.RawBytes || r.LogicalOffset != offset || offset > s.MaxObjectBytes-r.RawBytes {
				return s.ErrInvalid
			}
			offset += r.RawBytes
		}
		return nil
	})
	if h.Size == 0 && errors.Is(err, s.ErrNotFound) {
		if h.PageCount != 0 || h.ChunkCount != 0 || len(h.PageIndex) != 0 {
			return s.ErrInvalid
		}
		return nil
	}
	if err != nil {
		return err
	}
	if offset != h.Size {
		return s.ErrInvalid
	}
	if (h.PageCount != 0 || h.ChunkCount != 0 || len(h.PageIndex) != 0) && (h.PageCount != number || h.ChunkCount != chunks || !reflect.DeepEqual(h.PageIndex, index)) {
		return s.ErrInvalid
	}
	h.PageCount, h.ChunkCount, h.PageIndex = number, chunks, index
	return nil
}
