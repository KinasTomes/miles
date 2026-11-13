package multipart

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func TestUploadPartDebugHeadersGate(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	users := []storage.User{
		{ID: "owner", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "writer", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "admin-debug", Active: true, Revision: 1, DefaultDomain: "default"},
		{ID: "admin-nodebug", Active: true, Revision: 1, DefaultDomain: "default"},
	}
	creds := []storage.Credential{
		{ID: "OWNERKEY000000000001", UserID: "owner", Active: true, Revision: 1},
		{ID: "WRITERKEY00000000002", UserID: "writer", Active: true, Revision: 1},
		{ID: "ADMINDEBUG0000000003", UserID: "admin-debug", Active: true, SystemAdmin: true, DebugAllowed: true, Revision: 1},
		{ID: "ADMINNODEBUG00000004", UserID: "admin-nodebug", Active: true, SystemAdmin: true, DebugAllowed: false, Revision: 1},
	}
	bucket := storage.Bucket{Name: "demo", ID: "bkt-mp-debug", OwnerID: "owner", Domain: "default", Revision: 1, Active: true, CreatedAt: now}
	grants := []storage.Grant{
		{BucketID: bucket.ID, UserID: "writer", Write: true, Revision: 1},
		{BucketID: bucket.ID, UserID: "admin-debug", Write: true, Revision: 1},
		{BucketID: bucket.ID, UserID: "admin-nodebug", Write: true, Revision: 1},
	}
	meta := fake.NewMetadata(fake.Seed{Buckets: []storage.Bucket{bucket}, Users: users, Credentials: creds, Grants: grants})

	svc := &fakeDebugPartService{uploads: map[string]storage.MultipartUpload{
		"11111111111111111111111111111111": {ID: "11111111111111111111111111111111", State: "Active", Initiator: "admin-debug", Owner: "owner", Object: storage.ObjectKey{Bucket: "demo", BucketID: bucket.ID, Key: "k"}, ExpiresAt: now.Add(time.Hour), ChecksumAlgorithm: "CRC64NVME", ChecksumType: "FULL_OBJECT"},
		"22222222222222222222222222222222": {ID: "22222222222222222222222222222222", State: "Active", Initiator: "writer", Owner: "owner", Object: storage.ObjectKey{Bucket: "demo", BucketID: bucket.ID, Key: "k"}, ExpiresAt: now.Add(time.Hour), ChecksumAlgorithm: "CRC64NVME", ChecksumType: "FULL_OBJECT"},
		"33333333333333333333333333333333": {ID: "33333333333333333333333333333333", State: "Active", Initiator: "admin-nodebug", Owner: "owner", Object: storage.ObjectKey{Bucket: "demo", BucketID: bucket.ID, Key: "k"}, ExpiresAt: now.Add(time.Hour), ChecksumAlgorithm: "CRC64NVME", ChecksumType: "FULL_OBJECT"},
	}}
	credMap := credentials{}
	for _, c := range creds {
		credMap[c.ID] = s3.Credential{UserID: c.UserID, Secret: "secret-" + c.UserID, Active: true, SystemAdmin: c.SystemAdmin, DebugAllowed: c.DebugAllowed}
	}
	rt := &s3.Router{Region: region, Credentials: credMap, Access: meta, Now: func() time.Time { return now }, Handlers: NewHandlers(svc, func() time.Time { return now })}

	send := func(credID, uploadID string, signedDebug, unsignedDebug bool) *httptest.ResponseRecorder {
		body := []byte("multipart-part-body")
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])
		req := httptest.NewRequest(http.MethodPut, "http://s3.test/demo/k?partNumber=1&uploadId="+uploadID, bytes.NewReader(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
		req.Header.Set("X-Amz-Content-Sha256", hash)
		if signedDebug {
			req.Header.Set("X-Dsg-Debug", "1")
		}
		if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: credID, SecretAccessKey: credMap[credID].Secret}, req, hash, "s3", region, now); err != nil {
			t.Fatal(err)
		}
		if unsignedDebug {
			req.Header.Set("X-Dsg-Debug", "1")
		}
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, req)
		return rec
	}

	// Authorized system_admin + debug_allowed + initiator receives all 5 physical debug headers.
	okRec := send("ADMINDEBUG0000000003", "11111111111111111111111111111111", true, false)
	if okRec.Code != http.StatusOK ||
		okRec.Header().Get("X-Dsg-Chunks-Total") != "3" ||
		okRec.Header().Get("X-Dsg-Chunks-Dup") != "1" ||
		okRec.Header().Get("X-Dsg-Chunks-Revived") != "1" ||
		okRec.Header().Get("X-Dsg-Bytes-Logical") != "19" ||
		okRec.Header().Get("X-Dsg-Bytes-Stored") != "1024" {
		t.Fatalf("admin debug UploadPart: %d %s %+v", okRec.Code, okRec.Body.String(), okRec.Header())
	}

	// Regular user, missing debug_allowed, unsigned header, and non-initiator never see x-dsg-*.
	for _, tc := range []struct {
		name, cred, uploadID string
		signed, unsigned     bool
		status               int
	}{
		{"regular writer with signed debug", "WRITERKEY00000000002", "22222222222222222222222222222222", true, false, http.StatusOK},
		{"admin without debug_allowed", "ADMINNODEBUG00000004", "33333333333333333333333333333333", true, false, http.StatusOK},
		{"admin with unsigned debug", "ADMINDEBUG0000000003", "11111111111111111111111111111111", false, true, http.StatusOK},
		{"admin debug non-initiator denied", "ADMINDEBUG0000000003", "22222222222222222222222222222222", true, false, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := send(tc.cred, tc.uploadID, tc.signed, tc.unsigned)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			for _, h := range []string{"X-Dsg-Chunks-Total", "X-Dsg-Chunks-Dup", "X-Dsg-Chunks-Revived", "X-Dsg-Bytes-Logical", "X-Dsg-Bytes-Stored"} {
				if rec.Header().Get(h) != "" {
					t.Fatalf("unexpected debug header %s=%q", h, rec.Header().Get(h))
				}
			}
		})
	}
}

type fakeDebugPartService struct {
	fakeService
	uploads map[string]storage.MultipartUpload
}

func (f *fakeDebugPartService) GetUpload(_ context.Context, id string) (storage.MultipartUpload, error) {
	u, ok := f.uploads[id]
	if !ok {
		return storage.MultipartUpload{}, storage.ErrNotFound
	}
	return u, nil
}

func (f *fakeDebugPartService) UploadPart(_ context.Context, r object.PartRequest) (object.PartResult, error) {
	return object.PartResult{
		ETag: "0123456789abcdef0123456789abcdef", ChecksumAlgorithm: "CRC64NVME", ChecksumValue: "AAAAAAAAAAA=",
		Debug: object.WriteDebug{ChunksTotal: 3, ChunksDup: 1, ChunksRevived: 1, BytesLogical: r.ContentLength, BytesStored: 1024},
	}, nil
}
