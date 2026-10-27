package recovery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type journalS3 struct {
	objects                   map[string][]byte
	last                      *awss3.PutObjectInput
	lostACK, denied, readFail bool
	class                     types.StorageClass
}

func (m *journalS3) PutObject(_ context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	m.last = in
	if m.denied {
		return nil, &smithy.GenericAPIError{Code: "AccessDenied"}
	}
	k := aws.ToString(in.Key)
	if _, ok := m.objects[k]; ok {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.objects[k] = b
	if m.lostACK {
		return nil, crash
	}
	return &awss3.PutObjectOutput{}, nil
}
func (m *journalS3) GetObject(_ context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	if m.readFail {
		return nil, crash
	}
	b, ok := m.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, s.ErrNotFound
	}
	return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(b)), StorageClass: m.class}, nil
}
func TestS3ConditionalStandardAndLostACK(t *testing.T) {
	ctx := context.Background()
	api := &journalS3{objects: map[string][]byte{}, lostACK: true}
	o, err := NewS3Objects(api, "journal-bucket", "test/", "kms-key")
	if err != nil {
		t.Fatal(err)
	}
	b := []byte(`{"r2_version":1}`)
	if err = o.Create(ctx, "journal/a.json", b); err != nil {
		t.Fatal("lost ACK reconciliation", err)
	}
	in := api.last
	if aws.ToString(in.IfNoneMatch) != "*" || in.StorageClass != types.StorageClassStandard || aws.ToString(in.Key) != "test/journal/a.json" || aws.ToString(in.Bucket) != "journal-bucket" || in.ServerSideEncryption != types.ServerSideEncryptionAwsKms || aws.ToString(in.SSEKMSKeyId) != "kms-key" || aws.ToInt64(in.ContentLength) != int64(len(b)) {
		t.Fatal("unsafe PUT options")
	}
	if err = o.Create(ctx, "journal/a.json", b); err != nil {
		t.Fatal("idempotency", err)
	}
	if err = o.Create(ctx, "journal/a.json", []byte("different")); !errors.Is(err, s.ErrConflict) {
		t.Fatal("collision overwritten", err)
	}
	if !bytes.Equal(api.objects["test/journal/a.json"], b) {
		t.Fatal("immutable object changed")
	}
	api.readFail = true
	if err = o.Create(ctx, "journal/b.json", b); err == nil {
		t.Fatal("unknown accepted")
	}
	api.readFail = false
	api.class = types.StorageClassGlacierIr
	if err = o.Create(ctx, "journal/a.json", b); err == nil {
		t.Fatal("cold journal accepted")
	}
	api.class = ""
	api.denied = true
	if err = o.Create(ctx, "journal/c.json", b); err == nil {
		t.Fatal("access denied accepted")
	}
}
