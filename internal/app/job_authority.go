package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/storage"
)

// sharedJobAuthority uses the same R1 table and UTC proof as object/control
// mutations. No lease is released while pins or unknown I/O remain.
type sharedJobAuthority struct{ metadata *sharedMetadata }

func (a sharedJobAuthority) Acquire(ctx context.Context, id, kind string) (storage.WriterFence, error) {
	m := a.metadata
	g, err := m.GetR1Gate(ctx)
	if err != nil {
		return storage.WriterFence{}, err
	}
	f := storage.WriterFence{LeaseID: id, ServiceIncarnation: g.Incarnation, Epoch: 1}
	old, err := m.GetR1Lease(ctx, id)
	if err == nil {
		if old.Fence.ServiceIncarnation != g.Incarnation || old.Fence.Epoch >= storage.MaxPolicyRevision {
			return f, storage.ErrFenced
		}
		if old.State != "Released" {
			job, readErr := m.GetJob(ctx, id)
			if readErr != nil && !errors.Is(readErr, storage.ErrNotFound) {
				return f, readErr
			}
			if readErr == nil && job.PendingStep != "" && (job.Type == "tiering" || job.Type == "pack_restore" || job.Type == "pack_promote" || job.Type == "compaction" || job.Type == "gc") {
				if err = m.LifecycleQuiescent(ctx, id, old.Fence); err != nil {
					return f, errors.Join(jobs.ErrReconciliation, err)
				}
			}

			if err = m.ReclaimR1(ctx, old.Fence, 5*time.Second); err != nil {
				return f, err
			}
			if err = m.ReconcileR1(ctx, old.Fence); err != nil {
				return f, err
			}
			if err = m.ReleaseR1(ctx, old.Fence); err != nil {
				return f, err
			}
		}
		f.Epoch = old.Fence.Epoch + 1
	} else if !errors.Is(err, storage.ErrNotFound) {
		return f, err
	}
	b, err := m.utc.Bounds(ctx)
	if err != nil {
		return f, err
	}
	sum := sha256.Sum256([]byte("job-producer/" + id))
	op := hex.EncodeToString(sum[:16])
	_, err = m.AcquireR1(ctx, storage.R1Acquire{Fence: f, Owner: m.owner, Kind: kind, OpID: op, Deadline: minTime(b.Upper.Add(9*time.Minute), b.Horizon)})
	return f, err
}
func (a sharedJobAuthority) Renew(ctx context.Context, f storage.WriterFence) error {
	_, err := a.metadata.RenewR1(ctx, f)
	return err
}
func (a sharedJobAuthority) Close(ctx context.Context, f storage.WriterFence) error {
	err := a.metadata.CloseR1(ctx, f)
	if err == nil {
		err = a.metadata.ReleaseR1(ctx, f)
	}
	a.metadata.recordCleanup(f, err)
	return err
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
