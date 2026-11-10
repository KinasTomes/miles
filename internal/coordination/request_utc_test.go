package coordination

import (
	"context"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
)

type requestTime func(context.Context) (storage.R1Bounds, error)

func (f requestTime) Bounds(ctx context.Context) (storage.R1Bounds, error) { return f(ctx) }

func TestRequestUTCConservativeBridge(t *testing.T) {
	for _, delay := range []time.Duration{0, time.Second, 5*time.Second - time.Nanosecond, 5 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			wall := time.Date(1960, 1, 1, 0, 0, 0, 0, time.UTC)
			c, _ := clock.NewVirtual(wall, 100)
			deadline, _ := c.Now().Instant.Add(time.Minute)
			source := requestTime(func(context.Context) (storage.R1Bounds, error) {
				if e := c.Advance(delay); e != nil {
					t.Fatal(e)
				}
				return storage.R1Bounds{Lower: wall.Add(-time.Millisecond), Upper: wall.Add(time.Millisecond), Horizon: wall.Add(time.Hour)}, nil
			})
			r, p, e := RequestUTC(t.Context(), c, source, deadline)
			if delay >= 5*time.Second {
				if e == nil {
					t.Fatal("stale source accepted")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if p.Lower.After(wall.Add(-time.Millisecond)) || p.Upper.Before(wall.Add(delay+time.Millisecond)) {
				t.Fatal("uncertainty narrowed", p)
			}
			required, e := policy.RestoreRequiredUntil(r, p)
			if e != nil || required.Before(wall.Add(3*time.Minute+time.Millisecond)) {
				t.Fatal(required, e)
			}
		})
	}
}

func TestRequestUTCRejectsInvalidHorizonAndDomain(t *testing.T) {
	wall := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	c, _ := clock.NewVirtual(wall, 10)
	other, _ := clock.NewVirtual(wall, 10)
	end, _ := c.Now().Instant.Add(time.Minute)
	foreign, _ := other.Now().Instant.Add(time.Minute)
	for _, mode := range []string{"horizon", "interval", "domain", "missing"} {
		t.Run(mode, func(t *testing.T) {
			d := end
			if mode == "domain" {
				d = foreign
			}
			source := requestTime(func(context.Context) (storage.R1Bounds, error) {
				b := storage.R1Bounds{Lower: wall, Upper: wall, Horizon: wall.Add(time.Hour)}
				if mode == "horizon" {
					b.Horizon = wall.Add(time.Second)
				}
				if mode == "interval" {
					b.Lower = wall.Add(time.Second)
				}
				if mode == "missing" {
					b.Lower = time.Time{}
				}
				return b, nil
			})
			if _, _, e := RequestUTC(t.Context(), c, source, d); e == nil {
				t.Fatal("invalid UTC bridge accepted")
			}
		})
	}
}
