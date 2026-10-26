package dynamodb

import (
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func publishedHead() storage.ObjectHead {
	h := contracttest.NewFixture().Request("native-head", 0).Head
	h.Revision, h.PageCount, h.ChunkCount, h.PageIndex = 1, 1, 1, []uint64{0}
	return h
}
func TestLargestReferencePagesAndHeadIndexStayBounded(t *testing.T) {
	for _, kind := range []byte{manifest.ObjectPage, manifest.PartPage} {
		owner, domain := strings.Repeat("o", 64), strings.Repeat("d", 64)
		capacity, err := manifest.RefCapacity(owner, domain)
		if err != nil {
			t.Fatal(err)
		}
		p := storage.ManifestPage{Version: 1, ManifestID: owner, Domain: domain}
		for i := 0; i < capacity; i++ {
			p.Chunks = append(p.Chunks, storage.ChunkRef{Key: storage.ChunkKey{Domain: domain, Hash: strings.Repeat("a", 64)}, RawBytes: 1, LogicalOffset: int64(i)})
		}
		key, item, err := ReferencePageItem(kind, p)
		if err != nil || len(item) != 3 || key.Table == "" {
			t.Fatal(key, len(item), err)
		}
		size, err := ItemSizeUpperBound(item)
		if err != nil || size >= 400000 {
			t.Fatal(size, err)
		}
		body, err := CloneBinary(item["body"])
		if err != nil || len(body) > manifest.MaxPageBytes {
			t.Fatal(len(body), err)
		}
		if _, err := manifest.Decode(body, kind, owner, 0); err != nil {
			t.Fatal(err)
		}
	}
	h := publishedHead()
	h.PageCount, h.ChunkCount, h.Size = 1024, 1024, 1024
	h.PageIndex = make([]uint64, 1024)
	for i := range h.PageIndex {
		h.PageIndex[i] = uint64(i)
	}
	item, err := HeadItem(h)
	if err != nil {
		t.Fatal(err)
	}
	index, err := CloneBinary(item["page_index"])
	if err != nil || len(index) != 8192 {
		t.Fatal(len(index), err)
	}
	index[0] = 99
	if item["page_index"].(*types.AttributeValueMemberB).Value[0] != 0 {
		t.Fatal("returned binary aliases item")
	}
	h.Metadata = map[string]string{"unicode": strings.Repeat("đ", MaxItemBytes/2)}
	if _, err := HeadItem(h); err == nil {
		t.Fatal("oversized head metadata")
	}
}
func TestEnvelopeOutcomeAndReceiptSizes(t *testing.T) {
	h := publishedHead()
	_, outcome, err := OutcomeItem(strings.Repeat("1", 32), strings.Repeat("a", 64), storage.CommitResult{Head: h})
	if err != nil || ValidateItem(outcome) != nil {
		t.Fatal(err)
	}
	e := contracttest.NewFixture().Request("native-head", 0).Event
	if _, err := OutboxItem(e); err != nil {
		t.Fatal(err)
	}
	e.PayloadPointer = strings.Repeat("x", MaxItemBytes)
	if _, err := OutboxItem(e); err == nil {
		t.Fatal("oversized outbox envelope")
	}
	r := storage.CompletedReceipt{Version: 1, UploadID: "upload", BucketID: h.Object.BucketID, Key: h.Object.Key, Initiator: "owner", OpID: h.OpID, InputDigest: strings.Repeat("a", 64), ManifestID: h.ManifestID, ETag: h.ETag, ExpiresAt: time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)}
	if _, err := ReceiptAttributes(r); err != nil {
		t.Fatal(err)
	}
	r.ETag = strings.Repeat("x", MaxItemBytes)
	if _, err := ReceiptAttributes(r); err == nil {
		t.Fatal("oversized receipt")
	}
	h.PageIndex = nil
	if _, err := HeadItem(h); err == nil {
		t.Fatal("omitted persisted descriptor")
	}
	if _, err := CloneBinary(nil); err == nil {
		t.Fatal("missing binary treated as empty")
	}
}
