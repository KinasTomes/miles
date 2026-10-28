package coordination

import (
	"context"

	"github.com/KinasTomes/miles/internal/storage"
)

// ProtectPackStore preserves ordinary physical I/O but unconditionally blocks
// destructive lifecycle in the MVP skeleton. A pin check alone cannot prove
// GC, R2 retention, archive transition or unknown S3 action safety.
func ProtectPackStore(p storage.PackStore) storage.PackStore {
	if p == nil {
		return nil
	}
	return protectedPacks{PackStore: p}
}

type protectedPacks struct{ storage.PackStore }

// Preserve the bounded sidecar verification extension used by StorePublisher.
func (p protectedPacks) ReadSidecar(ctx context.Context, ref storage.PackRef) ([]byte, error) {
	if reader, ok := p.PackStore.(interface {
		ReadSidecar(context.Context, storage.PackRef) ([]byte, error)
	}); ok {
		return reader.ReadSidecar(ctx, ref)
	}
	return nil, storage.ErrUnavailable
}

func (protectedPacks) DeletePack(context.Context, storage.PackRef) error {
	return ErrMaintenanceDisabled
}
func (protectedPacks) ChangeStorageClass(context.Context, storage.PackRef, storage.StorageClass) error {
	return ErrMaintenanceDisabled
}
