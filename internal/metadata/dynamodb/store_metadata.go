// Business validation follows the shared Badger contract; persistence and
// conflict detection here use native DynamoDB items and conditional transactions.
package dynamodb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
)

func (m *Store) GetBucket(ctx context.Context, name string) (s.Bucket, error) {
	v, err := read[s.Bucket](m, ctx, key("buckets", name))
	if err == nil && !v.Active {
		err = s.ErrNotFound
	}
	return v, err
}

// CreateBucket is the API v2 2.2.3/3.5 transaction in one conditional DynamoDB transaction: the
// credential and user that admitted the request are rechecked, the name must
// still be free, and the new incarnation gets its own bucket_id, the owner's
// current default_domain and CreatedAt. A taken name returns the existing
// bucket without writing.
func (m *Store) CreateBucket(ctx context.Context, r s.BucketCreate) (s.Bucket, bool, error) {
	var out s.Bucket
	created := false
	err := m.update(ctx, func(t *transaction) error {
		created = false

		if err := r.Validate(); err != nil {
			return err
		}
		c, err := optional[s.Credential](t, key("credentials", r.CredentialID))
		if err != nil {
			return err
		}
		if !c.Active || c.UserID != r.UserID {
			return s.ErrAccessDenied
		}
		u, err := optional[s.User](t, key("users", r.UserID))
		if err != nil {
			return err
		}
		if !u.Active {
			return s.ErrAccessDenied
		}
		old, err := get[s.Bucket](t, key("buckets", r.Name))
		if err == nil {
			out = old
			return nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		id, err := s.NewBucketID()
		if err != nil {
			return err
		}
		domain := u.DefaultDomain
		if domain == "" {
			domain = "default"
		}
		if err := s.ValidateDomain(domain); err != nil {
			return err
		}
		out = s.Bucket{Name: r.Name, ID: id, OwnerID: u.ID, Domain: domain, Revision: 1, Active: true, CreatedAt: m.now()}
		created = true
		return put(t, key("buckets", r.Name), out)
	})
	if err != nil {
		return s.Bucket{}, false, err
	}
	return out, created, nil
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
	err := m.view(ctx, func(t *transaction) error { var e error; v, e = m.authorize(t, c, b); return e })
	return v, err
}
func (m *Store) GetChunks(ctx context.Context, keys []s.ChunkKey, consistent bool) (map[s.ChunkKey]s.Chunk, error) {
	out := make(map[s.ChunkKey]s.Chunk)
	err := m.view(ctx, func(t *transaction) error {
		logical := make([]string, len(keys))
		for i, k := range keys {
			logical[i] = chunkKey(k)
		}
		if err := t.batch(logical, consistent); err != nil {
			return err
		}

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

// validateCanonicalDestination admits only a new Warm destination. Existing
// winners are returned before this check so the engine can classify/revive them.
func validateCanonicalDestination(t *transaction, l s.ChunkLocation) error {
	if err := l.Validate(); err != nil {
		return err
	}
	p, err := get[s.PackInfo](t, packKey(l.Pack))
	if err != nil {
		return err
	}
	if p.State != "Warm" {
		return s.ErrConflict
	}
	return l.Range.Validate(p.Size)
}
func (m *Store) PutChunkIfAbsent(ctx context.Context, c s.Chunk, w s.WriterFence) (s.Chunk, bool, error) {
	var out s.Chunk
	won := false
	err := m.update(ctx, func(t *transaction) error {
		won = false

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
		if err := validateCanonicalDestination(t, c.Location); err != nil {
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
	err := m.update(ctx, func(t *transaction) error {
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
		if err := validateCanonicalDestination(t, next); err != nil {
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
	err := m.view(ctx, func(t *transaction) error {
		logical := make([]string, len(refs))
		for i, k := range refs {
			logical[i] = packKey(k)
		}
		if err := t.batch(logical, true); err != nil {
			return err
		}

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
func pageBytes(t *transaction, k string) ([]byte, error) {
	row, err := t.load(k)
	if err != nil {
		return nil, err
	}
	if row.after == nil {
		return nil, s.ErrNotFound
	}
	return CloneBinary(row.after["body"])
}
func (m *Store) WriteManifestPages(ctx context.Context, pages []s.ManifestPage, w s.WriterFence) error {
	for _, p := range pages {
		b, err := manifest.Encode(manifest.ObjectPage, p)
		if err != nil {
			return err
		}
		err = m.update(ctx, func(t *transaction) error {
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
			sealed, err := exists(t, key("manifest-seals", p.ManifestID))
			if err != nil {
				return err
			}
			if sealed {
				return s.ErrConflict
			}
			ownerKey := key("manifest-owner", p.ManifestID)
			owner, err := get[manifestOwner](t, ownerKey)
			if errors.Is(err, s.ErrNotFound) {
				owner = manifestOwner{State: "Building", Domain: p.Domain, Writer: w, Revision: 1}
			} else if err != nil {
				return err
			}
			if owner.State != "Building" || owner.Domain != p.Domain || owner.Writer != w {
				return s.ErrConflict
			}
			owner.Revision++
			if err := put(t, ownerKey, owner); err != nil {
				return err
			}
			return t.setPage(k, b)
		})
		if err != nil {
			return err
		}
	}
	return ctx.Err()
}
func walkPages(ctx context.Context, t *transaction, id string, fn func(s.ManifestPage) error) error {
	count := 0
	err := scan(t, key("pages", id)+"/", func(k string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if count >= manifest.MaxPages {
			return s.ErrInvalid
		}
		suffix := k[len(key("pages", id))+1:]
		parsed, err := strconv.ParseUint(suffix, 16, 32)
		if err != nil || len(suffix) != 8 {
			return s.ErrInvalid
		}
		number := uint32(parsed)
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
	err := m.view(ctx, func(t *transaction) error {
		err := walkPages(ctx, t, id, func(p s.ManifestPage) error { out = append(out, p); return nil })
		if errors.Is(err, s.ErrNotFound) {
			seal, sealErr := get[manifestSeal](t, key("manifest-seals", id))
			if sealErr == nil {
				if seal.PageCount != 0 {
					return s.ErrUnavailable
				}
				out = []s.ManifestPage{}
				return nil
			}
			if !errors.Is(sealErr, s.ErrNotFound) {
				return sealErr
			}
		}
		return err
	})
	return out, err
}

// GetManifestPage is a bounded-memory read path for callers that do not need
// the legacy whole-manifest MetadataStore method.
func (m *Store) GetManifestPage(ctx context.Context, id string, n uint32) (s.ManifestPage, error) {
	var p s.ManifestPage
	err := m.view(ctx, func(t *transaction) error {
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
	err := m.view(ctx, func(t *transaction) error {
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
