package manifest

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestHeadIndexEncodingAndBounds(t *testing.T) {
	h := storage.ObjectHead{Version: 1, Object: storage.ObjectKey{Bucket: "b", BucketID: "id", Key: "k"}, Domain: "default", ManifestID: "owner", Size: 16, PageCount: 2, ChunkCount: 2, PageIndex: []uint64{0, 8}}
	b, err := EncodePageIndex(h)
	want := make([]byte, 16)
	want[8] = 8
	if err != nil || !bytes.Equal(b, want) {
		t.Fatal(b, err)
	}
	index, err := DecodePageIndex(b, 2)
	if err != nil || !reflect.DeepEqual(index, h.PageIndex) {
		t.Fatal(index, err)
	}
	for _, idx := range [][]uint64{{1, 8}, {0, 0}, {0, 16}, {0}} {
		bad := h
		bad.PageIndex = idx
		if _, err := EncodePageIndex(bad); err == nil {
			t.Fatal("invalid index", idx)
		}
	}
	if _, err := DecodePageIndex(b[:15], 2); err == nil {
		t.Fatal("short index")
	}
	if _, err := DecodePageIndex(nil, MaxPages+1); err == nil {
		t.Fatal("oversized descriptor")
	}
	h.Size, h.PageCount, h.ChunkCount, h.PageIndex = 0, 0, 0, nil
	if _, err := EncodePageIndex(h); err != nil {
		t.Fatal("empty", err)
	}
	h.Deleted = true
	if h.ValidateManifestDescriptor() == nil {
		t.Fatal("tombstone retained manifest")
	}
	h.ManifestID = ""
	if err := h.ValidateManifestDescriptor(); err != nil {
		t.Fatal(err)
	}
}
