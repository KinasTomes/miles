package policy

import (
	"math/big"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

// TieringInput contains observations, not execution authority. SafeToTransition
// requires caller-established R1 exclusion of readers/writers/uploads and R2
// roots, bound to this exact generation/state revision. Unknown always defers.
// LiveBytes counts unique live stored blobs, never multiplied by refcount.
type TieringInput struct {
	Policy                                PinnedPolicy
	Request                               Request
	UTC                                   UTCProof
	Pack                                  storage.PackInfo
	LiveBytes, TotalBytes                 int64
	Metadata, Authority, SafeToTransition Fact
}

type TieringDecision struct {
	Decision
	Destination storage.StorageClass
}

const (
	TieringDisabled Reason = "tiering-disabled"
	PackTooYoung    Reason = "pack-too-young"
	LiveRatioTooLow Reason = "live-ratio-too-low"
	PackProtected   Reason = "pack-protected"
	PackRetained    Reason = "pack-retained"
)

// EvaluateTiering is shared by runtime/model consumers; it never changes pack
// class/state, retires resources or promises cost savings. Revalidate under R1
// and CAS before execution. This W04 selection does not enable a lifecycle job.
func EvaluateTiering(in TieringInput) (TieringDecision, error) {
	d, ok, err := base(in.Policy, in.Request)
	out := TieringDecision{Decision: d}
	if !ok {
		return out, err
	}
	d, ok, err = facts(d, in.Metadata, in.Authority, in.SafeToTransition)
	out.Decision = d
	if !ok {
		return out, err
	}
	p := in.Pack
	if !validPack(p) || in.TotalBytes <= 0 || in.TotalBytes > p.Size || in.LiveBytes < 0 || in.LiveBytes > in.TotalBytes {
		out.Reason = InvalidEvidence
		return out, ErrEvidence
	}
	t := in.Policy.Snapshot.Policy().Tiering
	if !t.Enabled {
		out.Verdict, out.Reason = Denied, TieringDisabled
		return out, nil
	}
	if p.State != "Warm" || p.Class != storage.Standard && p.Class != storage.StandardIA {
		out.Verdict, out.Reason = Denied, Unavailable
		return out, nil
	}
	if !in.UTC.validated || !in.UTC.Trusted {
		out.Reason = MissingEvidence
		return out, nil
	}
	if err := validateUTCBinding(in.UTC, in.Request); err != nil {
		out.Reason = InvalidEvidence
		return out, err
	}
	cmp, err := in.UTC.Horizon.Compare(in.Request.Deadline)
	if err != nil {
		out.Reason = InvalidEvidence
		return out, err
	}
	if cmp < 0 {
		out.Reason = MissingEvidence
		return out, nil
	}
	// Use the lower bound: uncertain time must never make an old timestamp
	// expire early. A zero protection observation is missing, not expired.
	now := in.UTC.Lower.UTC()
	if !validUTC(p.ProtectedUntil) {
		out.Reason = MissingEvidence
		return out, nil
	}
	if now.Before(p.ProtectedUntil.UTC()) {
		out.Verdict, out.Reason = Denied, PackProtected
		return out, nil
	}
	if p.Class == storage.StandardIA && !validUTC(p.MinRetentionUntil) {
		out.Reason = MissingEvidence
		return out, nil
	}
	if !p.MinRetentionUntil.IsZero() {
		if !validUTC(p.MinRetentionUntil) {
			out.Reason = InvalidEvidence
			return out, ErrEvidence
		}
		if now.Before(p.MinRetentionUntil.UTC()) {
			out.Verdict, out.Reason = Denied, PackRetained
			return out, nil
		}
	}
	if t.ColdAfterDays != 0 {
		if !validUTC(p.LastAccess) {
			out.Reason = MissingEvidence
			return out, nil
		}
		eligibleAt := p.LastAccess.UTC().Add(time.Duration(t.ColdAfterDays) * 24 * time.Hour)
		if !validUTC(eligibleAt) {
			out.Reason = InvalidEvidence
			return out, ErrEvidence
		}
		if now.Before(eligibleAt) {
			out.Verdict, out.Reason = Denied, PackTooYoung
			return out, nil
		}
	}
	// Exact cross multiplication; products may exceed int64. Decimal has
	// already passed snapshot validation, so conversion is resource-bounded.
	coefficient, exponent := t.MinPackLiveRatio.Parts()
	limits := config.DSGPolicyLimits()
	if len(coefficient) > limits.MaxDecimalDigits || exponent < -limits.MaxDecimalExponent || exponent > limits.MaxDecimalExponent {
		out.Reason = InvalidEvidence
		return out, ErrEvidence
	}
	threshold, ok := new(big.Rat).SetString(t.MinPackLiveRatio.String())
	if !ok {
		out.Reason = InvalidEvidence
		return out, ErrEvidence
	}
	live := new(big.Int).Mul(big.NewInt(in.LiveBytes), threshold.Denom())
	required := new(big.Int).Mul(big.NewInt(in.TotalBytes), threshold.Num())
	if live.Cmp(required) < 0 {
		out.Verdict, out.Reason = Denied, LiveRatioTooLow
		return out, nil
	}
	out.Verdict, out.Reason, out.Destination = Eligible, Ready, t.ColdStorageClass
	return out, nil
}
