package recovery

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type S3API interface {
	PutObject(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
}

// S3Objects is a journal sink, separate from the physical pack format. Its
// bucket/prefix must be restricted by IAM; catalog provisioning is separate.
type S3Objects struct {
	client                 S3API
	bucket, prefix, kmsKey string
}

func NewS3Objects(client S3API, bucket, prefix, kmsKey string) (*S3Objects, error) {
	if client == nil || bucket == "" || strings.Contains(prefix, "..") || strings.HasPrefix(prefix, "/") || prefix != "" && !strings.HasSuffix(prefix, "/") {
		return nil, s.ErrInvalid
	}
	return &S3Objects{client, bucket, prefix, kmsKey}, nil
}
func (o *S3Objects) Read(ctx context.Context, key string) ([]byte, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return nil, s.ErrInvalid
	}
	got, err := o.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(o.bucket), Key: aws.String(o.prefix + key)})
	if err != nil {
		return nil, err
	}
	if got == nil || got.Body == nil {
		return nil, s.ErrUnavailable
	}
	defer got.Body.Close()
	if got.StorageClass != "" && got.StorageClass != types.StorageClassStandard {
		return nil, s.ErrInvalid
	}
	b, err := io.ReadAll(io.LimitReader(got.Body, MaxMetadataBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 || len(b) > MaxMetadataBytes {
		return nil, s.ErrInvalid
	}
	return b, nil
}
func (o *S3Objects) Create(ctx context.Context, key string, b []byte) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") || len(b) == 0 || len(b) > MaxMetadataBytes {
		return s.ErrInvalid
	}
	in := &awss3.PutObjectInput{Bucket: aws.String(o.bucket), Key: aws.String(o.prefix + key), Body: bytes.NewReader(b), ContentLength: aws.Int64(int64(len(b))), IfNoneMatch: aws.String("*"), StorageClass: types.StorageClassStandard}
	if o.kmsKey != "" {
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(o.kmsKey)
	}
	_, err := o.client.PutObject(ctx, in)
	if err != nil {
		var api smithy.APIError
		// A transport error may be a lost ACK. Readback is safe for every error;
		// do not overwrite or infer no-write from a timeout.
		if errors.As(err, &api) && api.ErrorCode() == "AccessDenied" {
			return err
		}
	}
	got, readErr := o.Read(ctx, key)
	if readErr != nil {
		if err != nil {
			return err
		}
		return readErr
	}
	if !bytes.Equal(got, b) {
		return s.ErrConflict
	}
	return nil
}
