// Package s3 implements the physical PackStore boundary with AWS SDK for Go v2.
// R1 admission, GC safety and authoritative pack state remain caller-owned.
package s3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// API is deliberately narrow, so each S3 operation and failure can be tested
// without credentials or a live bucket. *s3.Client satisfies this interface.
type API interface {
	PutObject(context.Context, *awss3.PutObjectInput, ...func(*awss3.Options)) (*awss3.PutObjectOutput, error)
	HeadObject(context.Context, *awss3.HeadObjectInput, ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error)
	GetObject(context.Context, *awss3.GetObjectInput, ...func(*awss3.Options)) (*awss3.GetObjectOutput, error)
	CopyObject(context.Context, *awss3.CopyObjectInput, ...func(*awss3.Options)) (*awss3.CopyObjectOutput, error)
	RestoreObject(context.Context, *awss3.RestoreObjectInput, ...func(*awss3.Options)) (*awss3.RestoreObjectOutput, error)
	DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error)
}

type Store struct {
	client API
	bucket string
	kmsKey string
}

var _ storage.PackStore = (*Store)(nil)

// New receives an SDK client configured by the runtime. kmsKey may be empty
// when bucket-default encryption is enforced externally.
func New(client API, bucket, kmsKey string) (*Store, error) {
	if client == nil || bucket == "" {
		return nil, storage.ErrInvalid
	}
	return &Store{client: client, bucket: bucket, kmsKey: kmsKey}, nil
}

func keys(ref storage.PackRef) (string, string, error) {
	if len(ref.ID) < 2 || len(ref.ID) > 64 || len(ref.Generation) == 0 || len(ref.Generation) > 64 ||
		!validID(ref.ID) || !validID(ref.Generation) {
		return "", "", storage.ErrInvalid
	}
	prefix := ref.ID[:2]
	return "packs/" + prefix + "/" + ref.ID + "/" + ref.Generation,
		"packs-index/" + prefix + "/" + ref.ID + "/" + ref.Generation + ".idx", nil
}

func validID(s string) bool {
	for _, c := range s {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}

func (s *Store) PutPack(ctx context.Context, info storage.PackInfo, body io.Reader) error {
	key, _, err := keys(info.Ref)
	if err != nil || info.Version != storage.ModelVersion || info.Class != storage.Standard || info.State != "Warm" ||
		info.Size <= 0 || info.Size > pack.MaxPackBytes {
		return storage.ErrInvalid
	}
	return s.putImmutable(ctx, key, info.Size, body)
}

func (s *Store) PutSidecar(ctx context.Context, ref storage.PackRef, size int64, body io.Reader) error {
	_, key, err := keys(ref)
	if err != nil || size <= 0 || size > pack.MaxIndexBytes {
		return storage.ErrInvalid
	}
	if _, err := s.HeadPack(ctx, ref); err != nil {
		return err
	}
	return s.putImmutable(ctx, key, size, body)
}

func exactBody(ctx context.Context, size int64, body io.Reader) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if body == nil || size < 0 {
		return nil, storage.ErrInvalid
	}
	data, err := io.ReadAll(io.LimitReader(body, size+1))
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, storage.ErrInvalid
	}
	return data, nil
}

func (s *Store) putImmutable(ctx context.Context, key string, size int64, body io.Reader) error {
	data, err := exactBody(ctx, size, body)
	if err != nil {
		return err
	}
	in := &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(data),
		ContentLength: aws.Int64(size), IfNoneMatch: aws.String("*"), StorageClass: types.StorageClassStandard}
	if s.kmsKey != "" {
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(s.kmsKey)
	}
	_, err = s.client.PutObject(ctx, in)
	if err == nil {
		return nil
	}
	if code(err) != "PreconditionFailed" && code(err) != "ConditionalRequestConflict" {
		return mapError(err)
	}
	// A conditional collision or retry is successful only if the current
	// complete object is byte-identical. ETag is not a content proof for KMS or
	// multipart objects, so compare the bounded body itself.
	got, getErr := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if getErr != nil {
		return fmt.Errorf("%w: cannot reconcile immutable PUT: %v", storage.ErrUnavailable, getErr)
	}
	defer got.Body.Close()
	existing, getErr := io.ReadAll(io.LimitReader(got.Body, size+1))
	if getErr != nil {
		return fmt.Errorf("%w: cannot reconcile immutable PUT: %v", storage.ErrUnavailable, getErr)
	}
	if int64(len(existing)) != size || !bytes.Equal(existing, data) {
		return storage.ErrConflict
	}
	return nil
}

func (s *Store) HeadPack(ctx context.Context, ref storage.PackRef) (storage.PackInfo, error) {
	key, _, err := keys(ref)
	if err != nil {
		return storage.PackInfo{}, err
	}
	out, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return storage.PackInfo{}, mapError(err)
	}
	if out.ContentLength == nil || *out.ContentLength <= 0 || *out.ContentLength > pack.MaxPackBytes {
		return storage.PackInfo{}, storage.ErrUnavailable
	}
	class := storage.StorageClass(out.StorageClass)
	if class == "" {
		class = storage.Standard
	}
	if err := class.Validate(); err != nil {
		return storage.PackInfo{}, fmt.Errorf("%w: unsupported S3 storage class", storage.ErrUnavailable)
	}
	info := storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: *out.ContentLength, Class: class, State: "Warm"}
	if class == storage.Glacier {
		info.State = "Cold"
		if out.Restore != nil {
			if strings.Contains(*out.Restore, `ongoing-request="true"`) {
				info.State = "Restoring"
			} else if strings.Contains(*out.Restore, `ongoing-request="false"`) {
				if start := strings.Index(*out.Restore, `expiry-date="`); start >= 0 {
					rest := (*out.Restore)[start+len(`expiry-date="`):]
					if stop := strings.IndexByte(rest, '"'); stop >= 0 {
						if expiry, parseErr := time.Parse(time.RFC1123, rest[:stop]); parseErr == nil {
							info.RestoredUntil = expiry
							// The R1 caller compares expiry to its pinned Clock;
							// this adapter must not use a node-local wall clock.
							info.State = "Restored"
						}
					}
				}
			}
		}
	} else if class == storage.GlacierIR {
		info.State = "Cold" // instant retrieval is readable despite Cold state.
	}
	return info, nil
}

func (s *Store) RangeGet(ctx context.Context, ref storage.PackRef, span storage.ByteRange) (io.ReadCloser, error) {
	info, err := s.HeadPack(ctx, ref)
	if err != nil {
		return nil, err
	}
	if err := span.Validate(info.Size); err != nil {
		return nil, err
	}
	if info.Class == storage.Glacier && info.State != "Restored" {
		return nil, storage.ErrCold
	}
	key, _, _ := keys(ref)
	end := span.Offset + span.Length - 1
	want := fmt.Sprintf("bytes %d-%d/%d", span.Offset, end, info.Size)
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key),
		Range: aws.String(fmt.Sprintf("bytes=%d-%d", span.Offset, end))})
	if err != nil {
		return nil, mapError(err)
	}
	if out.Body == nil || out.ContentRange == nil || *out.ContentRange != want ||
		out.ContentLength == nil || *out.ContentLength != span.Length {
		if out.Body != nil {
			out.Body.Close()
		}
		return nil, fmt.Errorf("%w: S3 returned a different range", storage.ErrUnavailable)
	}
	return out.Body, nil
}

// ReadSidecar is the inventory/recovery extension, deliberately separate from
// PackStore. The caller validates these bytes with pack.ParseIndex.
func (s *Store) ReadSidecar(ctx context.Context, ref storage.PackRef) ([]byte, error) {
	_, key, err := keys(ref)
	if err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, mapError(err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, pack.MaxIndexBytes+1))
	if err != nil || len(data) == 0 || len(data) > pack.MaxIndexBytes {
		return nil, fmt.Errorf("%w: invalid sidecar read: %v", storage.ErrUnavailable, err)
	}
	return data, nil
}

// ReadVerifiedSidecar exposes physical inventory only. A canonical location
// still comes from metadata/journal, never from the sidecar by itself.
func (s *Store) ReadVerifiedSidecar(ctx context.Context, ref storage.PackRef) ([]pack.Entry, error) {
	data, err := s.ReadSidecar(ctx, ref)
	if err != nil {
		return nil, err
	}
	entries, _, err := pack.ParseIndex(data, ref)
	return entries, err
}

func (s *Store) ChangeStorageClass(ctx context.Context, ref storage.PackRef, next storage.StorageClass) error {
	if err := next.Validate(); err != nil {
		return err
	}
	info, err := s.HeadPack(ctx, ref)
	if err != nil {
		return err
	}
	if err := info.Class.ValidateTransition(next); err != nil {
		return err
	}
	if info.Class == next {
		return nil
	}
	key, _, _ := keys(ref)
	in := &awss3.CopyObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key),
		CopySource: aws.String(s.bucket + "/" + url.PathEscape(key)), StorageClass: types.StorageClass(next),
		MetadataDirective: types.MetadataDirectiveCopy}
	if s.kmsKey != "" {
		in.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		in.SSEKMSKeyId = aws.String(s.kmsKey)
	}
	_, err = s.client.CopyObject(ctx, in)
	return mapError(err)
}

func (s *Store) Restore(ctx context.Context, ref storage.PackRef, request storage.RestoreRequest) error {
	if request.Days <= 0 || request.Days > math.MaxInt32 ||
		request.Tier != "Standard" && request.Tier != "Bulk" && request.Tier != "Expedited" {
		return storage.ErrInvalid
	}
	info, err := s.HeadPack(ctx, ref)
	if err != nil {
		return err
	}
	if info.Class != storage.Glacier {
		return storage.ErrInvalid
	}
	key, _, _ := keys(ref)
	_, err = s.client.RestoreObject(ctx, &awss3.RestoreObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key),
		RestoreRequest: &types.RestoreRequest{Days: aws.Int32(int32(request.Days)), GlacierJobParameters: &types.GlacierJobParameters{Tier: types.Tier(request.Tier)}}})
	return mapError(err)
}

func (s *Store) DeletePack(ctx context.Context, ref storage.PackRef) error {
	key, _, err := keys(ref)
	if err != nil {
		return err
	}
	_, err = s.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	return mapError(err)
}

func code(err error) string {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode()
	}
	return ""
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	switch code(err) {
	case "NoSuchKey", "NotFound", "404":
		return fmt.Errorf("%w: %v", storage.ErrNotFound, err)
	case "InvalidObjectState":
		return fmt.Errorf("%w: %v", storage.ErrCold, err)
	case "AccessDenied":
		return fmt.Errorf("%w: %v", storage.ErrAccessDenied, err)
	case "PreconditionFailed":
		return fmt.Errorf("%w: %v", storage.ErrConflict, err)
	default:
		return fmt.Errorf("%w: %v", storage.ErrUnavailable, err)
	}
}

// RangeHeader is shared by controlled S3 test doubles.
func RangeHeader(offset, length, size int64) string {
	return "bytes " + strconv.FormatInt(offset, 10) + "-" + strconv.FormatInt(offset+length-1, 10) + "/" + strconv.FormatInt(size, 10)
}
