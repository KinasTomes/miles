package app

import (
	"context"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/observability"
)

func runtimeObserver(store *dynamodb.Store, metrics *observability.Runtime, clk clock.Clock, history string, deep bool) func(context.Context) error {
	var closedSince time.Time
	return func(ctx context.Context) error {
		now := clk.Now().WallUTC
		out, err := store.ObserveRuntime(ctx, history, now, deep)
		if err != nil {
			return err
		}
		if !out.GateClosed {
			closedSince = time.Time{}
		} else if !out.GateStartedAt.IsZero() {
			closedSince = out.GateStartedAt
		} else if closedSince.IsZero() {
			closedSince = now
		}
		duration := 0.0
		if !closedSince.IsZero() {
			duration = now.Sub(closedSince).Seconds()
		}
		var lag *float64
		var stuck *int
		if out.DeepScanned {
			l := 0.0
			if !out.OldestPendingExport.IsZero() {
				l = now.Sub(out.OldestPendingExport).Seconds()
			}
			lag = &l
			s := out.StuckCompleting
			stuck = &s
		}
		metrics.Observe(now, now.Sub(out.CutAt).Seconds(), duration, lag, stuck)
		return nil
	}
}
