package coordination

import (
	"context"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

// LeaseLimits are explicit local integration settings. Defaults and shared UTC
// evidence remain owned by Clock/Policy. No UTC proof is inferred from wall time.
type LeaseLimits struct{ Lifetime, Heartbeat time.Duration }

func (l LeaseLimits) Validate() error {
	if l.Lifetime <= 0 || l.Heartbeat <= 0 || l.Heartbeat >= l.Lifetime {
		return storage.ErrInvalid
	}
	return nil
}

func leaseEnd(c clock.Clock, deadline clock.Instant, lifetime time.Duration) (clock.Instant, error) {
	next, err := c.Now().Instant.Add(lifetime)
	if err != nil {
		return clock.Instant{}, err
	}
	cmp, err := next.Compare(deadline)
	if err != nil {
		return clock.Instant{}, err
	}
	if cmp > 0 {
		return deadline, nil
	}
	return next, nil
}

func keepAlive(ctx context.Context, c clock.Clock, interval time.Duration, renew func(context.Context) error) (context.Context, func(), error) {
	if ctx == nil {
		return nil, nil, storage.ErrInvalid
	}
	if interval == 0 {
		return ctx, func() {}, nil
	} // legacy Warm-only gate
	child, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if c.Sleep(child, interval) != nil {
				cancel()
				return
			}
			if renew(child) != nil {
				cancel()
				return
			}
		}
	}()
	return child, func() { cancel(); <-done }, nil
}
