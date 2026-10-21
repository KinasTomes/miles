package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"sync"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

type segmentedReader struct {
	r    io.Reader
	step int
}

func (s segmentedReader) Read(p []byte) (int, error) {
	if len(p) > s.step {
		p = p[:s.step]
	}
	return s.r.Read(p)
}

func collectChunks(t *testing.T, r io.Reader, params ChunkingParams) ([]Chunk, []byte) {
	t.Helper()
	chunker, err := NewChunker(r, "default", params)
	if err != nil {
		t.Fatal(err)
	}
	var chunks []Chunk
	var restored []byte
	for {
		chunk, err := chunker.Next(t.Context())
		if err == io.EOF {
			return chunks, restored
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Offset != int64(len(restored)) || len(chunk.Bytes) == 0 || len(chunk.Bytes) > params.Max {
			t.Fatalf("bad chunk offset/size: %+v", chunk)
		}
		key, err := HashChunk("default", chunk.Bytes)
		if err != nil || key != chunk.Key {
			t.Fatalf("bad raw hash: %v", err)
		}
		restored = append(restored, chunk.Bytes...)
		chunks = append(chunks, chunk)
	}
}

func signatures(chunks []Chunk) []struct {
	N int
	K storage.ChunkKey
} {
	out := make([]struct {
		N int
		K storage.ChunkKey
	}, len(chunks))
	for i, c := range chunks {
		out[i].N, out[i].K = len(c.Bytes), c.Key
	}
	return out
}

func TestChunkerReaderSegmentationProperty(t *testing.T) {
	params := ChunkingParams{Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10}
	rng := rand.New(rand.NewSource(20261021))
	for caseNo := 0; caseNo < 12; caseNo++ {
		data := make([]byte, rng.Intn(750000))
		if _, err := rng.Read(data); err != nil {
			t.Fatal(err)
		}
		baseline, restored := collectChunks(t, bytes.NewReader(data), params)
		if !bytes.Equal(restored, data) {
			t.Fatalf("case %d did not reconstruct input", caseNo)
		}
		for i, chunk := range baseline {
			if i < len(baseline)-1 && len(chunk.Bytes) < params.Min {
				t.Fatalf("non-final chunk below min: %d", len(chunk.Bytes))
			}
		}
		for _, step := range []int{1, 7, 4096, 65536} {
			actual, reassembled := collectChunks(t, segmentedReader{bytes.NewReader(data), step}, params)
			if !bytes.Equal(reassembled, data) || !reflect.DeepEqual(signatures(actual), signatures(baseline)) {
				t.Fatalf("case %d read step %d changed chunk boundaries", caseNo, step)
			}
		}
	}
}

func TestInsertionDeletionPreserveUnaffectedChunks(t *testing.T) {
	params := ChunkingParams{Min: 256 << 10, Avg: 1 << 20, Max: 4 << 20}
	base := stream("miles-cdc-1", 16<<20)
	before, _ := collectChunks(t, bytes.NewReader(base), params)
	at := len(base) / 2
	inserted := append(bytes.Clone(base[:at]), append([]byte{0}, base[at:]...)...)
	deleted := append(bytes.Clone(base[:at]), base[at+1:]...)
	for name, changed := range map[string][]byte{"insert": inserted, "delete": deleted} {
		after, rebuilt := collectChunks(t, bytes.NewReader(changed), params)
		if !bytes.Equal(rebuilt, changed) {
			t.Fatalf("%s failed round trip", name)
		}
		old := map[storage.ChunkKey]bool{}
		for _, chunk := range before {
			old[chunk.Key] = true
		}
		unchanged := 0
		for _, chunk := range after {
			if old[chunk.Key] {
				unchanged++
			}
		}
		if unchanged < len(before)-2 {
			t.Fatalf("%s changed too many chunks: kept %d/%d", name, unchanged, len(before))
		}
	}
}

type failingReader struct {
	r   *bytes.Reader
	err error
}

type repeatingReader struct{ remaining int64 }

func (r *repeatingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(p) > 32<<10 {
		panic("chunker requested an object-sized read")
	}
	n := min(int64(len(p)), r.remaining)
	clear(p[:int(n)])
	r.remaining -= n
	return int(n), nil
}

func (r failingReader) Read(p []byte) (int, error) {
	if r.r.Len() == 0 {
		return 0, r.err
	}
	return r.r.Read(p)
}

func TestChunkerCancellationErrorAndMax(t *testing.T) {
	params := ChunkingParams{Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10}
	for _, invalid := range []ChunkingParams{{Min: 0, Avg: 64 << 10, Max: 256 << 10}, {Min: 16 << 10, Avg: 100000, Max: 256 << 10}, {Min: 16 << 10, Avg: 64 << 10, Max: MaxChunkBytes + 1}} {
		if _, err := NewChunker(bytes.NewReader(nil), "default", invalid); err == nil {
			t.Fatalf("accepted invalid params %+v", invalid)
		}
	}
	if _, err := NewChunker(bytes.NewReader(nil), "Bad Domain", params); err == nil {
		t.Fatal("accepted invalid domain")
	}
	c, _ := NewChunker(bytes.NewReader([]byte("short")), "default", params)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	injected := errors.New("reader failed")
	c, _ = NewChunker(failingReader{bytes.NewReader([]byte("short")), injected}, "default", params)
	if _, err := c.Next(t.Context()); !errors.Is(err, injected) {
		t.Fatalf("partial read error: %v", err)
	}
	if _, err := c.Next(t.Context()); err != io.EOF {
		t.Fatalf("read continued after terminal error: %v", err)
	}
	zeros := make([]byte, 3*params.Max+1)
	chunks, output := collectChunks(t, bytes.NewReader(zeros), ChunkingParams{Min: params.Max, Avg: params.Max, Max: params.Max})
	if len(chunks) != 4 || !bytes.Equal(output, zeros) || len(chunks[3].Bytes) != 1 {
		t.Fatalf("forced max cuts: %v", signatures(chunks))
	}
}

func TestChunkerStreamsWithoutMaterializingObject(t *testing.T) {
	const size = 40 << 20
	params := ChunkingParams{Min: 256 << 10, Avg: 1 << 20, Max: 4 << 20}
	c, err := NewChunker(&repeatingReader{remaining: size}, "default", params)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for {
		chunk, err := c.Next(t.Context())
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(chunk.Bytes) > params.Max {
			t.Fatal("chunk exceeded memory bound")
		}
		total += int64(len(chunk.Bytes))
	}
	if total != size {
		t.Fatalf("streamed %d, want %d", total, size)
	}
}

func TestConcurrentBloomAndLRUCandidates(t *testing.T) {
	bloom, _ := NewBloom(1 << 16)
	lru, _ := NewCandidateLRU(32)
	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		wg.Go(func() {
			for i := 0; i < 200; i++ {
				row := lookupRow(t, "default", fmt.Sprintf("worker-%d-%d", worker, i))
				if err := bloom.Add(row.Key); err != nil {
					t.Error(err)
				}
				if err := lru.Put(row); err != nil {
					t.Error(err)
				}
				_, _ = bloom.MaybeContains(row.Key)
				_, _ = lru.Get(row.Key)
			}
		})
	}
	wg.Wait()
	if len(lru.entries) > 32 {
		t.Fatal("candidate cache exceeded bound")
	}
}

func FuzzChunkerRoundTrip(f *testing.F) {
	f.Add([]byte(""))
	f.Add(bytes.Repeat([]byte{0}, 300000))
	f.Add(stream("fuzz-seed", 130000))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		params := ChunkingParams{Min: 16 << 10, Avg: 64 << 10, Max: 256 << 10}
		chunks, restored := collectChunks(t, segmentedReader{bytes.NewReader(data), 17}, params)
		if !bytes.Equal(restored, data) {
			t.Fatal("chunking changed data")
		}
		for i, chunk := range chunks {
			if len(chunk.Bytes) > params.Max || i < len(chunks)-1 && len(chunk.Bytes) < params.Min {
				t.Fatal("chunk outside limits")
			}
		}
	})
}
