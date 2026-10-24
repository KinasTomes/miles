package manifest

import (
	"io"
	"reflect"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestStreamWriterMatchesGreedyPagination(t *testing.T) {
	owner := "0123456789abcdef0123456789abcdef"
	capacity, err := RefCapacity(owner, "default")
	if err != nil {
		t.Fatal(err)
	}
	refs := make([]storage.ChunkRef, capacity+1)
	for i := range refs {
		refs[i] = storage.ChunkRef{Key: storage.ChunkKey{Domain: "default", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			RawBytes: 16384, LogicalOffset: int64(i) * 16384}
	}
	var pushed []storage.ManifestPage
	w, err := NewStreamWriter(ObjectPage, owner, "default", func(page storage.ManifestPage, _ [32]byte) error {
		pushed = append(pushed, page)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if err := w.Add(ref); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	var pulled []storage.ManifestPage
	index := 0
	if err := Paginate(ObjectPage, owner, "default", func() (storage.ChunkRef, error) {
		if index == len(refs) {
			return storage.ChunkRef{}, io.EOF
		}
		ref := refs[index]
		index++
		return ref, nil
	}, func(page storage.ManifestPage) error {
		pulled = append(pulled, page)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	size, count, pages, pageIndex, digests, err := w.Descriptor()
	if err != nil || !reflect.DeepEqual(pushed, pulled) || size != int64(len(refs))*16384 || count != uint64(len(refs)) || pages != 2 ||
		len(digests) != 2 || !reflect.DeepEqual(pageIndex, []uint64{0, uint64(capacity * 16384)}) {
		t.Fatalf("stream descriptor mismatch: size=%d count=%d pages=%d index=%v err=%v", size, count, pages, pageIndex, err)
	}
}
