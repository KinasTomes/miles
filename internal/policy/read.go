package policy

import (
	"github.com/KinasTomes/miles/internal/storage"
	"time"
)

// EvaluateRead is payload-read eligibility (GET/Copy payload), not metadata-only
// Copy semantics or a guarantee of successful I/O. Cache cannot bypass it.
// Sources: Clock §3.5/7, R1 §14, Engine §5.2.
func EvaluateRead(in ReadInput) (Decision, error) {
	d, ok, err := base(in.Policy, in.Request)
	if !ok {
		return d, err
	}
	f := in.Facts
	d, ok, err = facts(d, f.Logical, f.Metadata, f.Authority, f.PinCoverage, f.Physical)
	if !ok {
		return d, err
	}
	if !validPack(in.Pack) {
		d.Reason = InvalidEvidence
		return d, ErrEvidence
	}
	p := in.Pack
	switch {
	case p.State == "Warm" && (p.Class == storage.Standard || p.Class == storage.StandardIA):
	case p.State == "Cold" && p.Class == storage.GlacierIR:
	case (p.State == "Cold" || p.State == "Restoring") && p.Class == storage.Glacier:
		d.Verdict = Denied
		d.Reason = RestoreRequired
		return d, nil
	case p.State == "Restored" && p.Class == storage.Glacier:
		d, ok, err = facts(d, in.RestoreVerified)
		if !ok {
			return d, err
		}
		required, err := RestoreRequiredUntil(in.Request, in.UTC)
		if err != nil {
			d.Reason = InvalidEvidence
			return d, err
		}
		if required.IsZero() {
			d.Reason = MissingEvidence
			return d, nil
		}
		if !validUTC(p.RestoredUntil) {
			d.Reason = InvalidEvidence
			return d, ErrEvidence
		}
		if p.RestoredUntil.UTC().Before(required) {
			d.Verdict = Denied
			d.Reason = RestoreRequired
			return d, nil
		}
	case p.State == "Transitioning" || p.State == "Retiring" || p.State == "Deleting" || p.State == "Uploading" || p.State == "Compacting" || p.State == "Retired" || p.State == "Deleted":
		d.Verdict = Denied
		d.Reason = Unavailable
		return d, nil
	default:
		d.Reason = InvalidEvidence
		return d, ErrEvidence
	}
	d.Verdict = Eligible
	d.Reason = Ready
	return d, nil
}

func validUTC(t time.Time) bool { return !t.IsZero() && t.UTC().Year() >= 1 && t.UTC().Year() <= 9999 }

// RestoreRequiredUntil returns the conservative expiry threshold for GET/Copy.
// A zero threshold means there is no trusted proof covering the request.
func RestoreRequiredUntil(request Request, u UTCProof) (time.Time, error) {
	if !u.Trusted || !u.validated {
		return time.Time{}, nil
	}
	if !validUTC(u.Lower) || !validUTC(u.Upper) || u.Lower.After(u.Upper) {
		return time.Time{}, ErrEvidence
	}
	cmp, err := u.Horizon.Compare(request.Deadline)
	if err != nil || cmp < 0 {
		return time.Time{}, err
	}
	remaining, err := request.Deadline.Sub(request.Now)
	if err != nil {
		return time.Time{}, err
	}
	if remaining <= 0 {
		return time.Time{}, ErrEvidence
	}
	if err := validateUTCBinding(u, request); err != nil {
		return time.Time{}, err
	}
	required := u.Upper.UTC().Add(remaining).Add(120 * time.Second)
	if !validUTC(required) {
		return time.Time{}, ErrEvidence
	}
	return required, nil
}

// CopyReuseCandidate extends write reuse with temporary restore eligibility.
func CopyReuseCandidate(p storage.PackInfo, restoreRequiredUntil time.Time) (bool, error) {
	if p.State == "Restored" && p.Class == storage.Glacier && !restoreRequiredUntil.IsZero() {
		if !validUTC(restoreRequiredUntil) || !validUTC(p.RestoredUntil) {
			return false, ErrEvidence
		}
		return !p.RestoredUntil.Before(restoreRequiredUntil), nil
	}
	return WriteReuseCandidate(p.State, p.Class)
}
