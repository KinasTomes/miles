package s3

import (
	"context"
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
		{name: "REPLACE takes the request headers", headers: map[string]string{"X-Amz-Copy-Source": "demo/a", "X-Amz-Metadata-Directive": "REPLACE", "X-Amz-Meta-Tag": "v2", "Content-Language": "vi-VN"}, srcKey: "a",
			replaced: map[string]string{"content-type": "application/octet-stream", "content-language": "vi-VN", "x-amz-meta-tag": "v2"}},
		{name: "REPLACE keeps an explicit Content-Type", headers: map[string]string{"X-Amz-Copy-Source": "demo/a", "X-Amz-Metadata-Directive": "REPLACE", "Content-Type": "text/csv"}, srcKey: "a",
			replaced: map[string]string{"content-type": "text/csv"}},
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

// metadataObjects keeps each key's stored HTTP metadata the way the service
// does: COPY repeats the source's, REPLACE stores the request's.
type metadataObjects struct {
	fakeObjectWriter
	stored map[string]map[string]string
}

func (m *metadataObjects) CopyObject(_ context.Context, r object.CopyRequest) (object.CopyResult, error) {
	md := r.Metadata
	if md == nil {
		md = m.stored[r.SourceKey]
	}
	m.stored[r.Key] = md
	return object.CopyResult{ETag: `"e"`}, nil
}
func (m *metadataObjects) info(key string) object.GetResult {
	return object.GetResult{Info: object.Info{ETag: `"e"`, Size: 1, Metadata: m.stored[key]}, Range: storage.ByteRange{Length: 1}}
}
func (m *metadataObjects) Stat(_ context.Context, r object.GetRequest) (object.GetResult, error) {
	return m.info(r.Key), nil
}
func (m *metadataObjects) GetObject(ctx context.Context, r object.GetRequest) (object.GetResult, error) {
	res := m.info(r.Key)
	if err := r.Ready(res); err != nil {
		return res, err
	}
	return res, r.Emit(ctx, []byte("x"))
}

// REPLACE metadata, Content-Language included, is what GET and HEAD return
// afterwards; COPY keeps the source's.
func TestCopyObjectMetadataReadsBack(t *testing.T) {
	w := newBucketWorld(t)
	objects := &metadataObjects{stored: map[string]map[string]string{
		"a": {"content-type": "text/plain", "content-language": "en", "x-amz-meta-k": "v"},
	}}
	w.router.Handlers = NewObjectHandlers(objects, objects, nil)
	if _, e := w.sendHeaders(t, http.MethodPut, "/demo/b", "owner", "", map[string]string{"X-Amz-Copy-Source": "demo/a"}); e != nil {
		t.Fatal(e)
	}
	if _, e := w.sendHeaders(t, http.MethodPut, "/demo/c", "owner", "", map[string]string{
		"X-Amz-Copy-Source": "demo/a", "X-Amz-Metadata-Directive": "REPLACE", "Content-Language": "vi-VN", "X-Amz-Meta-Tag": "v2",
	}); e != nil {
		t.Fatal(e)
	}
	want := map[string]map[string]string{
		"b": {"Content-Type": "text/plain", "Content-Language": "en", "X-Amz-Meta-K": "v", "X-Amz-Meta-Tag": ""},
		"c": {"Content-Type": "application/octet-stream", "Content-Language": "vi-VN", "X-Amz-Meta-K": "", "X-Amz-Meta-Tag": "v2"},
	}
	for key, headers := range want {
		for _, method := range []string{http.MethodHead, http.MethodGet} {
			rec, e := w.sendHeaders(t, method, "/demo/"+key, "owner", "", nil)
			if e != nil {
				t.Fatal(key, method, e)
			}
			for name, value := range headers {
				if got := rec.Header().Get(name); got != value {
					t.Fatalf("%s %s: %s = %q, want %q", method, key, name, got, value)
				}
			}
		}
	}
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
