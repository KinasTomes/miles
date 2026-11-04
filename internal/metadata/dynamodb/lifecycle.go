package dynamodb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	format "github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
	"slices"
	"time"
)

func lifecycleKey(id string) Key {
	return stringKey("SystemConfig", "config_id", "lifecycle/action/"+id)
}
func (m *Store) GetLifecycle(ctx context.Context, id string) (lifecycle.Action, error) {
	a, e := readLifecycle(m, ctx, id)
	if e == nil && (a.Validate() != nil || a.ID != id) {
		e = s.ErrUnavailable
	}
	return a, e
}

// Job, node and gate observations join the SAME publication transaction.
func (m *Store) lifecycleJob(t *transaction, j jobs.Job, dispatch bool) error {
	old, e := get[jobs.Job](t, key("jobs", j.ID))
	if e != nil {
		return e
	}
	if old.Worker != j.Worker || old.PendingStep != j.PendingStep || old.WorkerNodeID != j.WorkerNodeID || old.WorkerNodeIncarnation != j.WorkerNodeIncarnation || old.State != jobs.Running && old.State != jobs.Cancelling {
		return s.ErrFenced
	}
	n, e := get[jobs.Node](t, key("nodes", j.WorkerNodeID))
	if e != nil {
		return e
	}
	if n.Incarnation != j.WorkerNodeIncarnation || n.ServiceIncarnation != j.Worker.ServiceIncarnation {
		return s.ErrFenced
	}
	g, e := r1Read[s.R1Gate](t, r1GateKey())
	if e != nil {
		return e
	}
	if g.State != "Open" {
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil || a.Kind != "tiering" || a.Fence != j.Worker || g.State != "Maintenance" || a.GateRevision != g.Revision {
			return s.ErrFenced
		}
	}
	if dispatch {
		b, e := m.r1Bounds(t.ctx)
		if e != nil {
			return e
		}
		if !b.Upper.Before(n.HeartbeatAt.Add(jobs.HeartbeatMaxAge)) || n.HeartbeatAt.After(b.Upper.Add(2*time.Second)) {
			return s.ErrFenced
		}
	}
	if dispatch && (old.State != jobs.Running || !n.Ready && g.State != "Maintenance" || n.Draining) {
		return s.ErrFenced
	}
	return nil
}
func (m *Store) CheckLifecycle(ctx context.Context, j jobs.Job) error {
	return m.view(ctx, func(t *transaction) error {
		if e := m.lifecycleJob(t, j, true); e != nil {
			return e
		}
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if a.Fence != j.Worker || a.State != "Dispatched" && a.State != "Unknown" {
			return s.ErrFenced
		}
		// Last, after slow records: a new physical send cannot use stale UTC.
		return m.fence(t, j.Worker)
	})
}
func (m *Store) LifecycleProducer(ctx context.Context, j jobs.Job) (s.MutationContext, error) {
	l, e := m.CheckR1(ctx, j.Worker)
	if e != nil {
		return s.MutationContext{}, e
	}
	c := s.MutationContext{Writer: j.Worker, HistoryID: l.HistoryID, Epoch: l.RecoveryEpoch, LogicalOpID: l.OpID, MutationID: l.OpID}
	return c, c.Validate()
}
func (m *Store) PrepareLifecycle(ctx context.Context, j jobs.Job, p lifecycle.Params, plan []s.Chunk, digest string) (out lifecycle.Action, err error) {
	if p.Validate(j.Type) != nil || len(plan) > 16384 {
		return out, s.ErrInvalid
	}
	planHash := ""
	var pageCount uint32
	if lifecycle.Repack(j.Type) {
		planBody, _ := json.Marshal(plan)
		sum := sha256.Sum256(planBody)
		planHash = hex.EncodeToString(sum[:])
		pageCount = uint32((len(plan) + 31) / 32)
		if _, e := m.GetLifecycle(ctx, j.ID); errors.Is(e, s.ErrNotFound) {
			for number := uint32(0); number < pageCount; number++ {
				begin := int(number) * 32
				end := min(begin+32, len(plan))
				page := lifePlanPage{Version: 1, Root: planHash, Chunks: plan[begin:end]}
				if e = m.update(ctx, func(t *transaction) error {
					if e := m.fence(t, j.Worker); e != nil {
						return e
					}
					if e := m.lifecycleJob(t, j, true); e != nil {
						return e
					}
					k := lifecyclePlanKey(j.ID, number)
					old, e := r1Read[lifePlanPage](t, k)
					if e == nil {
						a, _ := json.Marshal(old)
						b, _ := json.Marshal(page)
						if !bytes.Equal(a, b) {
							return s.ErrConflict
						}
						return nil
					}
					if !errors.Is(e, s.ErrNotFound) {
						return e
					}
					return r1Put(t, k, page)
				}); e != nil {
					return out, e
				}
			}
		} else if e != nil {
			return out, e
		}
	}
	err = m.update(ctx, func(t *transaction) error {
		if e := m.fence(t, j.Worker); e != nil {
			return e
		}
		if e := m.lifecycleJob(t, j, false); e != nil {
			return e
		}
		old, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e == nil {
			if old.Kind != j.Type || old.Source.Ref != p.Ref() || old.Days != p.Days || old.Tier != p.Tier || lifecycle.Repack(old.Kind) && (old.PlanSHA256 != planHash || old.Digest != digest) {
				return s.ErrConflict
			}
			if old.Fence != j.Worker {
				if old.State == "Done" || old.State == "Skipped" || old.State == "NotApplied" {
					out = old
					return nil
				}
				if old.State != "Waiting" && old.State != "Prepared" {
					return s.ErrFenced
				}
				old.Fence = j.Worker
				old.Revision++
				if e = r1Put(t, lifecycleKey(j.ID), old); e != nil {
					return e
				}
			}
			out = old
			return nil
		}
		if !errors.Is(e, s.ErrNotFound) {
			return e
		}
		if e := m.lifecycleJob(t, j, true); e != nil {
			return e
		}
		source, e := get[s.PackInfo](t, packKey(p.Ref()))
		if e != nil {
			return e
		}
		if source.StateRevision == 0 || source.StateRevision >= s.MaxPolicyRevision-3 {
			return s.ErrInvalid
		}
		b, e := m.r1Bounds(ctx)
		if e != nil {
			return e
		}
		a := lifecycle.Action{Version: 1, ID: j.ID, JobID: j.ID, Kind: j.Type, State: "Prepared", Revision: 1, Fence: j.Worker, Source: source, Days: p.Days, Tier: p.Tier, PlanPages: pageCount, PlanSHA256: planHash, Digest: digest}
		a.Destination = lifecycle.Destination(j.Type, j.ID, source.Ref)
		switch j.Type {
		case "tiering":
			g, e := r1Read[s.R1Gate](t, r1GateKey())
			if e != nil {
				return e
			}
			if g.State != "Open" || g.Active != 1 || g.Resolving > 1 {
				return s.ErrR1Busy
			}
			pins, e := r1Read[r1Pins](t, r1PinKey(source.Ref))
			if e != nil && !errors.Is(e, s.ErrNotFound) {
				return e
			}
			if pins.Count != 0 {
				return s.ErrR1Busy
			}
			if source.State != "Warm" || source.ProtectedUntil.IsZero() || b.Lower.Before(source.ProtectedUntil) || !source.MinRetentionUntil.IsZero() && b.Lower.Before(source.MinRetentionUntil) {
				return s.ErrR1Busy
			}
			g.State = "Maintenance"
			g.Revision++
			a.GateRevision = g.Revision
			if e = r1Put(t, r1GateKey(), g); e != nil {
				return e
			}
		case "pack_restore":
			if source.Class == s.Glacier && source.State == "Restoring" {
				owner, e := r1Read[lifeRestoreOwner](t, lifecycleRestoreKey(source.Ref))
				if e != nil {
					return e
				}
				previous, e := r1Read[lifecycle.Action](t, lifecycleKey(owner.ID))
				if e != nil {
					return e
				}
				previousJob, e := get[jobs.Job](t, key("jobs", owner.ID))
				if e != nil {
					return e
				}
				if previous.State != "Waiting" || !previousJob.State.Terminal() || previous.Days != p.Days || previous.Tier != p.Tier {
					return s.ErrR1Busy
				}
				a.Source = previous.Source
				a.State = "Waiting"
			} else if source.Class == s.GlacierIR && source.State == "Cold" {
				a.State = "Skipped"
			} else if source.Class != s.Glacier || source.State != "Cold" && !(source.State == "Restored" && !b.Upper.Before(source.RestoredUntil)) {
				return s.ErrConflict
			}
		case "pack_promote", "compaction":
			if a.Kind == "compaction" && source.State != "Warm" {
				return s.ErrConflict
			}
			if !lifecycle.Readable(source, b.Upper) || len(plan) == 0 || len(digest) != 64 {
				return s.ErrConflict
			}
			pin, e := r1Read[r1PinEntry](t, r1PinEntryKey(j.Worker, source.Ref))
			if e != nil {
				return e
			}
			if pin.Fence != j.Worker || pin.Pack != source.Ref || pin.Released {
				return s.ErrFenced
			}
			seen := map[s.ChunkKey]bool{}
			for _, chunk := range plan {
				if chunk.Validate() != nil || chunk.Location.Pack != source.Ref || chunk.Location.Range.Validate(source.Size) != nil || seen[chunk.Key] {
					return s.ErrInvalid
				}
				seen[chunk.Key] = true
			}
		default:
			return s.ErrInvalid
		}
		if a.Kind == "pack_restore" && a.State != "Skipped" {
			if e := r1Put(t, lifecycleRestoreKey(a.Source.Ref), lifeRestoreOwner{Version: 1, ID: a.ID}); e != nil {
				return e
			}
		}
		out = a
		return r1Put(t, lifecycleKey(j.ID), a)
	})
	if err == nil {
		out, err = m.GetLifecycle(ctx, j.ID)
	}
	return
}

// Dispatch records the complete intent and pending protection before SDK I/O.
func (m *Store) DispatchLifecycle(ctx context.Context, j jobs.Job, target s.StorageClass, live int64) (out lifecycle.Action, err error) {
	err = m.update(ctx, func(t *transaction) error {
		if e := m.fence(t, j.Worker); e != nil {
			return e
		}
		if e := m.lifecycleJob(t, j, true); e != nil {
			return e
		}
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if a.Fence != j.Worker {
			return s.ErrFenced
		}
		if a.State != "Prepared" {
			out = a
			return nil
		}
		p, e := get[s.PackInfo](t, packKey(a.Source.Ref))
		if e != nil {
			return e
		}
		if p.StateRevision != a.Source.StateRevision || p.Class != a.Source.Class {
			return s.ErrConflict
		}
		if a.Kind == "tiering" {
			policyRecord, e := get[s.PolicySnapshot](t, policyKey(j.PolicyRevision))
			if e != nil {
				return e
			}
			b, e := m.r1Bounds(ctx)
			if e != nil {
				return e
			}
			eligible, e := lifecycle.TieringCandidate(policyRecord, p, live, b)
			if e != nil {
				return e
			}
			if eligible != target || target == "" {
				return s.ErrConflict
			}
			if e = p.Class.ValidateTransition(target); e != nil {
				return e
			}
			a.Target = target
			a.LiveBytes = live
			p.State = "Transitioning"
			p.StateRevision++
		} else if a.Kind == "pack_restore" {
			p.State = "Restoring"
			p.StateRevision++
		}
		l, e := r1Read[s.R1Lease](t, r1LeaseKey(j.ID))
		if e != nil {
			return e
		}
		pending := "lifecycle/" + j.ID
		if len(l.Pending) >= 32 {
			return s.ErrR1Busy
		}
		if !slices.Contains(l.Pending, pending) {
			l.Pending = append(l.Pending, pending)
			l.Revision++
		}
		a.State = "Ready"
		a.Revision++
		out = a
		if e = r1Put(t, r1LeaseKey(j.ID), l); e != nil {
			return e
		}
		return m.lifecycleRecord(t, j, a, p)
	})
	return
}
func (m *Store) lifecycleRecord(t *transaction, j jobs.Job, a lifecycle.Action, p s.PackInfo) error {
	l, e := r1Read[s.R1Lease](t, r1LeaseKey(j.ID))
	if e != nil {
		return e
	}
	c := s.MutationContext{Writer: j.Worker, HistoryID: l.HistoryID, Epoch: l.RecoveryEpoch, LogicalOpID: l.OpID, MutationID: l.OpID}
	c = s.ChildMutation(c, "PackLifecycle", j.ID, a.State)
	if c.Validate() != nil {
		return s.ErrFenced
	}
	identity := p.Ref.ID + "/" + p.Ref.Generation
	state, e := operations.Projection("Packs", identity, p.StateRevision, operations.PackStateProjection(p), false)
	if e != nil {
		return e
	}
	facts, e := operations.Projection("PackLifecycleFacts", identity, p.StateRevision, struct {
		Version                          uint32
		Ref                              s.PackRef
		RestoredUntil, MinRetentionUntil time.Time
	}{1, p.Ref, p.RestoredUntil, p.MinRetentionUntil}, false)
	if e != nil {
		return e
	}
	intent, e := operations.Projection("PackLifecycle", j.ID, a.Revision, a, false)
	if e != nil {
		return e
	}
	body, _ := json.Marshal(a)
	if _, e = operations.Produce(multipartTx{t}, c, "PackLifecycle", "entity_put", j.ID, a.Revision, body, []format.Effect{state, facts, intent}, a); e != nil {
		return e
	}
	if e = put(t, packKey(p.Ref), p); e != nil {
		return e
	}
	return r1Put(t, lifecycleKey(a.ID), a)
}
func (m *Store) UnknownLifecycle(ctx context.Context, j jobs.Job) error {
	return m.update(ctx, func(t *transaction) error {
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if a.Fence != j.Worker {
			return s.ErrFenced
		}
		if a.State == "Dispatched" {
			a.State = "Unknown"
			a.Revision++
			return r1Put(t, lifecycleKey(j.ID), a)
		}
		return nil
	})
}

// Waiting is a known accepted asynchronous restore, not a terminal restore.
// Unknown transitions are intentionally NOT settled from HEAD existence/class.
func (m *Store) FinishLifecycle(ctx context.Context, j jobs.Job, physical s.PackInfo, waiting bool) (out lifecycle.Action, err error) {
	err = m.update(ctx, func(t *transaction) error {
		if e := m.fence(t, j.Worker); e != nil {
			return e
		}
		if e := m.lifecycleJob(t, j, false); e != nil {
			return e
		}
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if a.Fence != j.Worker {
			return s.ErrFenced
		}
		if a.State == "Done" || a.State == "Skipped" || a.State == "NotApplied" {
			out = a
			return nil
		}
		p, e := get[s.PackInfo](t, packKey(a.Source.Ref))
		if e != nil {
			return e
		}
		b, e := m.r1Bounds(ctx)
		if e != nil {
			return e
		}
		switch a.Kind {
		case "tiering":
			if a.State != "Applied" || physical.Ref != p.Ref || (a.KnownApplied && physical.Class != a.Target || !a.KnownApplied && physical.Class != a.Source.Class) || physical.Size != p.Size || p.State != "Transitioning" || p.StateRevision != a.Source.StateRevision+1 {
				return s.ErrConflict
			}
			if a.KnownApplied {
				p.State = "Cold"
				p.Class = a.Target
				p.MinRetentionUntil = b.Upper.Add(90 * 24 * time.Hour)
			} else {
				p.State = a.Source.State
				p.Class = a.Source.Class
				a.State = "NotApplied"
			}
			p.StateRevision++
			g, e := r1Read[s.R1Gate](t, r1GateKey())
			if e != nil {
				return e
			}
			if g.State != "Maintenance" || g.Revision != a.GateRevision {
				return s.ErrFenced
			}
			g.State = "Open"
			g.Revision++
			if e = r1Put(t, r1GateKey(), g); e != nil {
				return e
			}
		case "pack_restore":
			if p.State != "Restoring" || p.StateRevision != a.Source.StateRevision+1 {
				return s.ErrConflict
			}
			if physical.Ref != p.Ref || physical.Class != s.Glacier || physical.Size != p.Size {
				return s.ErrConflict
			}
			if waiting {
				if a.State == "Waiting" {
					out = a
					return nil
				}
				if a.State == "Unknown" && physical.State != "Restoring" {
					return s.ErrR1Busy
				}
				a.State = "Waiting"
			} else {
				if !b.Upper.Before(physical.RestoredUntil) {
					return s.ErrR1Busy
				}
				p.State = "Restored"
				p.RestoredUntil = physical.RestoredUntil
				p.StateRevision++
			}
		case "pack_promote", "compaction":
			dest, e := get[s.PackInfo](t, packKey(a.Destination))
			if e != nil {
				return e
			}
			if dest.State != "Warm" || dest.Writer != j.Worker {
				return s.ErrConflict
			}
			a.ResultPack = dest
		default:
			return s.ErrInvalid
		}
		l, e := r1Read[s.R1Lease](t, r1LeaseKey(j.ID))
		if e != nil {
			return e
		}
		if at := slices.Index(l.Pending, "lifecycle/"+j.ID); at >= 0 {
			l.Pending = slices.Delete(l.Pending, at, at+1)
			l.Revision++
			if e = r1Put(t, r1LeaseKey(j.ID), l); e != nil {
				return e
			}
		}
		if !waiting && a.State != "NotApplied" {
			a.State = "Done"
		}
		a.Revision++
		out = a
		return m.lifecycleRecord(t, j, a, p)
	})
	return
}
func (m *Store) SkipLifecycle(ctx context.Context, j jobs.Job) (out lifecycle.Action, err error) {
	err = m.update(ctx, func(t *transaction) error {
		if e := m.fence(t, j.Worker); e != nil {
			return e
		}
		if e := m.lifecycleJob(t, j, false); e != nil {
			return e
		}
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if a.Fence != j.Worker {
			return s.ErrFenced
		}
		if a.State == "Skipped" {
			out = a
			return nil
		}
		if a.State != "Prepared" && a.State != "Ready" {
			return s.ErrFenced
		}
		registered := a.State == "Ready"
		if a.Kind == "tiering" {
			g, e := r1Read[s.R1Gate](t, r1GateKey())
			if e != nil {
				return e
			}
			if g.Revision != a.GateRevision || g.State != "Maintenance" {
				return s.ErrFenced
			}
			g.State = "Open"
			g.Revision++
			if e = r1Put(t, r1GateKey(), g); e != nil {
				return e
			}
		}
		a.State = "Skipped"
		a.Revision++
		out = a
		if !registered {
			return r1Put(t, lifecycleKey(j.ID), a)
		}
		p, e := get[s.PackInfo](t, packKey(a.Source.Ref))
		if e != nil {
			return e
		}
		if !lifecycle.Repack(a.Kind) {
			if p.StateRevision != a.Source.StateRevision+1 {
				return s.ErrConflict
			}
			p.State = a.Source.State
			p.StateRevision++
		}
		l, e := r1Read[s.R1Lease](t, r1LeaseKey(j.ID))
		if e != nil {
			return e
		}
		if at := slices.Index(l.Pending, "lifecycle/"+j.ID); at >= 0 {
			l.Pending = slices.Delete(l.Pending, at, at+1)
			l.Revision++
			if e = r1Put(t, r1LeaseKey(j.ID), l); e != nil {
				return e
			}
		}
		return m.lifecycleRecord(t, j, a, p)
	})
	return
}

// Applied records a known synchronous SDK acknowledgement, not an inferred HEAD
// outcome. Persisting this fact permits read-only verification on later ticks.
func (m *Store) AppliedLifecycle(ctx context.Context, j jobs.Job) error {
	return m.update(ctx, func(t *transaction) error {
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if a.Fence != j.Worker {
			return s.ErrFenced
		}
		if a.State == "Applied" {
			return nil
		}
		if a.State != "Dispatched" {
			return s.ErrConflict
		}
		a.State = "Applied"
		a.KnownApplied = true
		a.Revision++
		return r1Put(t, lifecycleKey(j.ID), a)
	})
}

type lifePlanPage struct {
	Version uint32
	Root    string
	Chunks  []s.Chunk
}

func lifecyclePlanKey(id string, n uint32) Key {
	return stringKey("SystemConfig", "config_id", fmt.Sprintf("lifecycle/plan/%s/%06d", id, n))
}
func readLifecycle(m *Store, ctx context.Context, id string) (out lifecycle.Action, err error) {
	err = m.view(ctx, func(t *transaction) error {
		var e error
		out, e = r1Read[lifecycle.Action](t, lifecycleKey(id))
		return e
	})
	if err != nil {
		return
	}
	if out.PlanPages > 512 {
		return out, s.ErrUnavailable
	}
	for n := uint32(0); n < out.PlanPages; n++ {
		var page lifePlanPage
		err = m.view(ctx, func(t *transaction) error {
			var e error
			page, e = r1Read[lifePlanPage](t, lifecyclePlanKey(id, n))
			return e
		})
		if err != nil {
			return
		}
		if page.Version != 1 || page.Root != out.PlanSHA256 || len(page.Chunks) == 0 || len(page.Chunks) > 32 {
			return out, s.ErrUnavailable
		}
		out.Chunks = append(out.Chunks, page.Chunks...)
	}
	if out.PlanPages > 0 {
		body, _ := json.Marshal(out.Chunks)
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != out.PlanSHA256 {
			return out, s.ErrUnavailable
		}
	}
	return
}

// LifecycleTerminalGuard belongs to a trusted reconciler. It must observe the
// exact original action terminal outcome AND isolation through Observe; HEAD,
// timeout, lease expiry or a caller-provided boolean is never such evidence.
type LifecycleTerminalGuard interface {
	Terminal(context.Context, lifecycle.Action, Observe) (bool, error)
}

func (m *Store) ResolveLifecycle(ctx context.Context, j jobs.Job) (out lifecycle.Action, err error) {
	err = m.update(ctx, func(t *transaction) error {
		if e := m.fence(t, j.Worker); e != nil {
			return e
		}
		if e := m.lifecycleJob(t, j, false); e != nil {
			return e
		}
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if a.Fence != j.Worker {
			return s.ErrFenced
		}
		if a.State == "Applied" {
			out = a
			return nil
		}
		if a.State != "Unknown" || a.Kind != "tiering" {
			return s.ErrConflict
		}
		authority, e := m.r1Authority()
		if e != nil {
			return e
		}
		if authority.LifecycleTerminal == nil {
			return jobs.ErrReconciliation
		}
		applied, e := authority.LifecycleTerminal.Terminal(ctx, a, r1Observe(t))
		if e != nil {
			return e
		}
		p, e := get[s.PackInfo](t, packKey(a.Source.Ref))
		if e != nil {
			return e
		}
		a.State = "Applied"
		a.KnownApplied = applied
		a.Revision++
		out = a
		return m.lifecycleRecord(t, j, a, p)
	})
	return
}

// ClaimLifecycleSend consumes one mutable-send permission. A lost claim ACK is
// NOT a grant on retry: Dispatched never grants a second mutable send.
func (m *Store) ClaimLifecycleSend(ctx context.Context, j jobs.Job) (out lifecycle.Action, fresh bool, err error) {
	err = m.update(ctx, func(t *transaction) error {
		if e := m.fence(t, j.Worker); e != nil {
			return e
		}
		if e := m.lifecycleJob(t, j, true); e != nil {
			return e
		}
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if a.Fence != j.Worker {
			return s.ErrFenced
		}
		if a.State != "Ready" {
			out = a
			return nil
		}
		a.State = "Dispatched"
		a.Revision++
		out = a
		fresh = true
		return r1Put(t, lifecycleKey(j.ID), a)
	})
	if err != nil {
		fresh = false
	}
	return
}

func lifecycleRestoreKey(p s.PackRef) Key {
	return stringKey("SystemConfig", "config_id", "lifecycle/restore/"+p.ID+"/"+p.Generation)
}

type lifeRestoreOwner struct {
	Version uint32
	ID      string
}

func (p lifePlanPage) Validate() error {
	if p.Version != 1 || len(p.Root) != 64 || len(p.Chunks) == 0 || len(p.Chunks) > 32 {
		return s.ErrInvalid
	}
	return nil
}
func (o lifeRestoreOwner) Validate() error {
	if o.Version != 1 || !jobs.ValidID(o.ID) {
		return s.ErrInvalid
	}
	return nil
}

// LifecycleQuiescent is consumed before job authority attempts expiry cleanup.
// A missing action is not proof that preflight I/O or an old step has joined.
func (m *Store) LifecycleQuiescent(ctx context.Context, id string, f s.WriterFence) error {
	return m.view(ctx, func(t *transaction) error {
		a, e := r1Read[lifecycle.Action](t, lifecycleKey(id))
		if errors.Is(e, s.ErrNotFound) {
			return jobs.ErrReconciliation
		}
		if e != nil {
			return e
		}
		if a.Fence != f || a.State != "Done" && a.State != "Skipped" && a.State != "NotApplied" && a.State != "Waiting" {
			return jobs.ErrReconciliation
		}
		lease, e := r1Read[s.R1Lease](t, r1LeaseKey(id))
		if e != nil {
			return e
		}
		if lease.Fence != f || len(lease.Pending) != 0 {
			return jobs.ErrReconciliation
		}
		return nil
	})
}
