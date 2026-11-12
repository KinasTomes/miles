package coordination

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestChronyBoundsAndUntrustedObservations(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	valid := fmt.Sprintf("A9FEA97B,169.254.169.123,3,%d,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n", now.Unix())
	for _, name := range []string{"valid", "unsynced", "wrong-reference", "local-clock", "nan", "infinite", "wide", "old", "future", "command-late", "clock-step", "trailing", "missing"} {
		t.Run(name, func(t *testing.T) {
			clk, _ := clock.NewVirtual(now, 100)
			body := valid
			switch name {
			case "unsynced":
				body = strings.Replace(body, "Normal", "Not synchronised", 1)
			case "wrong-reference":
				body = strings.Replace(body, "169.254.169.123", "1.2.3.4", 1)
			case "local-clock":
				body = strings.Replace(body, "A9FEA97B", "7F7F0101", 1)
			case "nan":
				body = strings.Replace(body, ",0.001,", ",NaN,", 1)
			case "infinite":
				body = strings.Replace(body, ",0.001,", ",+Inf,", 1)
			case "wide":
				body = strings.Replace(body, ",0.1,0.01,", ",500,0.01,", 1)
			case "old":
				body = strings.Replace(body, fmt.Sprint(now.Unix()), fmt.Sprint(now.Add(-time.Hour).Unix()), 1)
			case "future":
				body = strings.Replace(body, fmt.Sprint(now.Unix()), fmt.Sprint(now.Add(time.Hour).Unix()), 1)
			case "trailing":
				body += valid
			case "missing":
				body = ""
			}
			c := Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: "process-1", read: func(context.Context) ([]byte, error) {
				if name == "command-late" {
					_ = clk.Advance(time.Second)
				}
				if name == "clock-step" {
					_ = clk.SetWall(now.Add(time.Hour))
				}
				return []byte(body), nil
			}}
			b, err := c.Bounds(context.Background())
			if name == "valid" {
				if err != nil || !b.Lower.Before(now) || !b.Upper.After(now) || b.Horizon.Sub(b.Upper) != runtimeHorizon {
					t.Fatal(b, err)
				}
			} else if err == nil {
				t.Fatal("untrusted clock accepted", b)
			}
		})
	}
}

func TestChronyStaleBoundaryAndSelfRecovery(t *testing.T) {
	now := time.Date(2026, 11, 6, 5, 28, 0, 0, time.UTC)
	clk, _ := clock.NewVirtual(now, 100)
	// interval = 16s. Bound is min(1024, 2*16 + 2) = 34s.
	pollInterval := 16.0
	maxAllowedAge := 2*pollInterval + 2 // 34.0 seconds

	testCases := []struct {
		name      string
		ageOffset float64 // seconds before 'now'
		wantPass  bool
	}{
		{"age_well_within_bounds", 5.0, true},
		{"age_one_poll_gap", 16.0, true},
		{"age_two_poll_gap_within_limit", 32.0, true},
		{"age_exactly_at_bound", maxAllowedAge, true},
		{"age_slightly_exceeded", maxAllowedAge + 0.5, false},
		{"age_three_poll_gap_exceeded", 48.0, false},
		{"age_live_incident_49s_gap", 49.2, false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			refTime := float64(now.UnixNano())/1e9 - tc.ageOffset
			sampleLine := fmt.Sprintf("A9FEA97B,169.254.169.123,3,%.4f,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,%.1f,Normal\n", refTime, pollInterval)
			c := Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: "node-a", read: func(context.Context) ([]byte, error) {
				return []byte(sampleLine), nil
			}}
			_, err := c.Bounds(context.Background())
			if tc.wantPass {
				if err != nil {
					t.Fatalf("expected age=%.1fs to pass, got err: %v", tc.ageOffset, err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected age=%.1fs to fail-closed, but passed", tc.ageOffset)
				}
				if !errors.Is(err, storage.ErrClockUncertain) {
					t.Fatalf("expected ErrClockUncertain, got: %v", err)
				}
			}
		})
	}

	t.Run("self_recovery_on_fresh_observation", func(t *testing.T) {
		currentAge := 49.2 // exceeds 34s bound
		c := Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: "node-a", read: func(context.Context) ([]byte, error) {
			refTime := float64(now.UnixNano())/1e9 - currentAge
			sample := fmt.Sprintf("A9FEA97B,169.254.169.123,3,%.4f,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,%.1f,Normal\n", refTime, pollInterval)
			return []byte(sample), nil
		}}

		// 1. Initial attempt fails closed because NTP is stale (49.2s > 34s)
		_, err := c.Bounds(context.Background())
		if err == nil || !errors.Is(err, storage.ErrClockUncertain) {
			t.Fatalf("expected ErrClockUncertain during stale period, got %v", err)
		}

		// 2. Next NTP packet arrives, age is reset to 0.5s
		currentAge = 0.5
		b, err := c.Bounds(context.Background())
		if err != nil {
			t.Fatalf("expected successful recovery after fresh sample, got: %v", err)
		}
		if !b.Lower.Before(now) || !b.Upper.After(now) {
			t.Fatalf("recovered bounds invalid: lower=%v, upper=%v, now=%v", b.Lower, b.Upper, now)
		}
	})
}

func TestChronyTypedErrors(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	clk, _ := clock.NewVirtual(now, 100)

	// Tracking unavailable wraps storage.ErrUnavailable
	cUnavail := Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: "p1", read: func(context.Context) ([]byte, error) {
		return nil, errors.New("read failed")
	}}
	_, err := cUnavail.Bounds(context.Background())
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got: %v", err)
	}

	// Clock step during observation wraps storage.ErrClockUncertain
	cStep := Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: "p1", read: func(context.Context) ([]byte, error) {
		_ = clk.SetWall(now.Add(time.Hour))
		sample := fmt.Sprintf("A9FEA97B,169.254.169.123,3,%d,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16,Normal\n", now.Unix())
		return []byte(sample), nil
	}}
	_, err = cStep.Bounds(context.Background())
	if !errors.Is(err, storage.ErrClockUncertain) {
		t.Fatalf("expected ErrClockUncertain on clock step, got: %v", err)
	}
}

func TestChronyRecordedConsecutiveRejectionsAndFreshRecovery(t *testing.T) {
	// Replay tracking freshness from the live incident. Rejected NTP replies
	// leave the accepted reference unchanged; this does not simulate Chrony's
	// packet filters or prove that a different polling profile will recover.
	ref := time.Date(2026, 11, 11, 13, 41, 54, 530157800, time.UTC)
	for _, sample := range []struct {
		age   time.Duration
		fresh bool
	}{
		{17 * time.Second, true},
		{19 * time.Second, true},
		{34199 * time.Millisecond, true},
		{34252023 * time.Microsecond, false},
		{347999 * time.Millisecond / 10, false},
		{368087 * time.Millisecond / 10, false},
	} {
		t.Run(sample.age.String(), func(t *testing.T) {
			clk, err := clock.NewVirtual(ref.Add(sample.age), 100)
			if err != nil {
				t.Fatal(err)
			}
			accepted := ref
			c := Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: "replay", read: func(context.Context) ([]byte, error) {
				return []byte(fmt.Sprintf("A9FEA97B,169.254.169.123,3,%.7f,0.001,0.001,0.001,0.1,0.01,0.1,0.001,0.001,16.1,Normal\n", float64(accepted.UnixNano())/1e9)), nil
			}}
			_, err = c.Bounds(t.Context())
			if sample.fresh && err != nil || !sample.fresh && !errors.Is(err, storage.ErrClockUncertain) {
				t.Fatal("recorded freshness disagrees with the unchanged 34.2s guard", sample, err)
			}
			accepted = clk.Now().WallUTC.Add(-500 * time.Millisecond)
			if _, err = c.Bounds(t.Context()); err != nil {
				t.Fatal("new accepted reference failed to restore bounded time", err)
			}
		})
	}
}
