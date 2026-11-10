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
	WriteCache   storage.Cache
	AdmitOnWrite bool
	Lookup       Lookup
	Publisher    Publisher
	Chunking     engine.ChunkingParams
	Compression  engine.CompressionPolicy
	// BatchBytes bounds retained raw data; zero selects 32 MiB. Every batch
	// also has at most 100 distinct lookup keys for future DynamoDB adapters.
	BatchBytes int
	// Counters optionally accumulates revived/orphaned outcomes across requests.
	Counters *Counters
}

// Revived/Orphaned are distinct identities per request (a key is written at
// most once per pack and looked up again after its pack is published).
type Result struct {
	Size           int64
	ChunkCount     uint64
	PackCount      uint64
	RevivedChunks  uint64
	RevivedBytes   uint64
	OrphanedChunks uint64
	OrphanedBytes  uint64
}

const defaultBatchBytes = 32 << 20
const maxBatchKeys = 100

type pipelineChunk struct {
	ref    storage.ChunkRef
	raw    []byte
	source storage.ChunkRef
}

// Run consumes one logical object and emits ordered refs only after their
// canonical locations have been resolved.
func (p Pipeline) Run(ctx context.Context, body io.Reader, domain string, emit func(storage.ChunkRef) error) (Result, error) {
	chunker, err := engine.NewChunker(body, domain, p.Chunking)
	if err != nil {
		return Result{}, err
	}
	return p.run(ctx, domain, func(ctx context.Context) (pipelineChunk, error) {
		chunk, err := chunker.Next(ctx)
		return pipelineChunk{ref: storage.ChunkRef{Key: chunk.Key, RawBytes: int64(len(chunk.Bytes)), LogicalOffset: chunk.Offset}, raw: chunk.Bytes}, err
	}, nil, emit)
}

func (p Pipeline) run(ctx context.Context, domain string, next func(context.Context) (pipelineChunk, error),
	load func(context.Context, storage.ChunkRef) ([]byte, error), emit func(storage.ChunkRef) error) (Result, error) {
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
	compressor, err := engine.NewCompressor(p.Compression)
	if err != nil {
		return Result{}, err
	}
	defer compressor.Close()
	var result Result
	refs := orderedRefs{domain: domain, emit: emit, result: &result}
	defer refs.Close()
	if p.Counters != nil {
		setPublisherCounters(p.Publisher, p.Counters)
	}
	setPublisherObserver(p.Publisher, func(rc, rb, oc, ob uint64) {
		result.RevivedChunks += rc
		result.RevivedBytes += rb
		result.OrphanedChunks += oc
		result.OrphanedBytes += ob
	})
	var builder *pack.Builder
	var ref storage.PackRef
	prior := make(map[storage.ChunkKey]storage.Chunk)
	pendingKeys := make(map[storage.ChunkKey]int64)
	var publishedSeq uint64
	flush := func() error {
		if builder == nil {
			return nil
		}
		packBytes, sidecar, entries, err := builder.Finish()
		if err != nil {
			return err
		}
		published, err := p.Publisher.Publish(ctx, ref, packBytes, sidecar, entries, prior)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			row, ok := published[entry.Key]
			if !ok || row.Key != entry.Key || row.RawBytes != int64(entry.RawLength) || row.Validate() != nil {
				return fmt.Errorf("%w: incomplete canonical publication", storage.ErrUnavailable)
			}
		}
		result.PackCount++
		publishedSeq++
		builder = nil
		prior = make(map[storage.ChunkKey]storage.Chunk)
		pendingKeys = make(map[storage.ChunkKey]int64)
		return nil
	}
	var pending *pipelineChunk
	for {
		batch := make([]pipelineChunk, 0, maxBatchKeys)
		unique := make(map[storage.ChunkKey]pipelineChunk)
		bytesHeld := 0
		end := false
		for len(batch) < maxBatchKeys {
			var chunk pipelineChunk
			if pending != nil {
				chunk, pending = *pending, nil
			} else {
				chunk, err = next(ctx)
				if errors.Is(err, io.EOF) {
					end = true
					break
				}
				if err != nil {
					return result, err
				}
			}
			if len(batch) != 0 && bytesHeld+int(chunk.ref.RawBytes) > limit {
				pending = &chunk
				break
			}
			batch = append(batch, chunk)
			bytesHeld += int(chunk.ref.RawBytes)
			if first, ok := unique[chunk.ref.Key]; !ok {
				unique[chunk.ref.Key] = chunk
			} else if first.ref.RawBytes != chunk.ref.RawBytes {
				return result, storage.ErrInvalid
			}
		}
		if len(batch) == 0 {
			if err := flush(); err != nil {
				return result, err
			}
			if err := refs.Drain(publishedSeq); err != nil {
				return result, err
			}
			if !refs.Empty() {
				return result, fmt.Errorf("%w: pending refs after final pack", storage.ErrUnavailable)
			}
			return result, nil
		}
		keys := make([]storage.ChunkKey, 0, len(unique))
		alreadyPending := make(map[storage.ChunkKey]struct{})
		for key, chunk := range unique {
			if rawBytes, ok := pendingKeys[key]; ok {
				if rawBytes != chunk.ref.RawBytes {
					return result, storage.ErrInvalid
				}
				alreadyPending[key] = struct{}{}
				continue
			}
			keys = append(keys, key)
		}
		rows := make(map[storage.ChunkKey]storage.Chunk)
		if len(keys) != 0 {
			rows, err = lookupComplete(ctx, p.Lookup, keys)
			if err != nil {
				return result, err
			}
			if rows == nil {
				return result, fmt.Errorf("%w: nil lookup result", storage.ErrUnavailable)
			}
		}
		for key, row := range rows {
			if chunk, requested := unique[key]; !requested || row.Key != key || row.RawBytes != chunk.ref.RawBytes || row.Validate() != nil {
				return result, fmt.Errorf("%w: invalid lookup result", storage.ErrUnavailable)
			}
		}
		for key, chunk := range unique {
			if _, ok := alreadyPending[key]; ok {
				continue
			}
			if row, found := rows[key]; found {
				ok, err := p.Publisher.Reuse(ctx, row)
				if err != nil {
					return result, err
				}
				if ok {
					continue
				}
			}
			raw := chunk.raw
			if load != nil {
				raw, err = load(ctx, chunk.source)
				if err != nil {
					return result, err
				}
				if int64(len(raw)) != chunk.ref.RawBytes {
					return result, storage.ErrInvalid
				}
			}
			encoded, err := compressor.Encode(engine.Chunk{Key: key, Bytes: raw, Offset: chunk.ref.LogicalOffset})
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
			blob := pack.Blob{Key: key, Codec: encoded.Codec, RawLength: uint32(encoded.RawBytes), Stored: encoded.Bytes}
			if err := builder.Add(blob); errors.Is(err, pack.ErrFull) {
				if err := flush(); err != nil {
					return result, err
				}
				ref, err = pack.NewRef()
				if err != nil {
					return result, err
				}
				builder, err = pack.NewBuilder(ref)
				if err != nil {
					return result, err
				}
				err = builder.Add(blob)
				if err != nil {
					return result, err
				}
			} else if err != nil {
				return result, err
			}
			if row, found := rows[key]; found {
				prior[key] = row
			}
			pendingKeys[key] = chunk.ref.RawBytes
			if builder.Ready() {
				if err := flush(); err != nil {
					return result, err
				}
			}
		}
		for _, chunk := range batch {
			waitFor := publishedSeq
			if _, ok := pendingKeys[chunk.ref.Key]; ok {
				waitFor++
			}
			if err := refs.Add(chunk.ref, waitFor); err != nil {
				return result, err
			}
		}
		if err := refs.Drain(publishedSeq); err != nil {
			return result, err
		}
		if end {
			if err := flush(); err != nil {
				return result, err
			}
			if err := refs.Drain(publishedSeq); err != nil {
				return result, err
			}
			if !refs.Empty() {
				return result, fmt.Errorf("%w: pending refs after final pack", storage.ErrUnavailable)
			}
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

func setPublisherCounters(pub Publisher, c *Counters) {
	if sp, ok := pub.(*StorePublisher); ok && sp.Counters == nil {
		sp.Counters = c
	} else if pr, ok := pub.(*packRecorder); ok {
		if sp, ok := pr.Publisher.(*StorePublisher); ok && sp.Counters == nil {
			sp.Counters = c
		}
	}
}

func setPublisherObserver(pub Publisher, fn func(rc, rb, oc, ob uint64)) {
	if sp, ok := pub.(*StorePublisher); ok {
		sp.Observer = fn
	} else if pr, ok := pub.(*packRecorder); ok {
		if sp, ok := pr.Publisher.(*StorePublisher); ok {
			sp.Observer = fn
		}
	}
}
