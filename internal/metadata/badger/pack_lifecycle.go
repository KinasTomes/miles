package badger

import (
	"context"
	"errors"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

var _ s.PackLifecycleStore = (*Store)(nil)

func (m *Store) RegisterUploadingPack(ctx context.Context, info s.PackInfo, writer s.WriterFence) error {
	return m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, writer); err != nil {
			return err
		}
		if info.Ref.Validate() != nil || info.Version != s.ModelVersion || info.State != "Uploading" || info.StateRevision != 1 || info.Class != s.Standard || info.Writer != writer || info.Size <= 0 || info.Size > 64<<20 || !info.ProtectedUntil.After(m.now()) {
			return s.ErrInvalid
		}
		old, err := get[s.PackInfo](t, packKey(info.Ref))
		if err == nil {
			if old == info {
				return nil
			}
			return s.ErrConflict
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		return put(t, packKey(info.Ref), info)
	})
}

func (m *Store) MarkPackWarm(ctx context.Context, ref s.PackRef, expected uint64, writer s.WriterFence) (s.PackInfo, error) {
	var out s.PackInfo
	err := m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, writer); err != nil {
			return err
		}
		if err := ref.Validate(); err != nil {
			return err
		}
		v, err := get[s.PackInfo](t, packKey(ref))
		if err != nil {
			return err
		}
		if v.Writer != writer {
			return s.ErrFenced
		}
		if v.State == "Warm" && expected != ^uint64(0) && v.StateRevision == expected+1 {
			out = v
			return nil
		}
		if v.State != "Uploading" || v.StateRevision != expected || expected == ^uint64(0) || !v.ProtectedUntil.After(m.now()) {
			return s.ErrConflict
		}
		v.State = "Warm"
		v.StateRevision++
		out = v
		return put(t, packKey(ref), v)
	})
	return out, err
}
