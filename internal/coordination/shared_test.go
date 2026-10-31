package coordination

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/policy"
)

func TestSharedUTCRejectsSkewStaleAndUnverifiedSource(t *testing.T) {
	for _, scenario := range []string{"valid", "skew", "stale", "source", "horizon", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			wall := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
			c, _ := clock.NewVirtual(wall, 100)
			observed := c.Now().Instant
			through, _ := observed.Add(10 * time.Minute)
			source := policy.UTCSource{ID: "injected", Provenance: "bounded-test-oracle", RestartID: "run-1", Verified: policy.Valid}
			sample := policy.UTCSample{SourceID: source.ID, Provenance: source.Provenance, RestartID: source.RestartID, Observed: observed, ValidThrough: through, Lower: wall, Upper: wall, Precision: policy.DurationBound{Known: true, Value: time.Nanosecond}, MeasurementError: policy.DurationBound{Known: true}, NodeSkew: policy.DurationBound{Known: true}, Drift: policy.DriftBound{Known: true}, Sync: policy.Valid}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "skew":
				sample.NodeSkew.Value = policy.MaxNodeSkew + time.Nanosecond
			case "stale":
				if err := c.Advance(policy.MaxUTCSampleAge); err != nil {
					t.Fatal(err)
				}
			case "source":
				source.Verified = policy.Unknown
			case "horizon":
				sample.ValidThrough, _ = observed.Add(time.Second)
			case "cancelled":
				cancel()
			}
			adapter := ValidatedUTC{Clock: c, Source: source, Horizon: time.Minute, Sample: func(context.Context) (policy.UTCSample, error) { return sample, nil }}
			bounds, err := adapter.Bounds(ctx)
			if scenario == "valid" {
				if err != nil || bounds.Lower.After(wall) || !bounds.Upper.After(wall) || !bounds.Horizon.After(bounds.Upper) {
					t.Fatal(bounds, err)
				}
			} else if err == nil {
				t.Fatal("unproven UTC became authority", bounds)
			}
			if scenario == "stale" && !errors.Is(err, policy.ErrUTCStale) {
				t.Fatal(err)
			}
		})
	}
}
