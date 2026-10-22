package badger

import (
	"context"
	"errors"
	"strings"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

var _ s.MetadataStore = (*Store)(nil)
var _ s.ManifestPageReader = (*Store)(nil)
var stopScan = errors.New("page full")

func activeUser(t *db.Txn, id string) error {
	u, err := optional[s.User](t, key("users", id))
	if err != nil {
		return err
	}
	if !u.Active {
		return s.ErrAccessDenied
	}
	return nil
}
func (m *Store) ListAccessibleBuckets(ctx context.Context, r s.BucketListRequest) (s.BucketListPage, error) {
	out := s.BucketListPage{Buckets: []s.Bucket{}}
	err := m.view(ctx, func(t *db.Txn) error {
		if r.PrincipalID == "" || r.Limit < 1 || r.Limit > 1000 {
			return s.ErrInvalid
		}
		if err := activeUser(t, r.PrincipalID); err != nil {
			return err
		}
		err := scan(t, "buckets/", func(k string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			b, err := get[s.Bucket](t, k)
			if err != nil {
				return err
			}
			if !b.Active || b.Name <= r.After {
				return nil
			}
			g, err := optional[s.Grant](t, key("grants", b.ID, r.PrincipalID))
			if err != nil {
				return err
			}
			if b.OwnerID != r.PrincipalID && !g.Read && !g.Write {
				return nil
			}
			if len(out.Buckets) == r.Limit {
				out.NextAfter = out.Buckets[len(out.Buckets)-1].Name
				return stopScan
			}
			out.Buckets = append(out.Buckets, b)
			return nil
		})
		if errors.Is(err, stopScan) {
			return nil
		}
		return err
	})
	return out, err
}
func (m *Store) ListObjects(ctx context.Context, r s.ListObjectsRequest) (s.ObjectListPage, error) {
	out := s.ObjectListPage{Objects: []s.ObjectHead{}, CommonPrefixes: []string{}}
	err := m.view(ctx, func(t *db.Txn) error {
		if r.PrincipalID == "" || r.Bucket == "" || r.BucketID == "" || r.MaxKeys < 0 || r.MaxKeys > 1000 {
			return s.ErrInvalid
		}
		if err := activeUser(t, r.PrincipalID); err != nil {
			return err
		}
		b, err := get[s.Bucket](t, key("buckets", r.Bucket))
		if err != nil {
			return err
		}
		if !b.Active || b.ID != r.BucketID {
			return s.ErrNotFound
		}
		g, err := optional[s.Grant](t, key("grants", b.ID, r.PrincipalID))
		if err != nil {
			return err
		}
		if b.OwnerID != r.PrincipalID && !g.Read {
			return s.ErrAccessDenied
		}
		cursor := s.ObjectListCursor{PrincipalID: r.PrincipalID, Bucket: r.Bucket, BucketID: r.BucketID, Prefix: r.Prefix, Delimiter: r.Delimiter, LastScannedKey: r.StartAfter}
		if c := r.Cursor; c != nil {
			if c.PrincipalID != r.PrincipalID || c.Bucket != r.Bucket || c.BucketID != r.BucketID || c.Prefix != r.Prefix || c.Delimiter != r.Delimiter {
				return s.ErrInvalid
			}
			cursor = *c
		}
		if r.MaxKeys == 0 {
			return nil
		}
		err = scan(t, key("heads", r.Bucket, r.BucketID)+"/", func(k string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			h, err := get[s.ObjectHead](t, k)
			if err != nil {
				return err
			}
			objectKey := h.Object.Key
			if objectKey <= cursor.LastScannedKey || !strings.HasPrefix(objectKey, r.Prefix) {
				return nil
			}
			if h.Deleted {
				cursor.LastScannedKey = objectKey
				return nil
			}
			common := ""
			if r.Delimiter != "" {
				if i := strings.Index(strings.TrimPrefix(objectKey, r.Prefix), r.Delimiter); i >= 0 {
					common = objectKey[:len(r.Prefix)+i+len(r.Delimiter)]
				}
			}
			if common != "" && (common == cursor.LastCommonPrefix || (r.Cursor == nil && common <= r.StartAfter)) {
				cursor.LastScannedKey = objectKey
				return nil
			}
			if len(out.Objects)+len(out.CommonPrefixes) == r.MaxKeys {
				out.Next = &cursor
				return stopScan
			}
			cursor.LastScannedKey = objectKey
			if common != "" {
				out.CommonPrefixes = append(out.CommonPrefixes, common)
				cursor.LastCommonPrefix = common
			} else {
				out.Objects = append(out.Objects, h)
				cursor.LastCommonPrefix = ""
			}
			return nil
		})
		if errors.Is(err, stopScan) {
			return nil
		}
		return err
	})
	return out, err
}
func (m *Store) UpdateAccessStats(ctx context.Context, updates []s.PackAccess) error {
	return m.update(ctx, func(t *db.Txn) error {
		for _, u := range updates {
			if err := u.Pack.Validate(); err != nil {
				return err
			}
			if u.LastAccess.IsZero() {
				return s.ErrInvalid
			}
			p, err := get[s.PackInfo](t, packKey(u.Pack))
			if err != nil {
				return err
			}
			if u.LastAccess.After(p.LastAccess) {
				p.LastAccess = u.LastAccess
				if err := put(t, packKey(p.Ref), p); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
func packLess(a, b s.PackRef) bool {
	return a.ID < b.ID || (a.ID == b.ID && a.Generation < b.Generation)
}
func (m *Store) ListPacksByState(ctx context.Context, r s.PackListRequest) (s.PackListPage, error) {
	out := s.PackListPage{Packs: []s.PackInfo{}}
	err := m.view(ctx, func(t *db.Txn) error {
		switch r.State {
		case "Warm", "Cold", "Restoring", "Restored", "Transitioning", "Retiring", "Deleting":
		default:
			return s.ErrInvalid
		}
		if r.Limit < 1 || r.Limit > 1000 {
			return s.ErrInvalid
		}
		if r.After != nil {
			if err := r.After.Validate(); err != nil {
				return err
			}
		}
		err := scan(t, "packs/", func(k string) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			p, err := get[s.PackInfo](t, k)
			if err != nil {
				return err
			}
			if p.State != r.State || (r.After != nil && !packLess(*r.After, p.Ref)) {
				return nil
			}
			if len(out.Packs) == r.Limit {
				v := out.Packs[len(out.Packs)-1].Ref
				out.NextAfter = &v
				return stopScan
			}
			out.Packs = append(out.Packs, p)
			return nil
		})
		if errors.Is(err, stopScan) {
			return nil
		}
		return err
	})
	return out, err
}
