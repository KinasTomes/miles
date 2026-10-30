package multipart

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	crc32pkg "hash/crc32"
	crc64pkg "hash/crc64"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/KinasTomes/miles/internal/storage/fake"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

const region = "ap-southeast-1"

type credentials map[string]s3.Credential

func (c credentials) Credential(_ context.Context, id string) (s3.Credential, error) {
	if v, ok := c[id]; ok {
		return v, nil
	}
	return s3.Credential{}, storage.ErrNotFound
}

// fakeService records what the handlers pass on; uploads are keyed by ID.
type fakeService struct {
	mu        sync.Mutex
	uploads   map[string]object.Upload
	created   object.CreateUploadRequest
	part      object.PartRequest
	partBody  []byte
	partErr   error
	onPartErr func() // the state that won against a failed publish
	listed    object.ListPartsRequest
	parts     storage.PartListPage
	aborted   int
}

func (f *fakeService) CreateUpload(_ context.Context, r object.CreateUploadRequest) (storage.MultipartUpload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = r
	id := strings.Repeat("c", 32)
	f.uploads[id] = object.Upload{MultipartUpload: storage.MultipartUpload{ID: id, State: "Active", Initiator: "writer", Owner: r.Bucket.OwnerID,
		Object: storage.ObjectKey{Bucket: r.Bucket.Name, BucketID: r.Bucket.ID, Key: r.Key}, ExpiresAt: time.Now().Add(24 * time.Hour),
		ChecksumAlgorithm: r.ChecksumAlgorithm, ChecksumType: r.ChecksumType}}
	return f.uploads[id].MultipartUpload, nil
}

func (f *fakeService) GetUpload(_ context.Context, id string) (object.Upload, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.uploads[id]
	if !ok {
		return object.Upload{}, storage.ErrNotFound
	}
	return u, nil
}

func (f *fakeService) UploadPart(_ context.Context, r object.PartRequest) (object.PartResult, error) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return object.PartResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.part, f.partBody = r, data
	if f.partErr != nil {
		f.onPartErr()
		return object.PartResult{}, f.partErr
	}
	algorithm, digest := r.Body.Checksum()
	return object.PartResult{ETag: "0123456789abcdef0123456789abcdef", ChecksumAlgorithm: algorithm, ChecksumValue: base64.StdEncoding.EncodeToString(digest)}, nil
}

func (f *fakeService) ListParts(_ context.Context, r object.ListPartsRequest) (storage.PartListPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listed = r
	return f.parts, nil
}

func (f *fakeService) AbortUpload(context.Context, object.AbortRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.aborted++
	return nil
}

func (f *fakeService) set(u object.Upload) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[u.ID] = u
}

type world struct {
	svc     *fakeService
	now     time.Time
	clients map[string]*awss3.Client
}

func newWorld(t *testing.T) *world {
	t.Helper()
	f := contracttest.NewFixture()
	meta := fake.NewMetadata(fake.Seed{Buckets: []storage.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []storage.Grant{f.Grant}, Quotas: []storage.LogicalQuota{f.Quota}})
	w := &world{svc: &fakeService{uploads: map[string]object.Upload{}}, now: time.Now(), clients: map[string]*awss3.Client{}}
	creds := credentials{}
	for _, c := range f.Credentials {
		creds[c.ID] = s3.Credential{UserID: c.UserID, Secret: "secret-" + c.ID, Active: true, SystemAdmin: c.SystemAdmin}
	}
	rt := &s3.Router{Region: region, Credentials: creds, Access: meta, Now: time.Now, Handlers: NewHandlers(w.svc, func() time.Time { return w.now })}
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	for id, c := range creds {
		w.clients[id] = awss3.New(awss3.Options{Region: region, BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
			RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, RetryMaxAttempts: 1,
			Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
				return aws.Credentials{AccessKeyID: id, SecretAccessKey: c.Secret}, nil
			})})
	}
	return w
}

// upload seeds an Active upload of demo/key started by writer.
func (w *world) upload(state string) object.Upload {
	u := object.Upload{MultipartUpload: storage.MultipartUpload{ID: strings.Repeat("a", 32), State: state, Initiator: "writer", Owner: "owner",
		Object: storage.ObjectKey{Bucket: "demo", BucketID: "bucket-1", Key: "key"}, ExpiresAt: w.now.Add(time.Hour),
		ChecksumAlgorithm: "CRC64NVME", ChecksumType: "FULL_OBJECT"}}
	w.svc.set(u)
	return u
}

func code(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func uploadPart(c *awss3.Client, id, key string, n int32, body []byte, alg types.ChecksumAlgorithm) (*awss3.UploadPartOutput, error) {
	return c.UploadPart(context.Background(), &awss3.UploadPartInput{Bucket: aws.String("demo"), Key: aws.String(key), UploadId: aws.String(id),
		PartNumber: aws.Int32(n), Body: bytes.NewReader(body), ContentLength: aws.Int64(int64(len(body))), ChecksumAlgorithm: alg})
}

func TestSDKCreatePartListAbort(t *testing.T) {
	w := newWorld(t)
	writer := w.clients["writer-key"]
	ctx := context.Background()
	created, err := writer.CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String("demo"), Key: aws.String("big"),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256, ContentType: aws.String("text/plain"), Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatal(err)
	}
	c := w.svc.created
	if aws.ToString(created.UploadId) != strings.Repeat("c", 32) || created.ChecksumAlgorithm != types.ChecksumAlgorithmSha256 ||
		c.ChecksumAlgorithm != "SHA256" || c.ChecksumType != "COMPOSITE" || c.Metadata["content-type"] != "text/plain" || c.Metadata["x-amz-meta-k"] != "v" || c.CredentialID != "writer-key" {
		t.Fatalf("create %+v, service got %+v", created, c)
	}
	id := aws.ToString(created.UploadId)

	data := []byte("part payload")
	if _, err := uploadPart(writer, id, "big", 1, data, types.ChecksumAlgorithmCrc32); code(err) != "InvalidRequest" {
		t.Fatal("part checksum of another algorithm than the upload:", err)
	}
	sum := sha256.Sum256(data)
	want := base64.StdEncoding.EncodeToString(sum[:])
	for _, alg := range []types.ChecksumAlgorithm{types.ChecksumAlgorithmSha256, ""} { // named, then computed by the server
		out, err := uploadPart(writer, id, "big", 2, data, alg)
		if err != nil {
			t.Fatal(alg, err)
		}
		if aws.ToString(out.ETag) != `"0123456789abcdef0123456789abcdef"` || aws.ToString(out.ChecksumSHA256) != want ||
			w.svc.part.Number != 2 || w.svc.part.ContentLength != int64(len(data)) || !bytes.Equal(w.svc.partBody, data) {
			t.Fatalf("%q: part %+v service %+v", alg, out, w.svc.part)
		}
	}

	w.svc.parts = storage.PartListPage{Parts: []storage.UploadPart{
		{Number: 2, Size: 12, ETag: "0123456789abcdef0123456789abcdef", Checksum: storage.Checksum{Algorithm: "SHA256", Type: "COMPOSITE", Value: want}},
		{Number: 5, Size: 7, ETag: "fedcba9876543210fedcba9876543210", Checksum: storage.Checksum{Algorithm: "SHA256", Type: "COMPOSITE", Value: want}},
	}, Next: 5}
	owner := w.clients["owner-key"] // owner may list and abort another user's upload
	list, err := owner.ListParts(ctx, &awss3.ListPartsInput{Bucket: aws.String("demo"), Key: aws.String("big"), UploadId: aws.String(id), MaxParts: aws.Int32(2), PartNumberMarker: aws.String("1")})
	if err != nil {
		t.Fatal(err)
	}
	if w.svc.listed.After != 1 || w.svc.listed.Limit != 2 || !aws.ToBool(list.IsTruncated) || aws.ToString(list.NextPartNumberMarker) != "5" ||
		len(list.Parts) != 2 || aws.ToInt32(list.Parts[1].PartNumber) != 5 || aws.ToString(list.Parts[0].ETag) != `"0123456789abcdef0123456789abcdef"` ||
		aws.ToInt64(list.Parts[0].Size) != 12 || aws.ToString(list.Parts[0].ChecksumSHA256) != want || aws.ToString(list.Initiator.ID) != "writer" ||
		aws.ToString(list.Owner.ID) != "owner" || list.ChecksumAlgorithm != types.ChecksumAlgorithmSha256 || list.ChecksumType != types.ChecksumTypeComposite {
		t.Fatalf("list %+v", list)
	}
	if _, err := owner.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{Bucket: aws.String("demo"), Key: aws.String("big"), UploadId: aws.String(id)}); err != nil || w.svc.aborted != 1 {
		t.Fatal(err, w.svc.aborted)
	}
}

func TestBindingRejectsBeforeState(t *testing.T) {
	w := newWorld(t)
	u := w.upload("Active")
	other := u
	other.ID, other.Object.Key = strings.Repeat("b", 32), "other"
	w.svc.set(other)
	gone := u
	gone.ID, gone.Object.BucketID = strings.Repeat("d", 32), "old-incarnation"
	w.svc.set(gone)
	for name, tc := range map[string]struct {
		key, id, want string
	}{
		"unknown id":           {"key", strings.Repeat("e", 32), "NoSuchUpload"},
		"malformed id":         {"key", "../" + u.ID, "NoSuchUpload"},
		"other key":            {"key", other.ID, "NoSuchUpload"},
		"other incarnation":    {"key", gone.ID, "NoSuchUpload"},
		"owner, not initiator": {"key", u.ID, "AccessDenied"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := uploadPart(w.clients["owner-key"], tc.id, tc.key, 1, []byte("x"), "")
			if code(err) != tc.want {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
	// The system admin may abort another user's upload, never upload to it.
	if _, err := uploadPart(w.clients["admin-key"], u.ID, "key", 1, []byte("x"), ""); code(err) != "AccessDenied" {
		t.Fatal(err)
	}
	if w.svc.part.Number != 0 {
		t.Fatal("a rejected binding reached the service")
	}
}

func TestStateTable(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, state       string
		expired, receipt  bool
		part, list, abort string
	}{
		{name: "active", state: "Active", part: "", list: "", abort: ""},
		{name: "active expired", state: "Active", expired: true, part: "NoSuchUpload", list: "NoSuchUpload", abort: "NoSuchUpload"},
		{name: "completing", state: "Completing", part: "OperationAborted", list: "", abort: "OperationAborted"},
		{name: "completed", state: "Completed", receipt: true, part: "NoSuchUpload", list: "NoSuchUpload", abort: "OperationAborted"},
		{name: "completed receipt expired", state: "Completed", part: "NoSuchUpload", list: "NoSuchUpload", abort: "NoSuchUpload"},
		{name: "aborting", state: "Aborting", part: "NoSuchUpload", list: "NoSuchUpload", abort: "NoSuchUpload"},
		{name: "aborted", state: "Aborted", part: "NoSuchUpload", list: "NoSuchUpload", abort: "NoSuchUpload"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			u := w.upload(tc.state)
			u.LeaseUntil = w.now.Add(time.Minute)
			if tc.receipt {
				u.ReceiptExpiresAt = w.now.Add(time.Hour)
			}
			w.svc.set(u)
			if tc.expired {
				w.now = u.ExpiresAt
			}
			c := w.clients["writer-key"]
			_, err := uploadPart(c, u.ID, "key", 1, []byte("x"), "")
			_, listErr := w.clients["owner-key"].ListParts(ctx, &awss3.ListPartsInput{Bucket: aws.String("demo"), Key: aws.String("key"), UploadId: aws.String(u.ID)})
			_, abortErr := c.AbortMultipartUpload(ctx, &awss3.AbortMultipartUploadInput{Bucket: aws.String("demo"), Key: aws.String("key"), UploadId: aws.String(u.ID)})
			if code(err) != tc.part || code(listErr) != tc.list || code(abortErr) != tc.abort {
				t.Fatalf("UploadPart %v, ListParts %v, Abort %v", err, listErr, abortErr)
			}
		})
	}
}

// A publish that loses to a freeze or an abort answers from the state that won.
func TestLostPublishAnswersWinningState(t *testing.T) {
	for state, want := range map[string]string{"Completing": "OperationAborted", "Aborting": "NoSuchUpload", "Active": "ServiceUnavailable"} {
		t.Run(state, func(t *testing.T) {
			w := newWorld(t)
			u := w.upload("Active")
			w.svc.partErr = storage.ErrFenced
			w.svc.onPartErr = func() {
				won := u
				won.State, won.LeaseUntil = state, w.now.Add(time.Minute)
				w.svc.uploads[u.ID] = won
			}
			if _, err := uploadPart(w.clients["writer-key"], u.ID, "key", 1, []byte("x"), ""); code(err) != want {
				t.Fatalf("got %v, want %s", err, want)
			}
		})
	}
}

// A part naming another algorithm than the default CRC64NVME upload is
// verified with it and stored with the server's CRC64NVME (boto3 without
// awscrt sends CRC32 and cannot send CRC64NVME).
func TestDefaultUploadStoresCRC64NVMEOfAnyPartChecksum(t *testing.T) {
	w := newWorld(t)
	u := w.upload("Active")
	data := []byte("boto3 part")
	out, err := uploadPart(w.clients["writer-key"], u.ID, "key", 1, data, types.ChecksumAlgorithmCrc32)
	if err != nil {
		t.Fatal(err)
	}
	crc32, crc64 := make([]byte, 4), make([]byte, 8)
	binary.BigEndian.PutUint32(crc32, crc32pkg.ChecksumIEEE(data))
	binary.BigEndian.PutUint64(crc64, crc64pkg.Checksum(data, crc64pkg.MakeTable(0x9a6c9329ac4bc9b5)))
	algorithm, stored := w.svc.part.Body.Checksum()
	if aws.ToString(out.ChecksumCRC32) != base64.StdEncoding.EncodeToString(crc32) || algorithm != "CRC64NVME" || !bytes.Equal(stored, crc64) {
		t.Fatalf("response %+v, stored %s %x", out, algorithm, stored)
	}
	u.ChecksumAlgorithm, u.ChecksumType = "CRC32C", "COMPOSITE"
	w.svc.set(u)
	if _, err := uploadPart(w.clients["writer-key"], u.ID, "key", 1, data, types.ChecksumAlgorithmCrc32); code(err) != "InvalidRequest" {
		t.Fatal("an explicit algorithm must match:", err)
	}
}

func TestCreateChecksumDefaults(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	for _, tc := range []struct {
		alg       types.ChecksumAlgorithm
		kind      types.ChecksumType
		wantAlg   string
		wantKind  string
		wantError string
	}{
		{"", "", "CRC64NVME", "FULL_OBJECT", ""},
		{types.ChecksumAlgorithmCrc64nvme, "", "CRC64NVME", "FULL_OBJECT", ""},
		{types.ChecksumAlgorithmCrc32c, "", "CRC32C", "COMPOSITE", ""},
		{types.ChecksumAlgorithmCrc32, types.ChecksumTypeFullObject, "CRC32", "FULL_OBJECT", ""},
		{types.ChecksumAlgorithmSha1, types.ChecksumTypeFullObject, "", "", "InvalidRequest"},
	} {
		w.svc.created = object.CreateUploadRequest{}
		_, err := w.clients["writer-key"].CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String("demo"), Key: aws.String("k"), ChecksumAlgorithm: tc.alg, ChecksumType: tc.kind})
		if code(err) != tc.wantError || w.svc.created.ChecksumAlgorithm != tc.wantAlg || w.svc.created.ChecksumType != tc.wantKind {
			t.Fatalf("%s/%s: %v, service got %s/%s", tc.alg, tc.kind, err, w.svc.created.ChecksumAlgorithm, w.svc.created.ChecksumType)
		}
	}
	if _, err := w.clients["writer-key"].CreateMultipartUpload(ctx, &awss3.CreateMultipartUploadInput{Bucket: aws.String("demo"), Key: aws.String(strings.Repeat("k", 1025))}); code(err) != "KeyTooLongError" {
		t.Fatal(err)
	}
}
