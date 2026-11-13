package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/storage"
)

type sharedMetadata struct {
	*dynamodb.Store
	utc        storage.R1TimeSource
	owner      string
	mu         sync.Mutex
	incomplete map[storage.WriterFence]struct{}
}

func (m *sharedMetadata) admit(ctx context.Context) (storage.MutationContext, error) {
	g, err := m.GetR1Gate(ctx)
	if err != nil {
		return storage.MutationContext{}, err
	}
	id, err := storage.NewBucketID()
	if err != nil {
		return storage.MutationContext{}, err
	}
	op, err := storage.NewBucketID()
	if err != nil {
		return storage.MutationContext{}, err
	}
	b, err := m.utc.Bounds(ctx)
	if err != nil {
		return storage.MutationContext{}, err
	}
	budget := 30 * time.Second
	if d, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(d))
	}
	if budget <= 0 {
		return storage.MutationContext{}, context.DeadlineExceeded
	}
	l, err := m.AcquireR1(ctx, storage.R1Acquire{Fence: storage.WriterFence{LeaseID: id, Epoch: 1, ServiceIncarnation: g.Incarnation}, Kind: "control", Owner: m.owner, OpID: op, Deadline: b.Upper.Add(budget)})
	if err != nil {
		return storage.MutationContext{}, err
	}
	return storage.MutationContext{Writer: l.Fence, HistoryID: l.HistoryID, Epoch: l.RecoveryEpoch, LogicalOpID: op, MutationID: op}, nil
}
func (m *sharedMetadata) finish(ctx context.Context, c storage.MutationContext) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	err := m.CloseR1(ctx, c.Writer)
	if err == nil {
		err = m.ReleaseR1(ctx, c.Writer)
	}
	m.recordCleanup(c.Writer, err)
}

func (m *sharedMetadata) recordCleanup(f storage.WriterFence, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		delete(m.incomplete, f)
		return
	}
	if m.incomplete == nil {
		m.incomplete = map[storage.WriterFence]struct{}{}
	}
	m.incomplete[f] = struct{}{}
}

// Called after request and worker users have joined. A historical error is
// cleared only by native release proof; unknown work keeps shutdown incomplete.
func (m *sharedMetadata) closeError(ctx context.Context) error {
	m.mu.Lock()
	pending := make([]storage.WriterFence, 0, len(m.incomplete))
	for f := range m.incomplete {
		pending = append(pending, f)
	}
	m.mu.Unlock()
	for _, f := range pending {
		lease, err := m.GetR1Lease(ctx, f.LeaseID)
		if err == nil {
			switch {
			case lease.Fence.LeaseID != f.LeaseID || lease.Fence.ServiceIncarnation != f.ServiceIncarnation || lease.Fence.Epoch < f.Epoch:
				err = storage.ErrFenced
			case lease.Fence.Epoch > f.Epoch:
				// AcquireR1 can advance a generation only after its predecessor Released.
			case lease.State != "Released":
				if lease.Owner != m.owner {
					err = storage.ErrFenced
					break
				}
				err = m.CloseR1(ctx, f)
				if err == nil {
					err = m.ReconcileR1(ctx, f)
				}
				if err == nil {
					err = m.ReleaseR1(ctx, f)
				}
			}
		}
		if err != nil {
			return errors.Join(errors.New("shared control admission cleanup incomplete"), err)
		}
		m.recordCleanup(f, nil)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.incomplete) != 0 {
		return errors.New("shared control admission cleanup incomplete")
	}
	return nil
}

func (m *sharedMetadata) CreateBucket(ctx context.Context, r storage.BucketCreate) (storage.Bucket, bool, error) {
	c, err := m.admit(ctx)
	if err != nil {
		return storage.Bucket{}, false, err
	}
	defer m.finish(ctx, c)
	return m.Store.CreateBucket(storage.WithMutation(ctx, c), r)
}
func (m *sharedMetadata) DeleteBucket(ctx context.Context, r storage.BucketDelete) error {
	c, err := m.admit(ctx)
	if err != nil {
		return err
	}
	defer m.finish(ctx, c)
	return m.Store.DeleteBucket(storage.WithMutation(ctx, c), r)
}
