package app

import (
	"context"
	"errors"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/observability"
	"github.com/KinasTomes/miles/internal/storage"
)

// Operational wiring is explicit: bootstrap never invents a node identity,
// shared store or AWS credential provider. W5 supplies them with real runtime R1.
type Operational struct {
	Registry        *jobs.Registry
	Metrics         *observability.Operations
	Runtime         *observability.Runtime
	Collect         func(context.Context) error
	CollectInterval time.Duration
	CloudWatch      *observability.CloudWatch
	Cluster         *jobs.Cluster
	OnDrain         func(context.Context) error
}

func (o *Operational) validate() error {
	if o == nil {
		return nil
	}
	if o.Registry == nil || o.Registry.Store == nil || o.Registry.Clock == nil || o.Collect != nil && o.Runtime == nil {
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
	workers := []func() error{func() error {
		return o.Registry.Run(ctx, a, func() jobs.NodeStatus {
			return jobs.NodeStatus{Ready: h.Ready(), AppliedPolicyRevision: o.Registry.Identity.AppliedPolicyRevision}
		})
	}}
	if o.Cluster != nil {
		workers = append(workers, func() error { return o.Cluster.Runner.Run(ctx) })
	}
	if o.OnDrain != nil {
		workers = append(workers, func() error {
			for ctx.Err() == nil {
				if a.Draining() {
					return o.OnDrain(ctx)
				}
				if e := o.Registry.Clock.Sleep(ctx, time.Second); e != nil {
					return e
				}
			}
			return ctx.Err()
		})
	}
	if o.Collect != nil {
		interval := o.CollectInterval
		if interval <= 0 {
			interval = 30 * time.Second
		}
		workers = append(workers, func() error {
			for ctx.Err() == nil {
				call, done := context.WithTimeout(ctx, 10*time.Second)
				err := o.Collect(call)
				done()
				if err != nil {
					o.Runtime.ObservationFailure()
				}
				if err = o.Registry.Clock.Sleep(ctx, interval); err != nil {
					return err
				}
			}
			return ctx.Err()
		})
	}
	if o.CloudWatch != nil {
		workers = append(workers, func() error { return o.CloudWatch.Run(ctx, o.Registry.Clock, h.Snapshot) })
	}
	result := make(chan error, len(workers))
	for _, work := range workers {
		go func(run func() error) { result <- run() }(work)
	}
	var out error
	for range workers {
		e := <-result
		if e != nil && !errors.Is(e, context.Canceled) && !(parent.Err() != nil && errors.Is(e, parent.Err())) {
			out = errors.Join(out, e)
		}
		cancel()
	}
	return out
}
