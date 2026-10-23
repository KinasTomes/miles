package fake

import (
	"context"

	s "github.com/KinasTomes/miles/internal/storage"
)

var _ s.PackLifecycleStore = (*Metadata)(nil)

func (m *Metadata) RegisterUploadingPack(ctx context.Context, info s.PackInfo, writer s.WriterFence) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.fence(writer); err != nil {
		return err
	}
	if info.Ref.Validate() != nil || info.Version != s.ModelVersion || info.State != "Uploading" || info.StateRevision != 1 || info.Class != s.Standard || info.Size <= 0 || info.Size > 64<<20 || !info.ProtectedUntil.After(m.now()) {
		return s.ErrInvalid
	}
	if old, ok := m.packs[info.Ref]; ok {
		if old == info {
			return nil
		}
		return s.ErrConflict
	}
	m.packs[info.Ref] = info
	return nil
}

func (m *Metadata) MarkPackWarm(ctx context.Context, ref s.PackRef, expected uint64, writer s.WriterFence) (s.PackInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.PackInfo{}, err
	}
	if err := m.fence(writer); err != nil {
		return s.PackInfo{}, err
	}
	if err := ref.Validate(); err != nil {
		return s.PackInfo{}, err
	}
	old, ok := m.packs[ref]
	if !ok {
		return s.PackInfo{}, s.ErrNotFound
	}
	if old.State == "Warm" && expected != ^uint64(0) && old.StateRevision == expected+1 {
		return old, nil
	}
	if old.State != "Uploading" || old.StateRevision != expected || expected == ^uint64(0) || !old.ProtectedUntil.After(m.now()) {
		return s.PackInfo{}, s.ErrConflict
	}
	old.State = "Warm"
	old.StateRevision++
	m.packs[ref] = old
	return old, nil
}
