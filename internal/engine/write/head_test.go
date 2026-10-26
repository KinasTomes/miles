package write

import (
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestPreparedDescriptorBindsOwnedCandidateIndex(t *testing.T) {
	p := PreparedObject{ManifestID: "prepared", Domain: "default", Size: 4, PageCount: 1, ChunkCount: 1, PageIndex: []uint64{0}}
	h := storage.ObjectHead{Version: 1, Object: storage.ObjectKey{Bucket: "b", BucketID: "id", Key: "k"}, Domain: "default", ETag: "etag"}
	bound, err := p.BindHead(h)
	if err != nil || bound.ManifestID != p.ManifestID || bound.ETag != h.ETag {
		t.Fatal(bound, err)
	}
	bound.PageIndex[0] = 3
	if p.PageIndex[0] != 0 {
		t.Fatal("consumer mutated prepared descriptor")
	}
	h.Domain = "other"
	if _, err := p.BindHead(h); err == nil {
		t.Fatal("cross-domain binding")
	}
}
