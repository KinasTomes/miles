package policy

import (
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

// RecoveryEpoch is a PROPOSAL for pure input typing only, not a shared model,
// stored schema, coordinator or R1 fencing counter. Never compare it to Epoch
// in storage.WriterFence. Its presence grants no execution authority.
type RecoveryEpoch uint64

// RecoveryProtectionContext is optional for ordinary writer protection. When
// present, its independent registration/binding must have been validated by the
// R2 consumer. This evaluator cannot establish membership, closure or ownership.
type RecoveryProtectionContext struct {
	HistoryID          string
	Epoch              RecoveryEpoch
	ServiceIncarnation string
	Binding            Fact
}

// ProtectionInput is a consumer-review PROPOSAL for writer protection planning
// (new Uploading generation or admitted reuse). It is not a dispatch token.
// UTC has the same explicit adapter proof preconditions as EvaluateRead.
type ProtectionInput struct {
	Policy                          PinnedPolicy
	Request                         Request
	UTC                             UTCProof
	Pack                            storage.PackInfo
	OperationID, ServiceIncarnation string
	Writer                          storage.WriterFence
	// Binding covers operation ID, exact PackRef/state revision, current observed
	// protection, request deadline, policy and incarnation. Authority includes
	// independently verified R1 admission/lease/fence and resource-gate checks.
	// Neither a syntactically valid handle nor Trusted=true establishes proof.
	Binding, Authority Fact
	Recovery           *RecoveryProtectionContext
	// Informational only: root presence/absence/unknown never grants deletion,
	// changes the computed minimum or releases roots. Caller preserves all roots.
	RetentionRoots Fact
	// Explicit backend tick, anchored at Go's year-1 UTC zero coordinate (the
	// time.Time.Truncate lattice). Adapter must confirm this matches its backend.
	// No production precision default is chosen. Positive durations only.
	Precision time.Duration
}

type ProtectionDecision struct {
	Verdict        Verdict
	Reason         Reason
	PolicyRevision uint64
	Pack           storage.PackRef
	StateRevision  uint64
	// Nonzero only on Eligible. Minimum is rounded D_hi+15m. Target additionally
	// preserves/rounds up the observed old protection; it must never replace an
	// independently extended current value with a shorter stale proposal.
	Minimum, Target   time.Time
	ExtensionRequired bool
}

// EvaluateProtection implements Clock §3.5 and Engine §5.3. Eligible means a
// proposal can be handed to the executor, NOT that protection is applied.
// Caller must revalidate proofs/authority/incarnation, preserve retention roots,
// atomically persist max(current,Target) under the appropriate fence/R2 scope,
// verify backend accepted coverage, then publish/reuse. Unknown persistence
// outcomes require reconciliation. This API neither decides GC eligibility nor
// changes Q6: valid pre-expiry dispatch may settle before shared fence closure.
func EvaluateProtection(in ProtectionInput) (ProtectionDecision, error) {
	b, ok, err := base(in.Policy, in.Request)
	d := ProtectionDecision{Verdict: b.Verdict, Reason: b.Reason, PolicyRevision: b.PolicyRevision, Pack: in.Pack.Ref, StateRevision: in.Pack.StateRevision}
	if !ok {
		return d, err
	}
	b, ok, err = facts(b, in.Binding, in.Authority)
	d.Verdict, d.Reason = b.Verdict, b.Reason
	if !ok {
		return d, err
	}
	invalid := func() (ProtectionDecision, error) { d.Reason = InvalidEvidence; return d, ErrEvidence }
	if in.OperationID == "" || in.ServiceIncarnation == "" || in.Writer.LeaseID == "" || in.Writer.Epoch == 0 || in.Writer.ServiceIncarnation != in.ServiceIncarnation || in.Pack.Version != storage.ModelVersion || in.Pack.Ref.Validate() != nil || in.Pack.StateRevision == 0 || in.Precision < 0 || in.RetentionRoots > Invalid {
		return invalid()
	}
	if in.Recovery != nil {
		r := in.Recovery
		b, ok, err = facts(b, r.Binding)
		d.Verdict, d.Reason = b.Verdict, b.Reason
		if !ok {
			return d, err
		}
		if r.HistoryID == "" || r.Epoch == 0 || r.ServiceIncarnation != in.ServiceIncarnation {
			return invalid()
		}
	}
	if in.Precision == 0 || !in.UTC.Trusted {
		d.Reason = MissingEvidence
		return d, nil
	}
	u := in.UTC
	if !validUTC(u.Lower) || !validUTC(u.Upper) || u.Lower.After(u.Upper) || (!in.Pack.ProtectedUntil.IsZero() && !validUTC(in.Pack.ProtectedUntil)) {
		return invalid()
	}
	cmp, err := u.Horizon.Compare(in.Request.Deadline)
	if err != nil {
		d.Reason = InvalidEvidence
		return d, err
	}
	if cmp < 0 {
		d.Reason = MissingEvidence
		return d, nil
	}
	remaining, err := in.Request.Deadline.Sub(in.Request.Now)
	if err != nil {
		d.Reason = InvalidEvidence
		return d, err
	}
	raw := u.Upper.UTC().Add(remaining).Add(15 * time.Minute)
	if !validUTC(raw) {
		return invalid()
	}
	minimum, err := roundProtectionUp(raw, in.Precision)
	if err != nil {
		return invalid()
	}
	target := minimum
	old := in.Pack.ProtectedUntil.UTC()
	if old.After(target) {
		target = old
	}
	target, err = roundProtectionUp(target, in.Precision)
	if err != nil {
		return invalid()
	}
	d.Verdict, d.Reason = Eligible, Ready
	d.Minimum, d.Target = minimum, target
	d.ExtensionRequired = old.IsZero() || old.Before(target)
	return d, nil
}

func roundProtectionUp(t time.Time, precision time.Duration) (time.Time, error) {
	if precision <= 0 || !validUTC(t) {
		return time.Time{}, ErrEvidence
	}
	rounded := t.UTC().Truncate(precision)
	if rounded.Before(t) {
		rounded = rounded.Add(precision)
	}
	if !validUTC(rounded) || rounded.Before(t) {
		return time.Time{}, ErrEvidence
	}
	return rounded, nil
}
