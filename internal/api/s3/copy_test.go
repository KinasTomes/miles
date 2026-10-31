package s3

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

// sendHeaders is bucketWorld.send with extra request headers, signed like
// every other x-amz-* header.
func (w *bucketWorld) sendHeaders(t *testing.T, method, path, principal, body string, headers map[string]string) (*httptest.ResponseRecorder, *Error) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	sum := sha256.Sum256([]byte(body))
	signPayload(r, accessKey(principal), "secret-"+principal, hex.EncodeToString(sum[:]))
	rec := httptest.NewRecorder()
	err := w.router.serve(rec, r, "SENDTEST")
	var e *Error
	if err != nil && !errors.As(err, &e) {
		t.Fatalf("%s %s by %s: %v", method, path, principal, err)
	}
	return rec, e
}

func newCopyWorld(t *testing.T) (*bucketWorld, *fakeObjectWriter) {
	t.Helper()
	w := newBucketWorld(t)
	writer := &fakeObjectWriter{result: object.PutResult{ETag: `"source-etag"`}}
	w.router.Handlers = NewObjectHandlers(writer, nil, nil)
	return w, writer
}

func TestCopyObjectSourceAndDirective(t *testing.T) {
	cases := []struct {
		name      string
		headers   map[string]string
		code      ErrorCode
		srcKey    string
		replaced  map[string]string
		algorithm string
	}{
		{name: "leading slash, key decoded once", headers: map[string]string{"X-Amz-Copy-Source": "/demo/a%2Bb%2520c"}, srcKey: "a+b%20c"},
		{name: "COPY is the default and keeps source metadata", headers: map[string]string{"X-Amz-Copy-Source": "demo/a", "X-Amz-Metadata-Directive": "COPY", "Content-Type": "text/csv"}, srcKey: "a"},
		{name: "REPLACE takes the request headers", headers: map[string]string{"X-Amz-Copy-Source": "demo/a", "X-Amz-Metadata-Directive": "REPLACE", "X-Amz-Meta-Tag": "v2"}, srcKey: "a",
			replaced: map[string]string{"content-type": "binary/octet-stream", "x-amz-meta-tag": "v2"}},
		{name: "checksum algorithm is passed for the source check", headers: map[string]string{"X-Amz-Copy-Source": "demo/a", "X-Amz-Checksum-Algorithm": "crc32"}, srcKey: "a", algorithm: "CRC32"},
		{name: "unknown directive", headers: map[string]string{"X-Amz-Copy-Source": "demo/a", "X-Amz-Metadata-Directive": "MERGE"}, code: ErrInvalidArgument},
		{name: "source without key", headers: map[string]string{"X-Amz-Copy-Source": "demo/"}, code: ErrInvalidArgument},
		{name: "source with a version", headers: map[string]string{"X-Amz-Copy-Source": "demo/a?versionId=1"}, code: ErrNotImplemented},
		{name: "bad encoding", headers: map[string]string{"X-Amz-Copy-Source": "demo/%zz"}, code: ErrInvalidArgument},
		{name: "missing source bucket", headers: map[string]string{"X-Amz-Copy-Source": "nope/a"}, code: ErrNoSuchBucket},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, writer := newCopyWorld(t)
			rec, e := w.sendHeaders(t, http.MethodPut, "/demo/dst", "owner", "", tc.headers)
			if tc.code != "" {
				if e == nil || e.Code != tc.code || writer.copied.Key != "" {
					t.Fatalf("error %v, want %s; writer saw %+v", e, tc.code, writer.copied)
				}
				return
			}
			if e != nil || rec.Code != http.StatusOK {
				t.Fatalf("status %d error %v", rec.Code, e)
			}
			got := writer.copied
			if got.Source.Name != "demo" || got.Source.ID != "bkt-demo-0001" || got.SourceKey != tc.srcKey || got.Bucket.ID != "bkt-demo-0001" || got.Key != "dst" ||
				got.UserID != "user-owner" || got.CredentialID != accessKey("owner") || got.ChecksumAlgorithm != tc.algorithm {
				t.Fatalf("copy request %+v", got)
			}
			if tc.replaced == nil && got.Metadata != nil || tc.replaced != nil && !equalMaps(got.Metadata, tc.replaced) {
				t.Fatalf("metadata %v, want %v", got.Metadata, tc.replaced)
			}
			var out copyObjectResult
			if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.ETag != `"source-etag"` || out.LastModified == "" {
				t.Fatalf("CopyObjectResult %q: %v", rec.Body.String(), err)
			}
		})
	}
}

func equalMaps(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// Copy needs write on the destination (router) and read on the source
// (handler, before any source lookup).
func TestCopyObjectNeedsSourceReadAndDestinationWrite(t *testing.T) {
	for principal, want := range map[string]ErrorCode{"writer": ErrAccessDenied, "reader": ErrAccessDenied, "plain": ErrAccessDenied} {
		w, writer := newCopyWorld(t)
		_, e := w.sendHeaders(t, http.MethodPut, "/demo/dst", principal, "", map[string]string{"X-Amz-Copy-Source": "demo/a"})
		if e == nil || e.Code != want || writer.copied.Key != "" {
			t.Fatalf("%s: error %v, writer saw %+v", principal, e, writer.copied)
		}
	}
	w, writer := newCopyWorld(t)
	w.store.SetGrant(storage.Grant{BucketID: "bkt-demo-0001", UserID: "user-writer", Read: true, Write: true, Revision: 2})
	if _, e := w.sendHeaders(t, http.MethodPut, "/demo/dst", "writer", "", map[string]string{"X-Amz-Copy-Source": "demo/a"}); e != nil || writer.copied.Key != "dst" {
		t.Fatal("read+write grantee refused", e)
	}
}

func TestCopyObjectErrors(t *testing.T) {
	cases := []struct {
		err  error
		code ErrorCode
		key  string
	}{
		{object.ErrCopySourceTooLarge, ErrInvalidRequest, "dst"},
		{object.ErrCopyToItself, ErrInvalidRequest, "dst"},
		{object.ErrCopyChecksumChange, ErrInvalidRequest, "dst"},
		{object.ErrCrossDomainCopy, ErrNotImplemented, "dst"},
		{object.ErrNoSuchCopySource, ErrNoSuchKey, "a"},
		{storage.ErrQuotaExceeded, ErrQuotaExceeded, "dst"},
		{storage.ErrCold, ErrInvalidObjectState, "dst"},
		{object.ErrNoSuchCopySourceBucket, ErrNoSuchBucket, ""},
	}
	for _, tc := range cases {
		w, writer := newCopyWorld(t)
		writer.err = tc.err
		_, e := w.sendHeaders(t, http.MethodPut, "/demo/dst", "owner", "", map[string]string{"X-Amz-Copy-Source": "demo/a"})
		if e == nil || e.Code != tc.code || e.Key != tc.key {
			t.Fatalf("%v: error %+v, want %s on key %s", tc.err, e, tc.code, tc.key)
		}
	}
}
