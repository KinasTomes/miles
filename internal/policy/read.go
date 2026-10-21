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
		if !in.UTC.Trusted || !in.UTC.validated {
			d.Reason = MissingEvidence
			return d, nil
		}
		u := in.UTC
		if !validUTC(u.Lower) || !validUTC(u.Upper) || u.Lower.After(u.Upper) || !validUTC(p.RestoredUntil) {
			d.Reason = InvalidEvidence
			return d, ErrEvidence
		}
		cmp, e := u.Horizon.Compare(in.Request.Deadline)
		if e != nil {
			d.Reason = InvalidEvidence
			return d, e
		}
		if cmp < 0 {
			d.Reason = MissingEvidence
			return d, nil
		}
		remaining, e := in.Request.Deadline.Sub(in.Request.Now)
		if e != nil {
			d.Reason = InvalidEvidence
			return d, e
		}
		// Separate additions avoid duration overflow in remaining+margin.
		if err := validateUTCBinding(u, in.Request); err != nil {
			d.Reason = InvalidEvidence
			return d, err
		}
		required := u.Upper.UTC().Add(remaining).Add(120 * time.Second)
		if !validUTC(required) {
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
