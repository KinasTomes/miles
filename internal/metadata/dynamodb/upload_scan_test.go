package dynamodb

import (
	"context"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type uploadScanFixture struct {
	API
	t     *testing.T
	calls int
}

func (a *uploadScanFixture) Scan(_ context.Context, in *ddb.ScanInput, _ ...func(*ddb.Options)) (*ddb.ScanOutput, error) {
	a.calls++
	if !aws.ToBool(in.ConsistentRead) || aws.ToInt32(in.Limit) != 1 || aws.ToString(in.FilterExpression) != "#u.#state IN (:active,:completing,:aborting)" {
		a.t.Fatal(in)
	}
	id := "0123456789abcdef0123456789abcdef"
	if a.calls == 1 {
		return &ddb.ScanOutput{LastEvaluatedKey: Item{"upload_id": text(id)}}, nil
	}
	if in.ExclusiveStartKey["upload_id"] == nil {
		a.t.Fatal("lost empty-page continuation")
	}
	u := storage.MultipartUpload{Version: 1, ID: id, State: "Active", Revision: 1, ExpiresAt: time.Now().Add(-time.Hour), Object: storage.ObjectKey{Bucket: "demo", BucketID: "bucket-id", Key: "k"}}
	item, e := nativeItem(u)
	if e != nil {
		a.t.Fatal(e)
	}
	return &ddb.ScanOutput{Items: []Item{{"upload_id": text(id), "upload-record": &types.AttributeValueMemberM{Value: item}}}}, nil
}
func TestUploadCandidateScanPreservesEmptyNativePages(t *testing.T) {
	api := &uploadScanFixture{t: t}
	m := &Store{api: api, tables: map[string]string{"Uploads": "fixture"}}
	u, next, e := m.ScanUploadCandidate(context.Background(), "")
	if e != nil || u != nil || next == "" {
		t.Fatal(u, next, e)
	}
	u, next, e = m.ScanUploadCandidate(context.Background(), next)
	if e != nil || u == nil || u.State != "Active" || next != "" {
		t.Fatal(u, next, e)
	}
	if _, _, e = m.ScanUploadCandidate(context.Background(), "not-a-native-key"); e != storage.ErrInvalid {
		t.Fatal(e)
	}
}
