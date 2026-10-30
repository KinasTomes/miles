package operations

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
)

// NextProtectedPack is shared business validation, not a CAS implementation.
// Adapters must read/check/write these records in one fenced native transaction.
func NextProtectedPack(row s.Chunk, p s.PackInfo, r s.PackProtectionUpdate, now time.Time) (s.PackInfo, error) {
	if r.Validate() != nil || !r.RequiredUntil.After(now) {
		return s.PackInfo{}, s.ErrInvalid
	}
	if row != r.Chunk || p.Ref != r.Chunk.Location.Pack || p.StateRevision != r.ExpectedStateRevision {
		return s.PackInfo{}, s.ErrConflict
	}
	if p.Version != s.ModelVersion || r.Chunk.Location.Range.Validate(p.Size) != nil {
		return s.PackInfo{}, s.ErrInvalid
	}
	reusable, err := policy.WriteReuseCandidate(p.State, p.Class)
	if err != nil || !reusable {
		return s.PackInfo{}, s.ErrConflict
	}
	if !p.ProtectedUntil.IsZero() && (p.ProtectedUntil.UTC().Year() < 1 || p.ProtectedUntil.UTC().Year() > 9999) {
		return s.PackInfo{}, s.ErrUnavailable
	}
	if p.ProtectedUntil.Before(r.RequiredUntil) {
		p.ProtectedUntil = r.RequiredUntil.UTC()
	}
	return p, nil
}

// PackStateProjection preserves the already-published Packs projection schema.
// Protection is a separate fact: changing this projection at the SAME revision
// would make old and new journal pages contradict each other during replay.
func PackStateProjection(p s.PackInfo) any {
	return struct {
		Version  uint32
		Ref      s.PackRef
		Size     int64
		Class    s.StorageClass
		State    string
		Revision uint64
	}{1, p.Ref, p.Size, p.Class, p.State, p.StateRevision}
}

// ExtendProtection is executed by the native transaction engine. New protection
// facts have their own revision; the bundle also retains unchanged pack state.
// Changing protection must not invalidate an already admitted read generation.
func ExtendProtection(t Tx, ctx context.Context, r s.PackProtectionUpdate, now time.Time, requireProducer bool) (s.PackInfo, error) {
	var zero s.PackInfo
	if r.Validate() != nil {
		return zero, s.ErrInvalid
	}
	identity := r.Chunk.Location.Pack.ID + "/" + r.Chunk.Location.Pack.Generation
	input := r
	input.RequiredUntil = r.RequiredUntil.UTC()
	body, err := json.Marshal(input)
	if err != nil {
		return zero, s.ErrInvalid
	}
	root, admitted := s.MutationFromContext(ctx)
	if requireProducer && !admitted {
		return zero, s.ErrFenced
	}
	if admitted {
		if root.Writer != r.Writer {
			return zero, s.ErrFenced
		}
		digest := sha256.Sum256(body)
		child := s.ChildMutation(root, "PackProtection", identity, "protect/"+hex.EncodeToString(digest[:]))
		ctx = s.WithMutation(ctx, child)
		old, err := Replay(t, ctx, "PackProtection", "entity_put", identity, input)
		if err == nil {
			if json.Unmarshal(old.Value, &zero) != nil || zero.Ref != r.Chunk.Location.Pack || zero.ProtectedUntil.Before(r.RequiredUntil) {
				return s.PackInfo{}, s.ErrUnavailable
			}
			return zero, nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return zero, err
		}
	}
	row, err := get[s.Chunk](t, "chunks", r.Chunk.Key.Domain, r.Chunk.Key.Hash)
	if err != nil {
		return zero, err
	}
	p, err := get[s.PackInfo](t, "packs", r.Chunk.Location.Pack.ID, r.Chunk.Location.Pack.Generation)
	if err != nil {
		return zero, err
	}
	out, err := NextProtectedPack(row, p, r, now)
	if err != nil {
		return zero, err
	}
	if out.ProtectedUntil.After(p.ProtectedUntil) {
		revision, err := get[uint64](t, "pack-protection", out.Ref.ID, out.Ref.Generation)
		if err != nil && !errors.Is(err, s.ErrNotFound) {
			return zero, err
		}
		if revision >= s.MaxPolicyRevision {
			return zero, s.ErrConflict
		}
		revision++
		if err := put(t, "pack-protection", revision, out.Ref.ID, out.Ref.Generation); err != nil {
			return zero, err
		}
		if admitted {
			state, err := Projection("Packs", identity, out.StateRevision, PackStateProjection(out), false)
			if err != nil {
				return zero, err
			}
			protection, err := Projection("PackProtection", identity, revision, struct {
				Version        uint32
				Ref            s.PackRef
				ProtectedUntil time.Time
			}{1, out.Ref, out.ProtectedUntil}, false)
			if err != nil {
				return zero, err
			}
			c, _ := s.MutationFromContext(ctx)
			if _, err := Produce(t, c, "PackProtection", "entity_put", identity, revision, body, []format.Effect{state, protection}, out); err != nil {
				return zero, err
			}
		}
	}
	// Even max-no-op writes the identical record so DynamoDB condition-checks
	// every observed writer/location/state in the final native transaction.
	if err := put(t, "packs", out, out.Ref.ID, out.Ref.Generation); err != nil {
		return zero, err
	}
	return out, nil
}
