// Package write joins the streaming engine with immutable packs. Publication
// and R1 protection are supplied by the caller through Publisher.
package write

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

// Lookup must return a complete authoritative result or an error. In
// particular, an unprocessed/throttled key must never look like an absent key.
type Lookup interface {
	GetChunks(context.Context, []storage.ChunkKey, bool) (map[storage.ChunkKey]storage.Chunk, error)
}

// Publisher protects an existing canonical location before reuse and makes
// every newly packed entry canonical before returning. A failed publication
// leaves the physical bytes as orphans; it must not publish an object head.
type Publisher interface {
	Reuse(context.Context, storage.Chunk) (bool, error)
	Publish(context.Context, storage.PackRef, []byte, []byte, []pack.Entry, map[storage.ChunkKey]storage.Chunk) (map[storage.ChunkKey]storage.Chunk, error)
}

type Pipeline struct {
	Lookup      Lookup
	Publisher   Publisher
	Chunking    engine.ChunkingParams
	Compression engine.CompressionPolicy
	// BatchBytes bounds retained raw data; zero selects 32 MiB. Every batch
	// also has at most 100 distinct lookup keys for future DynamoDB adapters.
	BatchBytes int
}

type Result struct {
	Size       int64
	ChunkCount uint64
	PackCount  uint64
}

const defaultBatchBytes = 32 << 20
const maxBatchKeys = 100

// Run consumes one logical object and emits ordered refs only after their
// canonical locations have been resolved. The emitter may stream pages to a
// backend; Run never retains all refs of a large object.
func (p Pipeline) Run(ctx context.Context, body io.Reader, domain string, emit func(storage.ChunkRef) error) (Result, error) {
	if p.Lookup == nil || p.Publisher == nil || emit == nil {
		return Result{}, storage.ErrInvalid
	}
	limit := p.BatchBytes
	if limit == 0 {
		limit = defaultBatchBytes
	}
	if limit < engine.MaxChunkBytes || limit > pack.MaxPackBytes/2 {
		return Result{}, fmt.Errorf("%w: batch byte budget", storage.ErrInvalid)
	}
	chunker, err := engine.NewChunker(body, domain, p.Chunking)
	if err != nil {
		return Result{}, err
	}
	compressor, err := engine.NewCompressor(p.Compression)
	if err != nil {
		return Result{}, err
	}
	defer compressor.Close()
	var result Result
	var pending *engine.Chunk
	for {
		batch := make([]engine.Chunk, 0, maxBatchKeys)
		unique := make(map[storage.ChunkKey]engine.Chunk)
		bytesHeld := 0
		end := false
		for len(batch) < maxBatchKeys {
			var chunk engine.Chunk
			if pending != nil {
				chunk, pending = *pending, nil
			} else {
				chunk, err = chunker.Next(ctx)
				if errors.Is(err, io.EOF) {
					end = true
					break
				}
				if err != nil {
					return result, err
				}
			}
			if len(batch) != 0 && bytesHeld+len(chunk.Bytes) > limit {
				pending = &chunk
				break
			}
			batch = append(batch, chunk)
			bytesHeld += len(chunk.Bytes)
			if _, ok := unique[chunk.Key]; !ok {
				unique[chunk.Key] = chunk
			}
		}
		if len(batch) == 0 {
			return result, nil
		}
		keys := make([]storage.ChunkKey, 0, len(unique))
		for key := range unique {
			keys = append(keys, key)
		}
		rows, err := lookupComplete(ctx, p.Lookup, keys)
		if err != nil {
			return result, err
		}
		if rows == nil {
			return result, fmt.Errorf("%w: nil lookup result", storage.ErrUnavailable)
		}
		for key, row := range rows {
			if _, requested := unique[key]; !requested || row.Key != key || row.Validate() != nil {
				return result, fmt.Errorf("%w: invalid lookup result", storage.ErrUnavailable)
			}
		}
		var builder *pack.Builder
		var ref storage.PackRef
		prior := make(map[storage.ChunkKey]storage.Chunk)
		resolved := make(map[storage.ChunkKey]storage.Chunk)
		for key, chunk := range unique {
			if row, found := rows[key]; found {
				ok, err := p.Publisher.Reuse(ctx, row)
				if err != nil {
					return result, err
				}
				if ok {
					resolved[key] = row
					continue
				}
				prior[key] = row
			}
			encoded, err := compressor.Encode(chunk)
			if err != nil {
				return result, err
			}
			if builder == nil {
				ref, err = pack.NewRef()
				if err != nil {
					return result, err
				}
				builder, err = pack.NewBuilder(ref)
				if err != nil {
					return result, err
				}
			}
			if err := builder.Add(pack.Blob{Key: key, Codec: encoded.Codec, RawLength: uint32(encoded.RawBytes), Stored: encoded.Bytes}); err != nil {
				return result, err
			}
		}
		if builder != nil {
			packBytes, sidecar, entries, err := builder.Finish()
			if err != nil {
				return result, err
			}
			published, err := p.Publisher.Publish(ctx, ref, packBytes, sidecar, entries, prior)
			if err != nil {
				return result, err
			}
			for _, entry := range entries {
				row, ok := published[entry.Key]
				if !ok || row.Key != entry.Key || row.RawBytes != int64(entry.RawLength) || row.Validate() != nil {
					return result, fmt.Errorf("%w: incomplete canonical publication", storage.ErrUnavailable)
				}
				resolved[entry.Key] = row
			}
			result.PackCount++
		}
		for _, chunk := range batch {
			if _, ok := resolved[chunk.Key]; !ok {
				return result, fmt.Errorf("%w: unresolved chunk", storage.ErrUnavailable)
			}
			if err := emit(storage.ChunkRef{Key: chunk.Key, RawBytes: int64(len(chunk.Bytes)), LogicalOffset: chunk.Offset}); err != nil {
				return result, err
			}
			result.Size += int64(len(chunk.Bytes))
			result.ChunkCount++
		}
		if end {
			return result, nil
		}
	}
}

// A complete successful response distinguishes an absent key from a backend
// that could not process the whole batch. The backend must retry any DynamoDB
// UnprocessedKeys before reporting success; this bounded retry covers a
// transient whole-batch failure without spinning past request cancellation.
func lookupComplete(ctx context.Context, store Lookup, keys []storage.ChunkKey) (map[storage.ChunkKey]storage.Chunk, error) {
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := store.GetChunks(ctx, keys, true)
		if err == nil {
			return rows, nil
		}
		if !errors.Is(err, storage.ErrUnavailable) || attempt == 2 {
			return nil, err
		}
		timer := time.NewTimer(time.Duration(10<<attempt) * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, storage.ErrUnavailable
}
