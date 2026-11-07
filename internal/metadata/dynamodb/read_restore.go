package dynamodb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
)

// QueueReadRestore is an internal reader capability, not an Admin impersonation.
// Enqueue and read lease/root/pin/policy validation share one native transaction.
// It grants no readability and never performs physical I/O. Admission auth was
// pinned by the reader; grant revocation does not reauthorize an admitted GET.
func (m *Store) QueueReadRestore(ctx context.Context, reader s.WriterFence, head s.ObjectHead, ref s.PackRef, revision uint64, required time.Time) (out jobs.Job, err error) {
	if head.Object.Validate() != nil || head.Revision == 0 || head.ManifestID == "" || ref.Validate() != nil || revision == 0 || required.IsZero() || required.UTC().Year() < 1 || required.UTC().Year() > 9999 {
		return out, s.ErrInvalid
	}
	err = m.update(ctx, func(t *transaction) error {
		lease, e := m.r1Check(t, reader)
		if e != nil {
			return e
		}
		if lease.Kind != "reader" || lease.Resolving || lease.Object != head.Object || lease.Root != head.ManifestID || lease.RootRevision != head.Revision {
			return s.ErrFenced
		}
		bucket, e := get[s.Bucket](t, key("buckets", head.Object.Bucket))
		if e != nil {
			return e
		}
		if !bucket.Active || bucket.ID != head.Object.BucketID {
			return s.ErrFenced
		}
		pin, e := r1Read[r1PinEntry](t, r1PinEntryKey(reader, ref))
		if e != nil {
			return e
		}
		if pin.Fence != reader || pin.Pack != ref || pin.Released {
			return s.ErrFenced
		}
		p, e := get[s.PackInfo](t, packKey(ref))
		if e != nil {
			return e
		}
		if p.Class != s.Glacier || p.State != "Cold" && p.State != "Restoring" && p.State != "Restored" {
			return s.ErrConflict
		}
		b, e := m.r1Bounds(ctx)
		if e != nil {
			return e
		}
		if !required.After(b.Upper) {
			return s.ErrInvalid
		}
		// Preserve the earlier admitted deadline if a later UTC sample narrows.
		if floor := lease.Deadline.Add(120 * time.Second); required.Before(floor) {
			if floor.UTC().Year() > 9999 {
				return s.ErrInvalid
			}
			required = floor
		}
		record, e := get[s.PolicySnapshot](t, policyKey(revision))
		if e != nil {
			return e
		}
		v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
		policy, e := v.DecodeGlobal(record)
		if e != nil {
			return e
		}
		if !policy.Policy().Tiering.AutoRestore {
			return s.ErrAccessDenied
		}
		if p.State == "Restoring" {
			owner, e := r1Read[lifeRestoreOwner](t, lifecycleRestoreKey(ref))
			if e != nil {
				return e
			}
			out, e = get[jobs.Job](t, key("jobs", owner.ID))
			return e // acceptance already exists, never re-send it from GET
		}
		settings := policy.Policy().Tiering
		// State revision changes after completed restore. Concurrent reads of one
		// generation share a job; a completed/expired generation gets a new ID.
		identity, _ := json.Marshal(struct {
			Ref           s.PackRef
			State, Policy uint64
		}{ref, p.StateRevision, revision})
		sum := sha256.Sum256(identity)
		id := "restore_" + hex.EncodeToString(sum[:16])
		old, e := get[jobs.Job](t, key("jobs", id))
		if e == nil {
			if old.Type != "pack_restore" || old.PolicyRevision != revision {
				return s.ErrConflict
			}
			out = old
			if old.State.Terminal() {
				return jobs.ErrReconciliation
			}
			return nil // no hidden retry after a failed/unknown job
		}
		if !errors.Is(e, s.ErrNotFound) {
			return e
		}
		params, _ := json.Marshal(lifecycle.Params{PackID: ref.ID, Generation: ref.Generation, Days: int(settings.AutoRestoreDays), Tier: settings.AutoRestoreTier, RequiredUntil: required.UTC()})
		out = jobs.Job{ID: id, Type: "pack_restore", State: jobs.Queued, Params: params, PolicyRevision: revision, Revision: 1, CreatedAt: b.Upper, Progress: map[string]uint64{}}
		if e := out.Validate(); e != nil {
			return e
		}
		if _, e = m.r1Check(t, reader); e != nil {
			return e
		} // fresh dispatch permission after slow reads
		return put(t, key("jobs", id), out)
	})
	return
}
