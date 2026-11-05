package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type sweepRetirement struct {
	Version uint32
	Ref     s.PackRef
	At      time.Time
}

func (r sweepRetirement) Validate() error {
	if r.Version != 1 || r.Ref.Validate() != nil || r.At.IsZero() {
		return s.ErrInvalid
	}
	return nil
}
func sweepRetirementKey(r s.PackRef) Key {
	return stringKey("SystemConfig", "config_id", "coordination/gc/retired/"+r.ID+"/"+r.Generation)
}
func (m *Store) reserveSweep(t *transaction, a *lifecycle.Action, b s.R1Bounds) error {
	g, e := r1Read[s.R1Gate](t, r1GateKey())
	if e != nil {
		return e
	}
	// Admission and this reservation both condition the same shared gate.
	// Requiring the job to be the only remaining lease also excludes a
	// reader still resolving its view or a closed writer with unknown work.
	if g.State != "Open" || g.Active != 1 {
		return s.ErrR1Busy
	}
	pins, e := r1Read[r1Pins](t, r1PinKey(a.Source.Ref))
	if e != nil && !errors.Is(e, s.ErrNotFound) {
		return e
	}
	if pins.Count != 0 || e == nil && pins.Pack != a.Source.Ref {
		return s.ErrR1Busy
	}
	p := a.Source
	if p.ProtectedUntil.IsZero() || b.Lower.Before(p.ProtectedUntil) || b.Lower.Before(p.MinRetentionUntil) {
		return s.ErrR1Busy
	}
	switch p.State {
	case "Uploading":
		// Publication requires this writer to be active. A recorded release
		// proves that its already-dispatched work has quiesced.
		old, e := r1Read[s.R1Lease](t, r1LeaseKey(p.Writer.LeaseID))
		if e != nil {
			return e
		}
		if old.Fence != p.Writer || old.State != "Released" {
			return s.ErrR1Busy
		}
	case "Warm":
		// First pass records retirement under the same stable gate as mark.
	case "Retiring":
		fact, e := r1Read[sweepRetirement](t, sweepRetirementKey(p.Ref))
		if e != nil {
			return e
		}
		if fact.Ref != p.Ref || b.Lower.Before(fact.At.Add(24*time.Hour+10*time.Minute)) {
			return s.ErrR1Busy
		}
		a.RetiredAt = fact.At
	default:
		return s.ErrR1Busy
	}
	if p.Class != s.Standard && p.Class != s.StandardIA {
		return s.ErrR1Busy
	}
	g.State = "Maintenance"
	g.Revision++
	a.GateRevision = g.Revision
	return r1Put(t, r1GateKey(), g)
}
func (m *Store) reopenSweep(t *transaction, a lifecycle.Action) error {
	g, e := r1Read[s.R1Gate](t, r1GateKey())
	if e != nil {
		return e
	}
	if g.State != "Maintenance" || g.Revision != a.GateRevision || g.Incarnation != a.Fence.ServiceIncarnation {
		return s.ErrFenced
	}
	g.State = "Open"
	g.Revision++
	return r1Put(t, r1GateKey(), g)
}

// RetireSweep closes admission to an unreferenced Warm generation before
// starting its retention clock. Until then it remains ordinary Warm data.
func (m *Store) RetireSweep(ctx context.Context, j jobs.Job) (out lifecycle.Action, err error) {
	err = m.update(ctx, func(t *transaction) error {
		if j.Type != "gc" {
			return s.ErrInvalid
		}
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
		if a.State != "Prepared" || a.Kind != "gc" || a.Fence != j.Worker || a.Source.State != "Warm" {
			return s.ErrConflict
		}
		p, e := get[s.PackInfo](t, packKey(a.Source.Ref))
		if e != nil {
			return e
		}
		if p.State != "Warm" || p.StateRevision != a.Source.StateRevision {
			return s.ErrConflict
		}
		b, e := m.r1Bounds(ctx)
		if e != nil {
			return e
		}
		fact := sweepRetirement{Version: 1, Ref: p.Ref, At: b.Upper}
		if e = r1Put(t, sweepRetirementKey(p.Ref), fact); e != nil {
			return e
		}
		p.State = "Retiring"
		p.StateRevision++
		a.RetiredAt = fact.At
		a.State = "Done"
		a.Revision++
		if e = m.reopenSweep(t, a); e != nil {
			return e
		}
		if e = m.lifecycleRecord(t, j, a, p); e != nil {
			return e
		}
		out = a
		return nil
	})
	return
}

const chunkRetirementAttribute = "retired_location_revision"

func chunkRetirementRevision(t *transaction, key s.ChunkKey) (uint64, error) {
	address, e := resolve(chunkKey(key))
	if e != nil {
		return 0, e
	}
	row, e := t.observe(address.key)
	if e != nil {
		return 0, e
	}
	return decodeChunkRetirement(row.after)
}

func decodeChunkRetirement(item Item) (uint64, error) {
	attribute := item[chunkRetirementAttribute]
	if attribute == nil {
		return 0, nil
	}
	n, ok := attribute.(*types.AttributeValueMemberN)
	if !ok || n == nil {
		return 0, s.ErrUnavailable
	}
	revision, e := strconv.ParseUint(n.Value, 10, 64)
	if e != nil || revision == 0 || revision > s.MaxPolicyRevision {
		return 0, s.ErrUnavailable
	}
	return revision, nil
}

// Each transaction retires one expected winner and journals its tombstone.
// A resumed mark skips committed retirements and rechecks the remaining rows
// while the same maintenance gate still excludes publishers and readers.
func (m *Store) RetireCanonical(ctx context.Context, j jobs.Job, expected s.Chunk) error {
	producer, e := m.LifecycleProducer(ctx, j)
	if e != nil {
		return e
	}
	return m.update(ctx, func(t *transaction) error {
		if e := m.fence(t, j.Worker); e != nil {
			return e
		}
		if e := m.lifecycleJob(t, j, true); e != nil {
			return e
		}
		action, e := r1Read[lifecycle.Action](t, lifecycleKey(j.ID))
		if e != nil {
			return e
		}
		if action.Kind != "gc" || action.State != "Prepared" || action.Fence != j.Worker || action.Source.State != "Warm" || action.Source.Ref != expected.Location.Pack {
			return s.ErrFenced
		}
		pack, e := get[s.PackInfo](t, packKey(action.Source.Ref))
		if e != nil {
			return e
		}
		if pack.State != "Warm" || pack.StateRevision != action.Source.StateRevision {
			return s.ErrConflict
		}
		key := chunkKey(expected.Key)
		current, e := get[s.Chunk](t, key)
		if errors.Is(e, s.ErrNotFound) {
			revision, e := chunkRetirementRevision(t, expected.Key)
			if e != nil {
				return e
			}
			if revision == expected.LocationRevision+1 {
				return nil
			}
			return s.ErrConflict
		}
		if e != nil {
			return e
		}
		if current != expected || current.LocationRevision >= s.MaxPolicyRevision {
			return s.ErrConflict
		}
		revision := current.LocationRevision + 1
		identity, e := current.Key.PartitionKey()
		if e != nil {
			return e
		}
		body, e := json.Marshal(current)
		if e != nil {
			return e
		}
		effect, e := operations.Projection("Chunks", identity, revision, nil, true)
		if e != nil {
			return e
		}
		mutation := s.ChildMutation(producer, "Chunks", identity, "location_retire")
		if _, e = operations.Produce(multipartTx{t}, mutation, "Chunks", "location_retire", identity, revision, body, []format.Effect{effect}, nil); e != nil {
			return e
		}
		return put(t, key, struct{ RetiredLocationRevision uint64 }{revision})
	})
}
