package object

import (
	"context"
	"github.com/KinasTomes/miles/internal/storage"
	"strings"
	"testing"
)

// Exercise the shared begin/publish path introduced by LIST/Delete integration:
// both operations must retain admission through EventBuilder and final commit.
func TestPutDeleteRetainProducerAdmission(t *testing.T) {
	f := newServiceFixture(t)
	admit := f.options.AdmitWriter
	var ops []string
	f.options.AdmitWriter = func(ctx context.Context, r WriterRequest) (AdmittedWriter, error) {
		a, err := admit(ctx, r)
		if err != nil {
			return a, err
		}
		a.Producer = &storage.MutationContext{HistoryID: strings.Repeat("a", 32), LogicalOpID: r.OpID, MutationID: r.OpID, Object: r.Object, Writer: a.Fence}
		ops = append(ops, r.OpID)
		return a, nil
	}
	f.options.Events = MetadataEvents{}
	var err error
	f.service, err = New(f.options)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	f.put(t, ctx, "producer-delete", []byte("retained admission"))
	if err = f.service.DeleteObject(ctx, DeleteRequest{Bucket: f.bucket, Key: "producer-delete", UserID: "owner", CredentialID: "owner-key"}); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 2 {
		t.Fatal(ops)
	}
	for i, op := range ops {
		result, err := f.metadata.GetMutation(ctx, op)
		if err != nil || result.Event.Entity != "Objects" || result.LogicalOpID != op || result.Event.Revision != uint64(i+1) {
			t.Fatal("missing authoritative producer", result, err)
		}
	}
	head, err := f.metadata.GetObject(ctx, storage.ObjectKey{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "producer-delete"})
	if err != nil || !head.Deleted || head.Revision != 2 {
		t.Fatal(head, err)
	}
}
