package clock

import (
	"context"
	"runtime"
	"time"
)

// Real retains Go's monotonic anchor separately from normalized wall readings.
// Its elapsed horizon is time.Duration (about 292 years per process). Like Go's
// monotonic clock, it may stop during host suspend on some systems. Consumers
// must invalidate lease-use assumptions on resume and obtain fresh proof.
type Real struct {
	domain *domain
	origin time.Time
}

func NewReal() *Real { return &Real{domain: &domain{}, origin: time.Now()} }

func (r *Real) Now() Reading {
	now := time.Now()
	return Reading{WallUTC: now.UTC(), Instant: Instant{domain: r.domain, ticks: int64(now.Sub(r.origin))}}
}

func (r *Real) Sleep(ctx context.Context, d time.Duration) error {
	if d < 0 {
		return ErrDuration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == 0 {
		runtime.Gosched()
		return ctx.Err()
	}
	if _, err := r.Now().Instant.Add(d); err != nil {
		return err
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err() // Cancellation wins if already observable.
	}
}

var _ Clock = (*Real)(nil)
