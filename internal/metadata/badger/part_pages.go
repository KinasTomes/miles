package badger

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

var _ s.PartPageStore = (*Store)(nil)

func partPageKey(owner string, number uint32) string {
	return fmt.Sprintf("%s/%08x", key("part-pages", owner), number)
}

// Staging only: UploadPart state/revision CAS and publication remain caller-owned.
func (m *Store) WritePartPages(ctx context.Context, pages []s.ManifestPage, writer s.WriterFence) error {
	for _, page := range pages {
		body, err := manifest.Encode(manifest.PartPage, page)
		if err != nil {
			return err
		}
		if err := m.update(ctx, func(t *db.Txn) error {
			if err := m.fence(t, writer); err != nil {
				return err
			}
			k := partPageKey(page.ManifestID, page.Number)
			old, err := pageBytes(t, k)
			if err == nil {
				if !bytes.Equal(old, body) {
					return s.ErrConflict
				}
				return nil
			}
			if !errors.Is(err, s.ErrNotFound) {
				return err
			}
			return backend(t.Set([]byte(k), body))
		}); err != nil {
			return err
		}
	}
	return ctx.Err()
}
func (m *Store) GetPartPage(ctx context.Context, owner string, number uint32) (s.ManifestPage, error) {
	var page s.ManifestPage
	err := m.view(ctx, func(t *db.Txn) error {
		body, err := pageBytes(t, partPageKey(owner, number))
		if err != nil {
			return err
		}
		page, err = manifest.Decode(body, manifest.PartPage, owner, number)
		return err
	})
	return page, err
}
