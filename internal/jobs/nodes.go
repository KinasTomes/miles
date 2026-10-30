package jobs

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

const HeartbeatInterval = 15 * time.Second
const HeartbeatMaxAge = 30 * time.Second

// Gate is the existing runtime admission boundary. Remote drain requests are
// durable markers; W5 routing/fan-out must reach the target to close its gate.
type Gate interface {
	Drain()
	Status() (int64, bool)
}
type NodeStatus struct {
	Ready                 bool
	ActiveRequests        int64
	Draining              bool
	AppliedPolicyRevision uint64
}
type Registry struct {
	Store            Store
	Identity         Node
	Clock            clock.Clock
	mu               sync.RWMutex
	last             clock.Instant
	hasLast, healthy bool
}

func (r *Registry) Healthy() bool {
	if r == nil || r.Clock == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.healthy || !r.hasLast {
		return false
	}
	age, e := r.Clock.Now().Instant.Sub(r.last)
	return e == nil && age >= 0 && age < HeartbeatMaxAge
}
func (r *Registry) Tick(ctx context.Context, gate Gate, status NodeStatus) error {
	if r.Store == nil || r.Clock == nil || gate == nil {
		return storage.ErrInvalid
	}
	e := r.tick(ctx, gate, status)
	r.mu.Lock()
	r.healthy = e == nil
	if e == nil {
		r.last = r.Clock.Now().Instant
		r.hasLast = true
	}
	r.mu.Unlock()
	return e
}
func (r *Registry) tick(ctx context.Context, gate Gate, status NodeStatus) error {
	for range 4 {
		old, e := r.Store.GetNode(ctx, r.Identity.ID)
		if e != nil && !errors.Is(e, storage.ErrNotFound) {
			return e
		}
		if e == nil && (old.Incarnation != r.Identity.Incarnation || old.ServiceIncarnation != r.Identity.ServiceIncarnation) {
			return storage.ErrFenced
		}
		if old.Draining || status.Draining {
			gate.Drain()
		}
		active, draining := gate.Status()
		n := r.Identity
		n.Revision = old.Revision
		n.HeartbeatAt = r.Clock.Now().WallUTC
		n.ActiveRequests = active
		n.Draining = draining || status.Draining || old.Draining
		n.Ready = status.Ready && !n.Draining
		n.AppliedPolicyRevision = status.AppliedPolicyRevision
		_, e = r.Store.PutNode(ctx, old.Revision, n)
		if errors.Is(e, storage.ErrConflict) {
			continue
		}
		return e
	}
	return storage.ErrConflict
}
func (r *Registry) Run(ctx context.Context, gate Gate, status func() NodeStatus) error {
	if r == nil || r.Clock == nil || status == nil {
		return storage.ErrInvalid
	}
	for ctx.Err() == nil {
		_ = r.Tick(ctx, gate, status()) // A failed heartbeat closes readiness via Healthy.
		if e := r.Clock.Sleep(ctx, HeartbeatInterval); e != nil {
			return e
		}
	}
	return ctx.Err()
}
func (r *Registry) RequestDrain(ctx context.Context, id string, gate Gate) (Node, error) {
	if r == nil || r.Store == nil || !ValidID(id) {
		return Node{}, storage.ErrInvalid
	}
	if id == r.Identity.ID {
		if gate == nil {
			return Node{}, storage.ErrInvalid
		}
		gate.Drain()
	}
	for range 4 {
		n, e := r.Store.GetNode(ctx, id)
		if e != nil {
			return Node{}, e
		}
		if n.Draining {
			return n, nil
		}
		n.Draining = true
		n.Ready = false
		out, e := r.Store.PutNode(ctx, n.Revision, n)
		if errors.Is(e, storage.ErrConflict) {
			continue
		}
		return out, e
	}
	return Node{}, storage.ErrConflict
}
