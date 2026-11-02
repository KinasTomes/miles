package coordination

import (
	"context"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
)

// ValidatedUTC adapts the existing policy validator. Sample must be supplied by
// a verified source adapter, not wall time or a caller's Trusted=true flag.
type ValidatedUTC struct {
	Clock   clock.Clock
	Source  policy.UTCSource
	Sample  func(context.Context) (policy.UTCSample, error)
	Horizon time.Duration
}

func (u ValidatedUTC) Bounds(ctx context.Context) (storage.R1Bounds, error) {
	if ctx == nil || u.Clock == nil || u.Sample == nil || u.Horizon <= 0 {
		return storage.R1Bounds{}, storage.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return storage.R1Bounds{}, err
	}
	sample, err := u.Sample(ctx)
	if err != nil {
		return storage.R1Bounds{}, err
	}
	now := u.Clock.Now().Instant
	deadline, err := now.Add(u.Horizon)
	if err != nil {
		return storage.R1Bounds{}, err
	}
	p, err := policy.ValidateUTCSample(u.Source, sample, policy.Request{Now: now, Deadline: deadline, Cancelled: ctx.Err() != nil})
	if err != nil {
		return storage.R1Bounds{}, err
	}
	return storage.R1Bounds{Lower: p.Lower, Upper: p.Upper, Horizon: p.Upper.Add(u.Horizon)}, nil
}

func NewSharedR1Gate(c clock.Clock, incarnation, owner string, limits LeaseLimits, store storage.R1Store, utc storage.R1TimeSource) (*WarmGate, error) {
	if owner == "" || store == nil || utc == nil {
		return nil, storage.ErrInvalid
	}
	nonce, err := storage.NewBucketID()
	if err != nil {
		return nil, err
	}
	g, err := NewR1Gate(c, incarnation, limits)
	if err != nil {
		return nil, err
	}
	g.shared, g.sharedTime, g.owner = store, utc, owner+"/"+nonce
	return g, nil
}
func (g *WarmGate) sharedDeadline(ctx context.Context, end clock.Instant) (time.Time, error) {
	remaining, err := end.Sub(g.clock.Now().Instant)
	if err != nil || remaining <= 0 {
		return time.Time{}, storage.ErrFenced
	}
	b, err := g.sharedTime.Bounds(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return b.Upper.Add(remaining), nil
}
func cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// AdmitProducer binds recovery identity to the exact shared lease admission.
// Registering before returning also lets a failed session release this lease;
// BeginOperation subsequently claims execution once, without sampling a new epoch.
func (r *WriterRegistry) AdmitProducer(ctx context.Context, object storage.ObjectKey, opID string, deadline clock.Instant) (storage.MutationContext, *Writer, error) {
	if r == nil || r.gate.shared == nil || object.Validate() != nil || opID == "" {
		return storage.MutationContext{}, nil, storage.ErrInvalid
	}
	g := r.gate
	id, err := storage.NewBucketID()
	if err != nil {
		return storage.MutationContext{}, nil, err
	}
	wall, err := g.sharedDeadline(ctx, deadline)
	if err != nil {
		return storage.MutationContext{}, nil, err
	}
	f := storage.WriterFence{LeaseID: id, Epoch: 1, ServiceIncarnation: g.incarnation}
	l, err := g.shared.AcquireR1(ctx, storage.R1Acquire{Fence: f, Owner: g.owner, Kind: "writer", OpID: opID, Object: object, Deadline: wall})
	if err != nil {
		return storage.MutationContext{}, nil, err
	}
	w, err := r.Register(ctx, f, deadline)
	if err != nil {
		cleanup, cancel := cleanupContext()
		defer cancel()
		if g.shared.CloseR1(cleanup, f) == nil {
			_ = g.shared.ReleaseR1(cleanup, f)
		}
		return storage.MutationContext{}, nil, err
	}
	r.mu.Lock()
	w.state.shared = true
	w.state.sharedDeadline = l.Deadline
	r.mu.Unlock()
	m := storage.MutationContext{HistoryID: l.HistoryID, Epoch: l.RecoveryEpoch, LogicalOpID: opID, MutationID: opID, Object: object, Writer: f}
	return m, w, m.Validate()
}

// Quiescent is checked only after the process has joined all request workers.
// Unknown synchronous outcomes retain shared protection and block a clean exit.
func (r *WriterRegistry) Quiescent() error {
	if r == nil {
		return storage.ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.writers {
		if !s.released {
			return ErrReconcilePending
		}
	}
	return nil
}

// BindRoot binds logical preparation before native page/canonical publication.
// Multipart callers bind upload/candidate IDs with the same admitted writer.
func (w *Writer) BindRoot(ctx context.Context, object storage.ObjectKey, id string) error {
	if w == nil || w.state == nil || id == "" {
		return storage.ErrInvalid
	}
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	if err := w.checkLocked(ctx); err != nil {
		return err
	}
	if w.owner.gate.shared == nil {
		return nil
	}
	if !w.state.shared || object != w.state.object {
		return storage.ErrFenced
	}
	return w.owner.gate.shared.BindR1(ctx, w.Fence(), storage.ObjectHead{Object: object, ManifestID: id})
}
