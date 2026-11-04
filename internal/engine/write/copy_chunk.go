package write

import (
	"context"
	"time"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
)

// CopyChunks prepares ordered references from a pinned source manifest iterator.
// next returns io.EOF at the end. The caller holds the source read view while
// load runs and retains destination writer protection through head publication.
func (p *StorePublisher) CopyChunks(ctx context.Context, domain string, compression engine.CompressionPolicy,
	request policy.Request, utc policy.UTCProof,
	next func(context.Context) (storage.ChunkRef, error),
	load func(context.Context, storage.ChunkRef) ([]byte, error),
	emit func(storage.ChunkRef) error) (Result, error) {
	required, err := policy.RestoreRequiredUntil(request, utc)
	if err != nil {
		return Result{}, err
	}
	if p == nil || p.Metadata == nil || p.Writer.LeaseID == "" || next == nil || load == nil || storage.ValidateDomain(domain) != nil {
		return Result{}, storage.ErrInvalid
	}
	publisher := copyPublisher{p, required}
	pipeline := Pipeline{Lookup: p.Metadata, Publisher: publisher, Compression: compression}
	return pipeline.run(ctx, domain, func(ctx context.Context) (pipelineChunk, error) {
		source, err := next(ctx)
		if err != nil {
			return pipelineChunk{}, err
		}
		dest := source
		dest.Key.Domain = domain
		if source.Key.Validate() != nil || dest.Key.Validate() != nil ||
			source.RawBytes <= 0 || source.RawBytes > engine.MaxChunkBytes || source.LogicalOffset < 0 {
			return pipelineChunk{}, storage.ErrInvalid
		}
		return pipelineChunk{ref: dest, source: source}, nil
	}, load, emit)
}

type copyPublisher struct {
	store                *StorePublisher
	restoreRequiredUntil time.Time
}

func (p copyPublisher) Reuse(ctx context.Context, row storage.Chunk) (bool, error) {
	return p.store.reuse(ctx, row, p.restoreRequiredUntil)
}

func (p copyPublisher) Publish(ctx context.Context, ref storage.PackRef, data, index []byte, entries []pack.Entry, prior map[storage.ChunkKey]storage.Chunk) (map[storage.ChunkKey]storage.Chunk, error) {
	return p.store.publish(ctx, ref, data, index, entries, prior, p.restoreRequiredUntil)
}
