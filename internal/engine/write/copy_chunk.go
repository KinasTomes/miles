package write

import (
	"context"
	"time"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

// CopyChunk prepares one destination manifest reference using the admitted
// destination writer. The caller holds the source read view while load runs
// and retains destination writer protection through manifest publication.
// A failed publication leaves any staged generation for normal GC.
func (p *StorePublisher) CopyChunk(ctx context.Context, source storage.ChunkRef, domain string, load func(context.Context) ([]byte, error), restoreRequiredUntil ...time.Time) (storage.ChunkRef, error) {
	var zero storage.ChunkRef
	var required time.Time
	if len(restoreRequiredUntil) == 1 {
		required = restoreRequiredUntil[0]
	}
	if len(restoreRequiredUntil) > 1 {
		return zero, storage.ErrInvalid
	}
	dest := source
	dest.Key.Domain = domain
	if p == nil || p.Metadata == nil || p.Writer.LeaseID == "" || source.Key.Validate() != nil || dest.Key.Validate() != nil ||
		source.RawBytes <= 0 || source.RawBytes > engine.MaxChunkBytes || source.LogicalOffset < 0 || load == nil {
		return zero, storage.ErrInvalid
	}
	prior, err := lookupComplete(ctx, p.Metadata, []storage.ChunkKey{dest.Key})
	if err != nil {
		return zero, err
	}
	if row, found := prior[dest.Key]; found {
		if row.Key != dest.Key || row.RawBytes != dest.RawBytes {
			return zero, storage.ErrInvalid
		}
		reusable, err := p.reuse(ctx, row, required)
		if err != nil {
			return zero, err
		}
		if reusable {
			return dest, nil
		}
	}
	raw, err := load(ctx)
	if err != nil {
		return zero, err
	}
	key, err := engine.HashChunk(domain, raw)
	if err != nil || key != dest.Key || int64(len(raw)) != source.RawBytes {
		return zero, storage.ErrInvalid
	}

	ref, err := pack.NewRef()
	if err != nil {
		return zero, err
	}
	builder, err := pack.NewBuilder(ref)
	if err != nil {
		return zero, err
	}
	if err = builder.Add(pack.Blob{Key: key, Codec: "raw", RawLength: uint32(len(raw)), Stored: raw}); err != nil {
		return zero, err
	}
	data, index, entries, err := builder.Finish()
	if err != nil {
		return zero, err
	}
	winners, err := p.publish(ctx, ref, data, index, entries, prior, required)
	if err != nil {
		return zero, err
	}
	if winner := winners[key]; winner.Key != key || winner.RawBytes != dest.RawBytes || winner.Validate() != nil {
		return zero, storage.ErrUnavailable
	}
	return dest, nil
}
