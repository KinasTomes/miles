package write

import (
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestOrderedRefsSpillsAndWaitsForCanonicalPublication(t *testing.T) {
	var result Result
	var offsets []int64
	q := orderedRefs{domain: "default", result: &result, emit: func(ref storage.ChunkRef) error {
		offsets = append(offsets, ref.LogicalOffset)
		return nil
	}}
	defer q.Close()
	key := storage.ChunkKey{Domain: "default", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	for i := 0; i < maxMemoryRefs+1; i++ {
		if err := q.Add(storage.ChunkRef{Key: key, RawBytes: 16 << 10, LogicalOffset: int64(i * (16 << 10))}, 1); err != nil {
			t.Fatal(err)
		}
	}
	if q.file == nil {
		t.Fatal("large pending queue did not spill")
	}
	if err := q.Drain(0); err != nil || result.ChunkCount != 0 {
		t.Fatalf("emitted before publication: %+v %v", result, err)
	}
	if err := q.Drain(1); err != nil {
		t.Fatal(err)
	}
	if result.ChunkCount != maxMemoryRefs+1 || len(offsets) != maxMemoryRefs+1 || q.read != 0 || q.write != 0 {
		t.Fatalf("incomplete queue drain: %+v refs=%d read=%d write=%d", result, len(offsets), q.read, q.write)
	}
	for i, offset := range offsets {
		if offset != int64(i*(16<<10)) {
			t.Fatalf("ref %d out of order: %d", i, offset)
		}
	}
}
