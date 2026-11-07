package coordination

import (
	"context"
	"encoding/csv"
	"fmt"
	"math"
	"net/netip"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
)

// Chrony reads the local, operator-managed chronyd. Reference pins its approved
// upstream; neither an HTTP header nor an AWS response Date supplies UTC proof.
// The host must disallow clock steps after startup and trust the NTP upstream.
type Chrony struct {
	Clock     clock.Clock
	Reference string
	RestartID string
	read      func(context.Context) ([]byte, error)
}

var (
	// ErrClockUncertain indicates the host NTP synchronization fails uncertainty,
	// stratum, leap status, frequency drift or sample age bounds.
	ErrClockUncertain = fmt.Errorf("%w: chrony source is not synchronized with bounded uncertainty", storage.ErrClockUncertain)
	// ErrClockUnavailable indicates chronyc tracking could not be queried.
	ErrClockUnavailable = fmt.Errorf("%w: chrony tracking unavailable", storage.ErrUnavailable)
	// ErrClockChanged indicates an uncoordinated clock step or skew during sampling.
	ErrClockChanged = fmt.Errorf("%w: clock changed during chrony observation", storage.ErrClockUncertain)
)

const runtimeHorizon = 45 * time.Minute

func (c Chrony) Bounds(ctx context.Context) (storage.R1Bounds, error) {
	if c.Clock == nil || c.RestartID == "" {
		return storage.R1Bounds{}, storage.ErrInvalid
	}
	start := c.Clock.Now()
	read := c.read
	if read == nil {
		read = func(ctx context.Context) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/chronyc", "-n", "-c", "-h", "127.0.0.1", "tracking")
			cmd.Env = []string{"LC_ALL=C", "PATH=/usr/bin:/bin"}
			return cmd.Output()
		}
	}
	body, err := read(ctx)
	if err != nil {
		return storage.R1Bounds{}, ErrClockUnavailable
	}
	end := c.Clock.Now()
	elapsed, err := end.Instant.Sub(start.Instant)
	if err != nil || elapsed < 0 || elapsed >= time.Second || math.Abs(end.WallUTC.Sub(start.WallUTC).Seconds()-elapsed.Seconds()) > 0.01 {
		return storage.R1Bounds{}, ErrClockChanged
	}
	sample, err := c.parse(body, end, elapsed)
	if err != nil {
		return storage.R1Bounds{}, err
	}
	source := policy.UTCSource{ID: c.Reference, Provenance: "local-chronyd/tracking", RestartID: c.RestartID, Verified: policy.Valid}
	return (ValidatedUTC{Clock: c.Clock, Source: source, Horizon: runtimeHorizon, Sample: func(context.Context) (policy.UTCSample, error) { return sample, nil }}).Bounds(ctx)
}

func (c Chrony) parse(body []byte, now clock.Reading, elapsed time.Duration) (policy.UTCSample, error) {
	bad := ErrClockUncertain
	if len(body) > 4096 {
		return policy.UTCSample{}, bad
	}
	rows, err := csv.NewReader(strings.NewReader(string(body))).ReadAll()
	if err != nil || len(rows) != 1 || len(rows[0]) != 14 {
		return policy.UTCSample{}, bad
	}
	r := rows[0]
	ip, err := netip.ParseAddr(c.Reference)
	if err != nil || !ip.Is4() || r[1] != c.Reference {
		return policy.UTCSample{}, bad
	}
	a := ip.As4()
	reference := uint64(a[0])<<24 | uint64(a[1])<<16 | uint64(a[2])<<8 | uint64(a[3])
	id, e := strconv.ParseUint(r[0], 16, 32)
	if e != nil || id != reference {
		return policy.UTCSample{}, bad
	}
	numbers := make([]float64, 12)
	for i := 2; i < 13; i++ {
		n, e := strconv.ParseFloat(r[i], 64)
		if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
			return policy.UTCSample{}, bad
		}
		numbers[i-2] = n
	}
	stratum, refTime, offset := numbers[0], numbers[1], numbers[2]
	freq, residual, skew, delay, dispersion, interval := numbers[5], numbers[6], numbers[7], numbers[8], numbers[9], numbers[10]
	age := float64(now.WallUTC.UnixNano())/1e9 - refTime
	if stratum < 1 || stratum > 15 || math.Trunc(stratum) != stratum || r[13] != "Normal" || age < -2 || age > min(1024, 2*interval+2) || interval <= 0 || delay < 0 || dispersion < 0 || skew < 0 || math.Abs(offset) > 2 {
		return policy.UTCSample{}, bad
	}
	// chronyc's documented accuracy bound, padded for the command and 45m
	// of frequency uncertainty. Keep this conservative; reject wide horizons.
	driftPPM := math.Abs(freq) + math.Abs(residual) + skew + 1
	errorSeconds := math.Abs(offset) + dispersion + delay/2 + elapsed.Seconds() + 0.01
	if driftPPM > 500 || errorSeconds+driftPPM*1e-6*runtimeHorizon.Seconds() > 2 {
		return policy.UTCSample{}, bad
	}
	through, e := now.Instant.Add(runtimeHorizon + time.Second)
	if e != nil {
		return policy.UTCSample{}, e
	}
	return policy.UTCSample{SourceID: c.Reference, Provenance: "local-chronyd/tracking", RestartID: c.RestartID, Observed: now.Instant, ValidThrough: through, Lower: now.WallUTC, Upper: now.WallUTC,
		Precision: policy.DurationBound{Known: true, Value: time.Duration(math.Ceil(errorSeconds * 1e9))}, MeasurementError: policy.DurationBound{Known: true}, NodeSkew: policy.DurationBound{Known: true, Value: time.Duration(offset * 1e9)}, Drift: policy.DriftBound{Known: true, PPB: uint64(math.Ceil(driftPPM * 1000))}, Sync: policy.Valid}, nil
}
