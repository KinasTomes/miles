package write

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

type CanonicalStore interface {
	Lookup
	storage.PackLifecycleStore
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
}

var _ Publisher = (*StorePublisher)(nil)

func (p *StorePublisher) Reuse(ctx context.Context, row storage.Chunk) (bool, error) {
	if p == nil || p.Metadata == nil || p.Guard == nil {
		return false, storage.ErrInvalid
	}
	if err := row.Validate(); err != nil {
		return false, err
	}
	return p.Guard.Pin(ctx, row)
}

func (p *StorePublisher) Publish(ctx context.Context, ref storage.PackRef, data, sidecar []byte, entries []pack.Entry, prior map[storage.ChunkKey]storage.Chunk) (map[storage.ChunkKey]storage.Chunk, error) {
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
		resolved, err := p.canonicalize(ctx, row, old, found)
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

func (p *StorePublisher) canonicalize(ctx context.Context, desired, old storage.Chunk, found bool) (storage.Chunk, error) {
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return storage.Chunk{}, err
		}
		if !found {
			winner, inserted, err := p.Metadata.PutChunkIfAbsent(ctx, desired, p.Writer)
			if err == nil {
				if inserted {
					return winner, nil
				}
				old, found = winner, true
			} else if !errors.Is(err, storage.ErrUnavailable) {
				return storage.Chunk{}, err
			} else {
				// A lost ACK cannot prove absence. Read authoritative state;
				// if it is still unavailable, keep the pack as an orphan.
				old, found, err = p.readCanonical(ctx, desired.Key)
				if err != nil || !found {
					return storage.Chunk{}, storage.ErrUnavailable
				}
				if old.Location != desired.Location {
					return storage.Chunk{}, storage.ErrUnavailable
				}
				return old, nil
			}
		}
		if old.Key != desired.Key || old.RawBytes != desired.RawBytes || old.Validate() != nil {
			return storage.Chunk{}, storage.ErrInvalid
		}
		if old.Location == desired.Location {
			return old, nil
		}
		accepted, err := p.Reuse(ctx, old)
		if err != nil {
			return storage.Chunk{}, err
		}
		if accepted {
			return old, nil
		}
		winner, err := p.Metadata.CompareAndSwapChunkLocation(ctx, desired.Key, old.LocationRevision, desired.Location, p.Writer)
		if err == nil {
			return winner, nil
		}
		if !errors.Is(err, storage.ErrConflict) && !errors.Is(err, storage.ErrNotFound) && !errors.Is(err, storage.ErrUnavailable) {
			return storage.Chunk{}, err
		}
		unknown := errors.Is(err, storage.ErrUnavailable)
		old, found, err = p.readCanonical(ctx, desired.Key)
		if err != nil {
			return storage.Chunk{}, err
		}
		if unknown {
			if !found || old.Location != desired.Location {
				return storage.Chunk{}, storage.ErrUnavailable
			}
			return old, nil
		}
	}
	return storage.Chunk{}, fmt.Errorf("%w: canonical contention budget exhausted", storage.ErrUnavailable)
}

func (p *StorePublisher) readCanonical(ctx context.Context, key storage.ChunkKey) (storage.Chunk, bool, error) {
	rows, err := lookupComplete(ctx, p.Metadata, []storage.ChunkKey{key})
	if err != nil || rows == nil {
		return storage.Chunk{}, false, storage.ErrUnavailable
	}
	row, ok := rows[key]
	return row, ok, nil
}
