package object

import (
	"context"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestSelectRangeUsesCommittedHeadAfterOverwrite(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	f.put(t, ctx, "file", []byte(strings.Repeat("a", 100)))
	old, err := f.service.Stat(ctx, GetRequest{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "file", CredentialID: "owner-key"})
	if err != nil || old.Info.Size != 100 {
		t.Fatal(old, err)
	}
	f.put(t, ctx, "file", []byte(strings.Repeat("b", 190)+"0123456789"))
	var body []byte
	result, err := f.service.GetObject(ctx, GetRequest{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "file", CredentialID: "owner-key",
		SelectRange: func(info Info) (*storage.ByteRange, error) {
			return &storage.ByteRange{Offset: info.Size - 10, Length: 10}, nil
		},
		Emit: func(_ context.Context, b []byte) error { body = append(body, b...); return nil },
	})
	if err != nil || result.Info.Size != 200 || result.Range.Offset != 190 || string(body) != "0123456789" {
		t.Fatal("selected stale head", result, string(body), err)
	}
}
