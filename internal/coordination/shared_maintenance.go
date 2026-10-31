package coordination

import (
	"context"
	"errors"

	"github.com/KinasTomes/miles/internal/storage"
)

type SharedMaintenanceStore interface {
	BeginR1Maintenance(context.Context, storage.R1Maintenance) (storage.R1Maintenance, error)
	UnknownR1Maintenance(context.Context, string) error
	FinishR1Maintenance(context.Context, string) error
}

// RunSharedMaintenance persists closed state before ONE physical dispatch.
// It is opt-in and still requires the backend's GC/R2/terminal proof guard.
// The ordinary ProtectPackStore remains disabled for destructive operations.
func RunSharedMaintenance(ctx context.Context, store SharedMaintenanceStore, packs storage.PackStore, r storage.R1Maintenance) error {
	if store == nil || packs == nil {
		return storage.ErrInvalid
	}
	a, err := store.BeginR1Maintenance(ctx, r)
	if err != nil {
		return err
	}
	if a.Action == "delete" {
		err = packs.DeletePack(ctx, a.Pack)
	} else {
		err = packs.ChangeStorageClass(ctx, a.Pack, a.Target)
	}
	if err != nil {
		cleanup, cancel := cleanupContext()
		defer cancel()
		return errors.Join(storage.ErrUnavailable, err, store.UnknownR1Maintenance(cleanup, a.ID))
	}
	return store.FinishR1Maintenance(ctx, a.ID)
}
