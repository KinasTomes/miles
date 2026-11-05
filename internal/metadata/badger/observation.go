package badger

import (
	"context"
	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
	"strings"
)

var _ s.ObservationStore = (*Store)(nil)

func (m *Store) ObservePage(ctx context.Context, r s.ObservationRequest) (s.ObservationPage, error) {
	var out s.ObservationPage
	if err := r.Validate(); err != nil {
		return out, err
	}
	prefix := r.Kind + "/"
	if r.Kind == "uploads" {
		prefix = "upload-record/"
	}
	if r.Cursor != "" && !strings.HasPrefix(r.Cursor, prefix) {
		return out, s.ErrInvalid
	}
	err := m.view(ctx, func(t *db.Txn) error {
		opts := db.DefaultIteratorOptions
		opts.PrefetchValues = false
		it := t.NewIterator(opts)
		defer it.Close()
		start := prefix
		if r.Cursor != "" {
			start = r.Cursor
		}
		last := ""
		n := 0
		for it.Seek([]byte(start)); it.ValidForPrefix([]byte(prefix)); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			k := string(it.Item().KeyCopy(nil))
			if k == r.Cursor {
				continue
			}
			if n == r.Limit {
				out.Next = last
				break
			}
			switch r.Kind {
			case "buckets":
				v, e := get[s.Bucket](t, k)
				if e != nil {
					return e
				}
				out.Buckets = append(out.Buckets, v)
			case "heads":
				v, e := get[s.ObjectHead](t, k)
				if e != nil {
					return e
				}
				out.Heads = append(out.Heads, v)
			case "uploads":
				v, e := get[s.MultipartUpload](t, k)
				if e != nil {
					return e
				}
				out.Uploads = append(out.Uploads, v)
			case "chunks":
				v, e := get[s.Chunk](t, k)
				if e != nil {
					return e
				}
				out.Chunks = append(out.Chunks, v)
			case "packs":
				v, e := get[s.PackInfo](t, k)
				if e != nil {
					return e
				}
				out.Packs = append(out.Packs, v)
			}
			n++
			last = k
		}
		return nil
	})
	return out, err
}

// ObserveUploadParts reads authoritative part heads for maintenance without S3 authorization filters.
func (m *Store) ObserveUploadParts(ctx context.Context, id string, after uint32, limit int) (s.PartListPage, error) {
	return (multipartBridge{m}).Parts(ctx, id, after, limit)
}
