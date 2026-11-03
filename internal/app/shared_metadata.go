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
	incomplete bool
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
	if err != nil {
		m.mu.Lock()
		m.incomplete = true
		m.mu.Unlock()
	}
}
func (m *sharedMetadata) closeError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.incomplete {
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
