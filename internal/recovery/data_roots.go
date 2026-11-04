package recovery

import (
	"context"
	"sort"

	"github.com/KinasTomes/miles/internal/pack"
	s "github.com/KinasTomes/miles/internal/storage"
)

// DataRoot binds exact historical locations to a verified checkpoint and seal.
// Base and Dependencies identify immutable artifacts that must remain readable.
type DataRoot struct {
	Catalog      Catalog
	Cut          SupportedCut
	Snapshot     Snapshot
	Dependencies []Dependency
	Locations    []s.Chunk
}

// EnumerateProtectedData verifies each advertised cut against physical storage.
// Archived payloads are checked for existence and index integrity; byte
// verification is deferred until restore. The limit counts cuts and locations.
func (w Retention) EnumerateProtectedData(ctx context.Context, limit int) ([]DataRoot, error) {
	if limit <= 0 || w.Packs == nil {
		return nil, s.ErrInvalid
	}
	cuts, err := w.protectedCuts(ctx)
	if err != nil {
		return nil, err
	}
	if len(cuts) > limit {
		return nil, s.ErrUnavailable
	}
	limit -= len(cuts)
	roots := make([]DataRoot, 0, len(cuts))
	for _, source := range cuts {
		cp := Checkpointer{Objects: w.Objects, Catalog: source.Catalog}
		image, err := cp.Load(ctx, source.Cut.Checkpoint)
		if err != nil {
			return nil, err
		}
		if image.Snapshot.BeforeFirst || image.Snapshot.Target != source.Cut.Epoch {
			return nil, s.ErrInvalid
		}
		if _, err := readArtifact(ctx, w.Objects, image.Snapshot.Base); err != nil {
			return nil, err
		}
		rebuilt, err := cp.rebuildImage(ctx, image)
		if err != nil {
			return nil, err
		}
		if len(rebuilt.Live) > limit {
			return nil, s.ErrUnavailable
		}
		limit -= len(rebuilt.Live)
		refs, err := w.rootRefs(ctx, source.Cut)
		if err != nil {
			return nil, err
		}
		locations, err := w.verifyLocations(ctx, rebuilt, refs)
		if err != nil {
			return nil, err
		}
		roots = append(roots, DataRoot{source.Catalog, source.Cut, image.Snapshot, image.Dependencies, locations})
	}
	return roots, nil
}

func (w Retention) verifyLocations(ctx context.Context, rebuilt Rebuilt, refs map[s.PackRef]bool) ([]s.Chunk, error) {
	indexes := make(map[s.PackRef]map[s.ChunkKey]pack.Entry)
	locations := make([]s.Chunk, 0, len(rebuilt.Live))
	for key := range rebuilt.Live {
		row := rebuilt.Chunks[key]
		ref := row.Location.Pack
		if !refs[ref] {
			return nil, s.ErrInvalid
		}
		entries, found := indexes[ref]
		if !found {
			reader, ok := w.Packs.(interface {
				ReadSidecar(context.Context, s.PackRef) ([]byte, error)
			})
			if !ok {
				return nil, s.ErrUnavailable
			}
			info, err := w.Packs.HeadPack(ctx, ref)
			if err != nil {
				return nil, err
			}
			if info.Ref != ref || info.Size != rebuilt.Packs[ref].Size {
				return nil, s.ErrInvalid
			}
			index, err := reader.ReadSidecar(ctx, ref)
			if err != nil {
				return nil, err
			}
			parsed, _, err := pack.ParseIndex(index, ref)
			if err != nil {
				return nil, err
			}
			entries = make(map[s.ChunkKey]pack.Entry, len(parsed))
			for _, entry := range parsed {
				entries[entry.Key] = entry
			}
			indexes[ref] = entries
		}
		entry, ok := entries[key]
		if !ok || entry.Location(ref) != row.Location || int64(entry.RawLength) != row.RawBytes {
			return nil, s.ErrInvalid
		}
		locations = append(locations, row)
	}
	if len(indexes) != len(refs) {
		return nil, s.ErrInvalid
	}
	if err := rebuilt.verifyPayload(ctx, w.Packs, true); err != nil {
		return nil, err
	}
	sort.Slice(locations, func(i, j int) bool {
		a, b := locations[i].Key, locations[j].Key
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		return a.Hash < b.Hash
	})
	return locations, nil
}
