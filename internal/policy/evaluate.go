package policy

import "github.com/KinasTomes/miles/internal/storage"

func base(p PinnedPolicy, r Request) (Decision, bool, error) {
	d := Decision{Verdict: Indeterminate, PolicyRevision: p.RequiredRevision}
	if p.RequiredRevision == 0 || p.RequiredRevision > storage.MaxPolicyRevision {
		d.Reason = InvalidEvidence
		return d, false, ErrEvidence
	}
	if p.Snapshot.Revision() == 0 {
		d.Reason = MissingPolicy
		return d, false, nil
	}
	if p.Snapshot.Revision() != p.RequiredRevision {
		d.Reason = RevisionMismatch
		return d, false, nil
	}
	if r.Cancelled {
		d.Verdict = Denied
		d.Reason = Cancelled
		return d, false, nil
	}
	cmp, err := r.Now.Compare(r.Deadline)
	if err != nil {
		d.Reason = InvalidEvidence
		return d, false, err
	}
	if cmp >= 0 {
		d.Verdict = Denied
		d.Reason = Expired
		return d, false, nil
	}
	return d, true, nil
}

func facts(d Decision, fs ...Fact) (Decision, bool, error) {
	for _, f := range fs {
		if f > Invalid {
			d.Reason = InvalidEvidence
			return d, false, ErrEvidence
		}
	}
	for _, f := range fs {
		if f == Invalid {
			d.Verdict = Denied
			d.Reason = NotPermitted
			return d, false, nil
		}
	}
	for _, f := range fs {
		if f == Unknown {
			d.Reason = MissingEvidence
			return d, false, nil
		}
	}
	return d, true, nil
}

func validPack(p storage.PackInfo) bool {
	return p.Version == storage.ModelVersion && p.Ref.Validate() == nil && p.Size > 0 && p.StateRevision > 0 && p.Class.Validate() == nil
}
