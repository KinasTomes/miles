package policy

import (
	"errors"
	"math/big"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
)

const MaxUTCSampleAge = 5 * time.Second
const MaxNodeSkew = 2 * time.Second

var (
	ErrUTCStale    = errors.New("policy: stale UTC sample")
	ErrUTCSource   = errors.New("policy: unknown or mismatched UTC source provenance")
	ErrUTCUnsynced = errors.New("policy: UTC source unsynced")
	ErrUTCHorizon  = errors.New("policy: insufficient UTC validity horizon")
)

// UTCSource là handoff từ adapter/registry đã xác minh, không phải proof mật mã.
// Adapter chịu trách nhiệm xác thực provenance, restart và bounds thực tế.
// Concrete API là PROPOSAL, chưa tích hợp OS/chrony.
type UTCSource struct {
	ID, Provenance, RestartID string
	Verified                  Fact
}
type DurationBound struct {
	Known bool
	Value time.Duration
}
type DriftBound struct {
	Known bool
	PPB   uint64
}

// UTCSample.Observed cùng domain với Request.Now/Deadline và ValidThrough.
// Lower/Upper là UTC tại Observed, chưa cộng Precision/MeasurementError/Drift.
// NodeSkew là offset có dấu do nguồn đã xác minh cung cấp, không phải SigV4.
// Known=true và Value=0 biểu diễn bound zero explicit, không có default ngầm.
type UTCSample struct {
	SourceID, Provenance, RestartID       string
	Observed, ValidThrough                clock.Instant
	Lower, Upper                          time.Time
	Precision, MeasurementError, NodeSkew DurationBound
	Drift                                 DriftBound
	Sync                                  Fact
}

// ValidateUTCSample tính interval tại đúng Request.Now, bao uncertainty qua
// toàn hard deadline. Không I/O, không host wall/timer, không cấp authority.
func ValidateUTCSample(source UTCSource, sample UTCSample, request Request) (UTCProof, error) {
	if source.Verified != Valid || source.ID == "" || source.Provenance == "" || source.RestartID == "" || sample.SourceID != source.ID || sample.Provenance != source.Provenance || sample.RestartID != source.RestartID {
		return UTCProof{}, ErrUTCSource
	}
	if sample.Sync != Valid {
		return UTCProof{}, ErrUTCUnsynced
	}
	if !sample.Precision.Known || !sample.MeasurementError.Known || !sample.NodeSkew.Known || !sample.Drift.Known || sample.Precision.Value < 0 || sample.MeasurementError.Value < 0 {
		return UTCProof{}, ErrEvidence
	}
	if sample.NodeSkew.Value < -MaxNodeSkew || sample.NodeSkew.Value > MaxNodeSkew {
		return UTCProof{}, ErrEvidence
	}
	if !validUTC(sample.Lower) || !validUTC(sample.Upper) || sample.Lower.After(sample.Upper) {
		return UTCProof{}, ErrEvidence
	}
	if request.Cancelled {
		return UTCProof{}, ErrEvidence
	}
	cmp, err := request.Now.Compare(request.Deadline)
	if err != nil {
		return UTCProof{}, err
	}
	if cmp >= 0 {
		return UTCProof{}, ErrEvidence
	}
	age, err := request.Now.Sub(sample.Observed)
	if err != nil {
		return UTCProof{}, err
	}
	if age < 0 {
		return UTCProof{}, ErrEvidence
	}
	if age >= MaxUTCSampleAge {
		return UTCProof{}, ErrUTCStale
	}
	cmp, err = sample.ValidThrough.Compare(request.Deadline)
	if err != nil {
		return UTCProof{}, err
	}
	if cmp < 0 {
		return UTCProof{}, ErrUTCHorizon
	}
	horizon, err := request.Deadline.Sub(sample.Observed)
	if err != nil {
		return UTCProof{}, err
	}
	// ceil(horizon_ns * drift_ppb / 1e9), tính exact và kiểm Duration overflow.
	drift := new(big.Int).Mul(big.NewInt(int64(horizon)), new(big.Int).SetUint64(sample.Drift.PPB))
	drift.Add(drift, big.NewInt(999999999))
	drift.Quo(drift, big.NewInt(1000000000))
	pad := drift.Add(drift, big.NewInt(int64(sample.Precision.Value)))
	pad.Add(pad, big.NewInt(int64(sample.MeasurementError.Value)))
	if !pad.IsInt64() {
		return UTCProof{}, clock.ErrOverflow
	}
	errorBudget := time.Duration(pad.Int64())
	lower := sample.Lower.UTC().Add(age).Add(-errorBudget)
	upper := sample.Upper.UTC().Add(age).Add(errorBudget)
	if !validUTC(lower) || !validUTC(upper) || lower.After(upper) {
		return UTCProof{}, clock.ErrOverflow
	}
	return UTCProof{Trusted: true, Lower: lower, Upper: upper, Horizon: request.Deadline, validated: true, boundNow: request.Now, boundLower: lower, boundUpper: upper, boundHorizon: request.Deadline}, nil
}

// Chặn Trusted=true tự đặt, sửa interval/horizon và tái dùng proof khi Now đổi.
func validateUTCBinding(p UTCProof, r Request) error {
	if !p.validated || !p.Trusted {
		return ErrEvidence
	}
	cmp, err := p.boundNow.Compare(r.Now)
	if err != nil {
		return err
	}
	if cmp != 0 {
		return ErrEvidence
	}
	cmp, err = p.boundHorizon.Compare(p.Horizon)
	if err != nil {
		return err
	}
	if cmp != 0 {
		return ErrEvidence
	}
	if !p.Lower.UTC().Equal(p.boundLower) || !p.Upper.UTC().Equal(p.boundUpper) {
		return ErrEvidence
	}
	return nil
}
