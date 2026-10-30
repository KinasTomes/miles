package app

import (
	"context"
	"errors"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/observability"
	"github.com/KinasTomes/miles/internal/storage"
)

// Operational wiring is explicit: bootstrap never invents a node identity,
// shared store or AWS credential provider. W5 supplies them with real runtime R1.
type Operational struct {
	Registry   *jobs.Registry
	Metrics    *observability.Operations
	CloudWatch *observability.CloudWatch
}

func (o *Operational) validate() error {
	if o == nil {
		return nil
	}
	if o.Registry == nil || o.Registry.Store == nil || o.Registry.Clock == nil {
		return storage.ErrInvalid
	}
	n := o.Registry.Identity
	n.Revision = 1
	n.HeartbeatAt = o.Registry.Clock.Now().WallUTC
	n.Ready = false
	return n.Validate()
}
func (o *Operational) run(ctx context.Context, h *observability.Health, a *Admission) error {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	count := 1
	result := make(chan error, 2)
	go func() {
		result <- o.Registry.Run(ctx, a, func() jobs.NodeStatus {
			return jobs.NodeStatus{Ready: h.Ready(), AppliedPolicyRevision: o.Registry.Identity.AppliedPolicyRevision}
		})
	}()
	if o.CloudWatch != nil {
		count++
		go func() { result <- o.CloudWatch.Run(ctx, o.Registry.Clock, h.Snapshot) }()
	}
	var out error
	for range count {
		e := <-result
		if e != nil && !errors.Is(e, context.Canceled) && !(parent.Err() != nil && errors.Is(e, parent.Err())) {
			out = errors.Join(out, e)
		}
		cancel()
	}
	return out
}
