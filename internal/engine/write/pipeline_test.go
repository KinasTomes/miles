package write

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"math/big"
	"testing"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

type memoryPublisher struct {
	rows       map[storage.ChunkKey]storage.Chunk
	lookupRuns int
	maxKeys    int
	packs      map[storage.PackRef][]byte
}

type flakyLookup struct {
	*memoryPublisher
	failures int
}

func (f *flakyLookup) GetChunks(ctx context.Context, keys []storage.ChunkKey, consistent bool) (map[storage.ChunkKey]storage.Chunk, error) {
	if f.failures > 0 {
		f.failures--
		return nil, storage.ErrUnavailable
	}
	return f.memoryPublisher.GetChunks(ctx, keys, consistent)
}

func newMemoryPublisher() *memoryPublisher {
	return &memoryPublisher{rows: map[storage.ChunkKey]storage.Chunk{}, packs: map[storage.PackRef][]byte{}}
}
func (m *memoryPublisher) GetChunks(ctx context.Context, keys []storage.ChunkKey, consistent bool) (map[storage.ChunkKey]storage.Chunk, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !consistent {
		return nil, storage.ErrInvalid
	}
	m.lookupRuns++
	if len(keys) > m.maxKeys {
		m.maxKeys = len(keys)
	}
	out := map[storage.ChunkKey]storage.Chunk{}
	for _, key := range keys {
		if row, ok := m.rows[key]; ok {
			out[key] = row
		}
	}
	return out, nil
}
func (m *memoryPublisher) Reuse(_ context.Context, row storage.Chunk) (bool, error) {
	return m.rows[row.Key] == row, nil
}
func (m *memoryPublisher) Publish(_ context.Context, ref storage.PackRef, data, sidecar []byte, entries []pack.Entry, _ map[storage.ChunkKey]storage.Chunk) (map[storage.ChunkKey]storage.Chunk, error) {
	parsed, index, err := pack.Inspect(data, ref)
	if err != nil || !bytes.Equal(index, sidecar) || len(parsed) != len(entries) {
		return nil, storage.ErrInvalid
	}
	m.packs[ref] = bytes.Clone(data)
	out := map[storage.ChunkKey]storage.Chunk{}
	for _, entry := range entries {
		if old, ok := m.rows[entry.Key]; ok {
			out[entry.Key] = old
			continue
		}
		row := storage.Chunk{Version: 1, Key: entry.Key, RawBytes: int64(entry.RawLength), Location: entry.Location(ref), LocationRevision: 1}
		m.rows[entry.Key] = row
		out[entry.Key] = row
	}
	return out, nil
}

func TestPipelineBatchesReusesAndKeepsDomainsSeparate(t *testing.T) {
	ctx := context.Background()
	m := newMemoryPublisher()
	p := Pipeline{Lookup: m, Publisher: m, Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	block := bytes.Repeat([]byte("same-content"), (16<<10)/len("same-content")+1)[:16<<10]
	body := bytes.Repeat(block, 205)
	run := func(domain string) (Result, []storage.ChunkRef) {
		t.Helper()
		refs := []storage.ChunkRef{}
		result, err := p.Run(ctx, bytes.NewReader(body), domain, func(ref storage.ChunkRef) error {
			refs = append(refs, ref)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		return result, refs
	}
	first, refs := run("default")
	if first.Size != int64(len(body)) || first.ChunkCount != 205 || first.PackCount != 1 || len(refs) != 205 || m.lookupRuns < 3 || m.maxKeys > maxBatchKeys {
		t.Fatalf("unexpected first run: %+v refs=%d lookups=%d max=%d", first, len(refs), m.lookupRuns, m.maxKeys)
	}
	for i, ref := range refs {
		if ref.LogicalOffset != int64(i*16<<10) || ref.Key != refs[0].Key {
			t.Fatalf("ref %d: %+v", i, ref)
		}
	}
	second, _ := run("default")
	if second.PackCount != 0 || len(m.rows) != 1 {
		t.Fatalf("same-domain reuse: %+v rows=%d", second, len(m.rows))
	}
	third, other := run("tenant-b")
	if third.PackCount != 1 || len(m.rows) != 2 || other[0].Key.Hash != refs[0].Key.Hash || other[0].Key.Domain == refs[0].Key.Domain {
		t.Fatalf("cross-domain identity: %+v rows=%d", third, len(m.rows))
	}
}

func TestPipelineUniqueAndLowCompression(t *testing.T) {
	m := newMemoryPublisher()
	p := Pipeline{Lookup: m, Publisher: m, Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	var body []byte
	for i := byte(0); i < 80; i++ {
		var block []byte
		for n := byte(0); len(block) < 16<<10; n++ {
			seed := sha256.Sum256([]byte{i, n, byte(len(block) >> 8)})
			block = append(block, seed[:]...)
		}
		body = append(body, block[:16<<10]...)
	}
	var refs []storage.ChunkRef
	result, err := p.Run(context.Background(), bytes.NewReader(body), "default", func(ref storage.ChunkRef) error {
		refs = append(refs, ref)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Size != int64(len(body)) || len(refs) != 80 || len(m.rows) != 80 || result.PackCount != 1 {
		t.Fatalf("unique stream: %+v refs=%d rows=%d", result, len(refs), len(m.rows))
	}
	for _, row := range m.rows {
		if row.Location.Codec != "raw" {
			t.Fatal("incompressible chunk did not use raw fallback")
		}
	}
}

func TestPipelineRetriesUnavailableLookupWithoutAssumingAbsence(t *testing.T) {
	m := newMemoryPublisher()
	lookup := &flakyLookup{memoryPublisher: m, failures: 2}
	p := Pipeline{Lookup: lookup, Publisher: m, Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression: engine.CompressionPolicy{Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	_, err := p.Run(context.Background(), bytes.NewReader(bytes.Repeat([]byte("x"), 16<<10)), "default", func(storage.ChunkRef) error { return nil })
	if err != nil || lookup.failures != 0 || len(m.rows) != 1 {
		t.Fatalf("transient failure: err=%v remaining=%d rows=%d", err, lookup.failures, len(m.rows))
	}
	lookup.failures = 3
	_, err = p.Run(context.Background(), bytes.NewReader(bytes.Repeat([]byte("y"), 16<<10)), "default", func(storage.ChunkRef) error { return nil })
	if !errors.Is(err, storage.ErrUnavailable) || len(m.rows) != 1 {
		t.Fatalf("failed lookup published data: err=%v rows=%d", err, len(m.rows))
	}
}
