package storage

import (
	"context"
	"time"
)

// PackProtectionStore is the existing backend's optional atomic reuse extension.
// It condition-checks the exact canonical binding, generation/state revision and
// writer authority, then persists max(current, RequiredUntil), never a decrease.
// Caller must retain the R1 generation pin through publication/quiescence. This
// method neither acquires that pin nor authorizes physical maintenance.
// Monotonic protection does not change lifecycle StateRevision. Native adapters
// keep an independent protection journal revision. A same-mutation retry may
// return its retained committed outcome, which is not fresh execution authority.
type PackProtectionStore interface {
	ExtendPackProtection(context.Context, PackProtectionUpdate) (PackInfo, error)
}

type PackProtectionUpdate struct {
	Chunk                 Chunk
	ExpectedStateRevision uint64
	RequiredUntil         time.Time // policy-evaluated conservative UTC deadline
	Writer                WriterFence
}

func (r PackProtectionUpdate) Validate() error {
	if r.Chunk.Validate() != nil || r.ExpectedStateRevision == 0 || r.ExpectedStateRevision > MaxPolicyRevision || r.RequiredUntil.IsZero() || r.RequiredUntil.UTC().Year() < 1 || r.RequiredUntil.UTC().Year() > 9999 {
		return ErrInvalid
	}
	return nil
}
