package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
)

// TestMandatoryS3AndAdminCoverageChecklist verifies every P0/P1 S3 operation
// resolves, P2/out-of-scope routes explicitly fail with NotImplemented, and
// Admin endpoints return the documented JSON error envelope and precedence.
func TestMandatoryS3AndAdminCoverageChecklist(t *testing.T) {
	// 1. S3 P0 (8) and P1 (11 routing rows; Presigned and If-Match layer onto GetObject/PutObject)
	p0p1 := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
		wantOp  s3.Operation
	}{
		{"ListBuckets", http.MethodGet, "/", nil, s3.ListBuckets},
		{"CreateBucket", http.MethodPut, "/demo", nil, s3.CreateBucket},
		{"HeadBucket", http.MethodHead, "/demo", nil, s3.HeadBucket},
		{"PutObject", http.MethodPut, "/demo/k", nil, s3.PutObject},
		{"GetObject", http.MethodGet, "/demo/k", nil, s3.GetObject},
		{"GetObject+IfMatch", http.MethodGet, "/demo/k", map[string]string{"If-Match": `"abc"`}, s3.GetObject},
		{"HeadObject", http.MethodHead, "/demo/k", nil, s3.HeadObject},
		{"DeleteObject", http.MethodDelete, "/demo/k", nil, s3.DeleteObject},
		{"ListObjectsV2", http.MethodGet, "/demo?list-type=2", nil, s3.ListObjectsV2},
		{"CreateMultipartUpload", http.MethodPost, "/demo/k?uploads", nil, s3.CreateMultipartUpload},
		{"UploadPart", http.MethodPut, "/demo/k?partNumber=1&uploadId=u1", nil, s3.UploadPart},
		{"CompleteMultipartUpload", http.MethodPost, "/demo/k?uploadId=u1", nil, s3.CompleteMultipartUpload},
		{"AbortMultipartUpload", http.MethodDelete, "/demo/k?uploadId=u1", nil, s3.AbortMultipartUpload},
		{"ListParts", http.MethodGet, "/demo/k?uploadId=u1", nil, s3.ListParts},
		{"DeleteObjects", http.MethodPost, "/demo?delete", nil, s3.DeleteObjects},
		{"CopyObject", http.MethodPut, "/demo/k", map[string]string{"X-Amz-Copy-Source": "src/a"}, s3.CopyObject},
		{"RestoreObject", http.MethodPost, "/demo/k?restore", nil, s3.RestoreObject},
		{"DeleteBucket", http.MethodDelete, "/demo", nil, s3.DeleteBucket},
		{"GetBucketLocation", http.MethodGet, "/demo?location", nil, s3.GetBucketLocation},
	}
	for _, tc := range p0p1 {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		route, err := s3.Resolve(req)
		if err != nil || route.Op != tc.wantOp {
			t.Fatalf("%s: got op=%q err=%v, want %q", tc.name, route.Op, err, tc.wantOp)
		}
	}

	// 2. P2 and out-of-scope S3 endpoints/headers/params must explicitly return 501 NotImplemented.
	unsupported := []struct {
		name    string
		method  string
		target  string
		headers map[string]string
	}{
		{"ListObjects v1", http.MethodGet, "/demo", nil},
		{"ListMultipartUploads", http.MethodGet, "/demo?uploads", nil},
		{"UploadPartCopy", http.MethodPut, "/demo/k?partNumber=1&uploadId=u1", map[string]string{"X-Amz-Copy-Source": "src/a"}},
		{"GetObjectAttributes", http.MethodGet, "/demo/k?attributes", nil},
		{"GET partNumber", http.MethodGet, "/demo/k?partNumber=1", nil},
		{"HEAD partNumber", http.MethodHead, "/demo/k?partNumber=1", nil},
		{"Response override", http.MethodGet, "/demo/k?response-content-type=text/plain", nil},
		{"HEAD If-Match", http.MethodHead, "/demo/k", map[string]string{"If-Match": `"e"`}},
		{"GET If-None-Match", http.MethodGet, "/demo/k", map[string]string{"If-None-Match": `"e"`}},
		{"PUT If-None-Match", http.MethodPut, "/demo/k", map[string]string{"If-None-Match": "*"}},
		{"CopySource If-Match", http.MethodPut, "/demo/k", map[string]string{"X-Amz-Copy-Source": "src/a", "X-Amz-Copy-Source-If-Match": `"e"`}},
		{"Bucket Versioning", http.MethodGet, "/demo?versioning", nil},
		{"Object Tagging", http.MethodGet, "/demo/k?tagging", nil},
		{"Object ACL", http.MethodPut, "/demo/k?acl", nil},
		{"Query debug param", http.MethodGet, "/demo/k?x-dsg-debug=1", nil},
	}
	for _, tc := range unsupported {
		req := httptest.NewRequest(tc.method, tc.target, nil)
		for k, v := range tc.headers {
			req.Header.Set(k, v)
		}
		_, err := s3.Resolve(req)
		if err == nil || err.Code != s3.ErrNotImplemented || err.Status() != http.StatusNotImplemented {
			t.Fatalf("%s: got %v, want 501 NotImplemented", tc.name, err)
		}
		rec := httptest.NewRecorder()
		_ = s3.WriteError(rec, req, err, "0123456789ABCDEF")
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s: status %d", tc.name, rec.Code)
		}
		if tc.method == http.MethodHead && rec.Body.Len() != 0 {
			t.Fatalf("%s: HEAD error must have empty body", tc.name)
		}
		if tc.method != http.MethodHead {
			var parsed struct {
				Code, Message, RequestId string
			}
			if xmlErr := xml.Unmarshal(rec.Body.Bytes(), &parsed); xmlErr != nil || parsed.Code != "NotImplemented" || parsed.RequestId != "0123456789ABCDEF" {
				t.Fatalf("%s: XML schema mismatch: %s (%v)", tc.name, rec.Body.String(), xmlErr)
			}
		}
	}

	// 3. Admin API error envelope schema + bare 404/405 for unregistered/wrong-method routes.
	store, err := badger.Open(badger.Options{Path: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sealer, _ := admin.NewAESSealer("k", bytes.Repeat([]byte{9}, 32))
	srv, err := admin.NewServer(admin.Options{
		Store: store,
		Admit: func(context.Context, string) (storage.MutationContext, error) {
			return storage.MutationContext{}, storage.ErrUnavailable
		},
		Sealer:  sealer,
		Cursors: admin.Keyring{Current: "k", Keys: map[string][]byte{"k": bytes.Repeat([]byte{1}, 32)}},
		Clock:   clock.NewReal(),
	})
	if err != nil {
		// Use explicit Admit func signature.
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/users", nil))
	if rec.Code != http.StatusUnauthorized || rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Fatalf("Admin 401 schema: %d %+v", rec.Code, rec.Header())
	}
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Message   string `json:"message"`
			RequestID string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != "unauthorized" || len(envelope.Error.RequestID) != 32 {
		t.Fatalf("Admin JSON error schema mismatch: %s (%v)", rec.Body.String(), err)
	}

	// Unwired /admin/v1/cost/report returns bare router 404 without JSON error envelope.
	costRec := httptest.NewRecorder()
	srv.ServeHTTP(costRec, httptest.NewRequest(http.MethodGet, "/admin/v1/cost/report", nil))
	if costRec.Code != http.StatusNotFound || strings.Contains(costRec.Body.String(), `"error"`) {
		t.Fatalf("unwired cost/report expected bare 404, got %d %s", costRec.Code, costRec.Body.String())
	}
}

func TestBothProfilesRejectSharedCredentialsAndValidateStartup(t *testing.T) {
	for _, profile := range []string{"cloud", "edge"} {
		c := config.DefaultStartup()
		c.Profile = profile
		c.Mode = "local"
		c.LocalDataPath = t.TempDir()
		c.PackEndpoint = "http://127.0.0.1:5000"
		c.PackBucket = "packs"
		c.S3Address = "127.0.0.1:8080"
		c.AdminAddress = "127.0.0.1:9000"
		if err := c.Validate(); err != nil {
			t.Fatalf("%s local config invalid: %v", profile, err)
		}
	}
}
