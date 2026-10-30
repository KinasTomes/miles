package policy

import (
	"github.com/KinasTomes/miles/internal/storage"
	"sort"
	"time"
)

// EvaluateWrite implements Engine §5.2 for existing canonical candidates. It
// never restores the source and never creates protection deadlines or roots.
// Revival means writing verified client bytes to a NEW Warm generation followed
// by fenced location CAS, not modifying the supplied physical generation.
func EvaluateWrite(in WriteInput) (Decision, error) {
	d, ok, err := base(in.Policy, in.Request)
	if !ok {
		return d, err
	}
	d, ok, err = facts(d, in.Authority, in.Metadata)
	if !ok {
		return d, err
	}
	if in.Operation != PUT && in.Operation != UploadPart || in.ReusePins > Invalid {
		d.Reason = InvalidEvidence
		return d, ErrEvidence
	}
	unique := make(map[storage.PackRef]storage.PackInfo, len(in.Packs))
	for _, p := range in.Packs {
		if !validPack(p) {
			d.Reason = InvalidEvidence
			return d, ErrEvidence
		}
		if prev, exists := unique[p.Ref]; exists && !samePack(prev, p) {
			d.Reason = InvalidEvidence
			return d, ErrEvidence
		}
		unique[p.Ref] = p
	}
	actions := make([]Action, 0, len(unique))
	refs := make([]storage.PackRef, 0, len(unique))
	for ref := range unique {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Generation < refs[j].Generation
	})
	for _, ref := range refs {
		p := unique[ref]
		kind := ReviveFromBody
		reusable, err := WriteReuseCandidate(p.State, p.Class)
		if err != nil {
			d.Reason = InvalidEvidence
			return d, err
		}
		if reusable {
			if in.ReusePins == Unknown {
				d.Reason = MissingEvidence
				return d, nil
			}
			if in.ReusePins == Valid {
				kind = Reuse
			}
		}
		actions = append(actions, Action{Pack: p.Ref, StateRevision: p.StateRevision, Kind: kind})
	}
	sort.Slice(actions, func(i, j int) bool {
		a, b := actions[i].Pack, actions[j].Pack
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Generation < b.Generation
	})
	d.Verdict = Eligible
	d.Reason = Ready
	d.Actions = actions
	return d, nil
}

// WriteReuseCandidate is the shared state/class classification used by the
// evaluator and write engine. True only permits attempting an exact-generation
// reuse pin; it grants no authority and validates no identity/revision. Every
// Restored archive chunk is revived from client body even with a long expiry.
func WriteReuseCandidate(state string, class storage.StorageClass) (bool, error) {
	switch state {
	case "Warm":
		if class == storage.Standard || class == storage.StandardIA {
			return true, nil
		}
	case "Cold":
		if class == storage.GlacierIR {
			return true, nil
		}
		if class == storage.Glacier {
			return false, nil
		}
	case "Restored", "Restoring":
		if class == storage.Glacier {
			return false, nil
		}
	case "Uploading", "Transitioning", "Compacting", "Retiring", "Retired", "Deleting", "Deleted":
		return false, nil
	}
	return false, ErrEvidence
}

// Compare all observations without Go time.Time == (location/monotonic payload
// is not UTC identity). Duplicate contradictions fail closed, never last-wins.
func samePack(a, b storage.PackInfo) bool {
	return a.Version == b.Version && a.Ref == b.Ref && a.State == b.State && a.Class == b.Class && a.Size == b.Size && a.StateRevision == b.StateRevision && equalTime(a.RestoredUntil, b.RestoredUntil) && equalTime(a.ProtectedUntil, b.ProtectedUntil) && equalTime(a.MinRetentionUntil, b.MinRetentionUntil) && equalTime(a.LastAccess, b.LastAccess)
}
func equalTime(a, b time.Time) bool { return a.UTC().Equal(b.UTC()) }
