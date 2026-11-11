package write

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
)

type CanonicalStore interface {
	Lookup
	storage.PackLifecycleStore
	storage.PackProtectionStore
	GetPacks(context.Context, []storage.PackRef) (map[storage.PackRef]storage.PackInfo, error)
	PutChunkIfAbsent(context.Context, storage.Chunk, storage.WriterFence) (storage.Chunk, bool, error)
	CompareAndSwapChunkLocation(context.Context, storage.ChunkKey, uint64, storage.ChunkLocation, storage.WriterFence) (storage.Chunk, error)
}

// ReuseGuard must confirm protection of the exact canonical location and
// revision. The caller must retain that protection through head publication.
// A mere metadata lookup or cache hit does not satisfy this interface.
type ReuseGuard interface {
	Pin(context.Context, storage.Chunk) (bool, error)
}

type canonicalOutcome byte

const (
	outcomeNone canonicalOutcome = iota
	outcomeInsert
	outcomeRevive
	outcomeOrphan
	outcomeUnchanged
)

// StorePublisher is the durable pack/canonical adapter for Pipeline. The
// caller supplies an already admitted writer fence and a protection deadline.
type StorePublisher struct {
	WriteCache     storage.Cache
	AdmitOnWrite   bool
	Metadata       CanonicalStore
	Packs          storage.PackStore
	Guard          ReuseGuard
	Writer         storage.WriterFence
	ProtectedUntil time.Time
	Counters       *Counters
	Observer       func(revivedChunks, revivedBytes, orphanedChunks, orphanedBytes uint64)
}

func (p *StorePublisher) recordPhysical(revivedChunks, revivedBytes, orphanedChunks, orphanedBytes uint64) {
	if p == nil {
		return
	}
	if p.Counters != nil {
		p.Counters.RecordPhysical(revivedChunks, revivedBytes, orphanedChunks, orphanedBytes)
	}
	if p.Observer != nil {
		p.Observer(revivedChunks, revivedBytes, orphanedChunks, orphanedBytes)
	}
}

var _ Publisher = (*StorePublisher)(nil)

func (p *StorePublisher) Reuse(ctx context.Context, row storage.Chunk) (bool, error) {
	return p.reuse(ctx, row, time.Time{})
}

func (p *StorePublisher) reuse(ctx context.Context, row storage.Chunk, restoreRequiredUntil time.Time) (bool, error) {
	if p == nil || p.Metadata == nil || p.Guard == nil || p.ProtectedUntil.IsZero() {
		return false, storage.ErrInvalid
	}
	if err := row.Validate(); err != nil {
		return false, err
	}
	infos, err := p.Metadata.GetPacks(ctx, []storage.PackRef{row.Location.Pack})
	if err != nil {
		return false, err
	}
	info, ok := infos[row.Location.Pack]
	if !ok {
		return false, storage.ErrNotFound
	}
	if info.Ref != row.Location.Pack || row.Location.Range.Validate(info.Size) != nil {
		return false, storage.ErrInvalid
	}
	reusable, err := policy.CopyReuseCandidate(info, restoreRequiredUntil)
	if err != nil || !reusable {
		return false, err
	}
	pin := p.Guard.Pin
	if info.State == "Restored" {
		guard, ok := p.Guard.(interface {
			PinCopy(context.Context, storage.Chunk, time.Time) (bool, error)
		})
		if !ok {
			return false, storage.ErrUnavailable
		}
		pin = func(ctx context.Context, row storage.Chunk) (bool, error) {
			return guard.PinCopy(ctx, row, restoreRequiredUntil)
		}
	}
	pinned, err := pin(ctx, row)
	if err != nil || !pinned {
		return false, err
	}
	// Retain the exact generation pin while the backend atomically validates
	// location/state/fence and persists max(existing, required). Unknown outcome
	// aborts reuse; never remove the check or silently relabel a new generation.
	protected, err := p.Metadata.ExtendPackProtection(ctx, storage.PackProtectionUpdate{Chunk: row, ExpectedStateRevision: info.StateRevision, RequiredUntil: p.ProtectedUntil, RestoreRequiredUntil: restoreRequiredUntil, Writer: p.Writer})
	if err != nil {
		return false, err
	}
	canReuse, err := policy.CopyReuseCandidate(protected, restoreRequiredUntil)
	if err != nil || !canReuse || protected.Ref != info.Ref || protected.ProtectedUntil.Before(p.ProtectedUntil) {
		return false, storage.ErrUnavailable
	}
	return true, nil
}

func (p *StorePublisher) Publish(ctx context.Context, ref storage.PackRef, data, sidecar []byte, entries []pack.Entry, prior map[storage.ChunkKey]storage.Chunk) (map[storage.ChunkKey]storage.Chunk, error) {
	return p.publish(ctx, ref, data, sidecar, entries, prior, time.Time{})
}

func (p *StorePublisher) publish(ctx context.Context, ref storage.PackRef, data, sidecar []byte, entries []pack.Entry, prior map[storage.ChunkKey]storage.Chunk, restoreRequiredUntil time.Time) (map[storage.ChunkKey]storage.Chunk, error) {
	if p == nil || p.Metadata == nil || p.Packs == nil || p.Guard == nil || p.Writer.LeaseID == "" || p.ProtectedUntil.IsZero() {
		return nil, storage.ErrInvalid
	}
	parsed, index, err := pack.Inspect(data, ref)
	if err != nil || !bytes.Equal(index, sidecar) || len(entries) != len(parsed) {
		return nil, storage.ErrInvalid
	}
	for i, entry := range entries {
		if entry != parsed[i] {
			return nil, storage.ErrInvalid
		}
	}
	info := storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: int64(len(data)), Class: storage.Standard,
		State: "Uploading", StateRevision: 1, Writer: p.Writer, ProtectedUntil: p.ProtectedUntil}
	if err := p.Metadata.RegisterUploadingPack(ctx, info, p.Writer); err != nil {
		return nil, err
	}
	physical := info
	physical.State = "Warm"
	if err := p.Packs.PutPack(ctx, physical, bytes.NewReader(data)); err != nil {
		return nil, err
	}
	if err := p.Packs.PutSidecar(ctx, ref, int64(len(sidecar)), bytes.NewReader(sidecar)); err != nil {
		return nil, err
	}
	got, err := p.Packs.HeadPack(ctx, ref)
	if err != nil || got.Size != int64(len(data)) || got.Class != storage.Standard {
		return nil, fmt.Errorf("%w: pack verification: %v", storage.ErrUnavailable, err)
	}
	if reader, ok := p.Packs.(interface {
		ReadSidecar(context.Context, storage.PackRef) ([]byte, error)
	}); ok {
		actual, err := reader.ReadSidecar(ctx, ref)
		if err != nil || !bytes.Equal(actual, sidecar) {
			return nil, fmt.Errorf("%w: sidecar verification: %v", storage.ErrUnavailable, err)
		}
	}
	if _, err := p.Metadata.MarkPackWarm(ctx, ref, 1, p.Writer); err != nil {
		return nil, err
	}
	out := make(map[storage.ChunkKey]storage.Chunk, len(entries))
	for _, entry := range entries {
		row := storage.Chunk{Version: storage.ModelVersion, Key: entry.Key, RawBytes: int64(entry.RawLength), Location: entry.Location(ref), LocationRevision: 1}
		old, found := prior[entry.Key]
		resolved, outcome, err := p.canonicalize(ctx, row, old, found, restoreRequiredUntil)
		switch outcome {
		case outcomeRevive:
			p.recordPhysical(1, uint64(entry.StoredLen), 0, 0)
		case outcomeOrphan:
			p.recordPhysical(0, 0, 1, uint64(entry.StoredLen))
		}
		if err != nil {
			return nil, err
		}
		out[entry.Key] = resolved
		// Only admit complete stored bytes under their own finalized location.
		// A dedup loser must never relabel its bytes as the canonical winner.
		if p.AdmitOnWrite && p.WriteCache != nil && resolved.Location == row.Location {
			r := row.Location.Range
			_ = p.WriteCache.Put(ctx, storage.CacheKey{Pack: ref, Range: r}, data[r.Offset:r.Offset+r.Length])
		}
	}
	return out, nil
}

func (p *StorePublisher) canonicalize(ctx context.Context, desired, old storage.Chunk, found bool, restoreRequiredUntil time.Time) (storage.Chunk, canonicalOutcome, error) {
	for range 8 {
		if err := ctx.Err(); err != nil {
			return storage.Chunk{}, outcomeNone, err
		}
		if !found {
			winner, inserted, err := p.Metadata.PutChunkIfAbsent(ctx, desired, p.Writer)
			if err == nil {
				if inserted {
					return winner, outcomeInsert, nil
				}
				old, found = winner, true
			} else if !errors.Is(err, storage.ErrUnavailable) {
				return storage.Chunk{}, outcomeNone, err
			} else {
				// A lost ACK cannot prove absence. Read authoritative state;
				// if it is still unavailable, keep the pack as an orphan.
				old, found, err = p.readCanonical(ctx, desired.Key)
				if err != nil || !found {
					return storage.Chunk{}, outcomeNone, storage.ErrUnavailable
				}
				if old.Location != desired.Location {
					return storage.Chunk{}, outcomeNone, storage.ErrUnavailable
				}
				return old, outcomeInsert, nil
			}
		}
		if old.Key != desired.Key || old.RawBytes != desired.RawBytes || old.Validate() != nil {
			return storage.Chunk{}, outcomeNone, storage.ErrInvalid
		}
		if old.Location == desired.Location {
			return old, outcomeUnchanged, nil
		}
		accepted, err := p.reuse(ctx, old, restoreRequiredUntil)
		if err != nil {
			return storage.Chunk{}, outcomeNone, err
		}
		if accepted {
			return old, outcomeOrphan, nil
		}
		winner, err := p.Metadata.CompareAndSwapChunkLocation(ctx, desired.Key, old.LocationRevision, desired.Location, p.Writer)
		if err == nil {
			return winner, outcomeRevive, nil
		}
		if !errors.Is(err, storage.ErrConflict) && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrUnavailable) {
			return storage.Chunk{}, outcomeNone, err
		}
		unknown := errors.Is(err, storage.ErrUnavailable)
		old, found, err = p.readCanonical(ctx, desired.Key)
		if err != nil {
			return storage.Chunk{}, outcomeNone, err
		}
		if unknown {
			if !found || old.Location != desired.Location {
				return storage.Chunk{}, outcomeNone, storage.ErrUnavailable
			}
			return old, outcomeRevive, nil
		}
	}
	return storage.Chunk{}, outcomeNone, fmt.Errorf("%w: canonical contention budget exhausted", storage.ErrUnavailable)
}

func (p *StorePublisher) readCanonical(ctx context.Context, key storage.ChunkKey) (storage.Chunk, bool, error) {
	rows, err := lookupComplete(ctx, p.Metadata, []storage.ChunkKey{key})
	if err != nil || rows == nil {
		return storage.Chunk{}, false, storage.ErrUnavailable
	}
	row, ok := rows[key]
	return row, ok, nil
}

// packRecorder collects the exact pack generation of every chunk location
// a Publisher reused or published, for multipart root retention.
type packRecorder struct {
	Publisher
	packs map[storage.PackRef]struct{}
}

func (r *packRecorder) Reuse(ctx context.Context, row storage.Chunk) (bool, error) {
	ok, err := r.Publisher.Reuse(ctx, row)
	if ok && err == nil {
		r.packs[row.Location.Pack] = struct{}{}
	}
	return ok, err
}

func (r *packRecorder) Publish(ctx context.Context, ref storage.PackRef, data, sidecar []byte, entries []pack.Entry, prior map[storage.ChunkKey]storage.Chunk) (map[storage.ChunkKey]storage.Chunk, error) {
	out, err := r.Publisher.Publish(ctx, ref, data, sidecar, entries, prior)
	for _, row := range out {
		r.packs[row.Location.Pack] = struct{}{}
	}
	return out, err
}

// refs returns the recorded generations in a stable order.
func (r *packRecorder) refs() []storage.PackRef {
	out := slices.Collect(maps.Keys(r.packs))
	slices.SortFunc(out, func(a, b storage.PackRef) int {
		return cmp.Or(strings.Compare(a.ID, b.ID), strings.Compare(a.Generation, b.Generation))
	})
	return out
}
