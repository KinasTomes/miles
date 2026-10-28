package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

type fakeObjectWriter struct {
	got    object.PutRequest
	result object.PutResult
	err    error
}

func (f *fakeObjectWriter) PutObject(_ context.Context, r object.PutRequest) (object.PutResult, error) {
	f.got = r
	return f.result, f.err
}

type fakeObjectReader struct {
	stat      object.GetResult
	statErr   error
	get       object.GetResult
	getErr    error
	body      []byte
	readyErr  error
	statCalls int
	getCalls  int
}

func (f *fakeObjectReader) Stat(context.Context, object.GetRequest) (object.GetResult, error) {
	f.statCalls++
	return f.stat, f.statErr
}

func (f *fakeObjectReader) GetObject(ctx context.Context, r object.GetRequest) (object.GetResult, error) {
	f.getCalls++
	if f.getErr != nil {
		return f.get, f.getErr
	}
	if r.Ready != nil {
		if err := r.Ready(f.get); err != nil {
			return f.get, err
		}
	}
	if f.body != nil {
		if err := r.Emit(ctx, f.body); err != nil {
			return f.get, err
		}
	}
	return f.get, nil
}

func objectRequest(method, bucket, key string, body io.Reader, headers map[string]string) *Request {
	req := httptest.NewRequest(method, "/"+bucket+"/"+key, body)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return &Request{
		Route:  Route{Op: Operation(strings.ToLower(method)), Bucket: bucket, Key: key},
		HTTP:   req,
		ID:     "REQ000000000000",
		Caller: Caller{AccessKeyID: "key", UserID: "user"},
		Access: storage.Authorization{UserID: "user", CredentialID: "key", Read: true, Write: true,
			Bucket: storage.Bucket{Name: bucket, ID: "bucket-" + bucket, OwnerID: "user", Domain: "default", Active: true}},
	}
}

func objectInfo(size int64, etag string) object.GetResult {
	return object.GetResult{Info: object.Info{
		ETag: etag, Size: size, LastModified: time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC),
		Metadata: map[string]string{"content-type": "text/plain"},
		Checksum: storage.Checksum{Algorithm: "CRC64NVME", Type: "FULL_OBJECT", Value: "Y53hf1fT3M4="},
	}, Range: storage.ByteRange{Offset: 0, Length: size}}
}

func code(t *testing.T, err error) ErrorCode {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("error %v is not an S3 error", err)
	}
	return e.Code
}

func TestPutObjectWiringAndMetadata(t *testing.T) {
	writer := &fakeObjectWriter{result: object.PutResult{ETag: `"etag"`, ChecksumAlgorithm: "CRC64NVME", ChecksumValue: "AAAA"}}
	h := &Objects{Writer: writer}
	r := objectRequest(http.MethodPut, "demo", "file.txt", strings.NewReader("hello"), map[string]string{
		"X-Amz-Meta-Owner": "team",
		"Cache-Control":    "max-age=60",
	})
	r.Body = newPayload(r.HTTP, PutObject, nil)
	r.Access.Bucket.Domain = "domain-1"
	rec := httptest.NewRecorder()
	if err := h.PutObject(rec, r); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || rec.Header().Get("ETag") != `"etag"` || rec.Header().Get("x-amz-checksum-crc64nvme") != "AAAA" {
		t.Fatalf("response %d %v", rec.Code, rec.Header())
	}
	got := writer.got
	if got.Key != "file.txt" || got.ContentLength != 5 || got.Bucket.Domain != "domain-1" || got.UserID != "user" || got.CredentialID != "key" {
		t.Fatalf("request %+v", got)
	}
	if got.Metadata["content-type"] != "binary/octet-stream" || got.Metadata["cache-control"] != "max-age=60" || got.Metadata["x-amz-meta-owner"] != "team" {
		t.Fatalf("metadata %v", got.Metadata)
	}
}

func TestPutObjectErrors(t *testing.T) {
	for name, tc := range map[string]struct {
		writer *fakeObjectWriter
		header map[string]string
		body   io.Reader
		code   ErrorCode
	}{
		"quota":     {writer: &fakeObjectWriter{err: storage.ErrQuotaExceeded}, code: ErrQuotaExceeded},
		"denied":    {writer: &fakeObjectWriter{err: storage.ErrAccessDenied}, code: ErrAccessDenied},
		"unready":   {writer: &fakeObjectWriter{err: storage.ErrUnavailable}, code: ErrServiceUnavailable},
		"bad md5":   {writer: &fakeObjectWriter{err: &Error{Code: ErrBadDigest}}, code: ErrBadDigest},
		"no length": {writer: &fakeObjectWriter{}, body: strings.NewReader(""), header: map[string]string{}, code: ErrInvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			r := objectRequest(http.MethodPut, "demo", "k", tc.body, tc.header)
			if tc.body == nil {
				r.HTTP.Body = http.NoBody
				r.HTTP.ContentLength = 0
			}
			if name == "no length" {
				r.HTTP.ContentLength = -1
				r.HTTP.Body = http.NoBody
			}
			r.Body = newPayload(r.HTTP, PutObject, nil)
			rec := httptest.NewRecorder()
			err := (&Objects{Writer: tc.writer}).PutObject(rec, r)
			if got := code(t, err); got != tc.code {
				t.Fatalf("code %s, want %s", got, tc.code)
			}
		})
	}
}

func TestGetObjectHeadersRangeAndChecksum(t *testing.T) {
	tests := map[string]struct {
		rangeHdr   string
		mode       string
		status     int
		length     int64
		contentRng string
		checksum   bool
	}{
		"full":           {status: 200, length: 100, checksum: false},
		"checksum mode":  {mode: "ENABLED", status: 200, length: 100, checksum: true},
		"closed range":   {rangeHdr: "bytes=10-19", status: 206, length: 10, contentRng: "bytes 10-19/100"},
		"open range":     {rangeHdr: "bytes=90-", status: 206, length: 10, contentRng: "bytes 90-99/100"},
		"suffix range":   {rangeHdr: "bytes=-10", status: 206, length: 10, contentRng: "bytes 90-99/100"},
		"clamped end":    {rangeHdr: "bytes=90-1000", status: 206, length: 10, contentRng: "bytes 90-99/100"},
		"other unit":     {rangeHdr: "items=0-5", status: 200, length: 100},
		"range checksum": {rangeHdr: "bytes=0-9", mode: "ENABLED", status: 206, length: 10, contentRng: "bytes 0-9/100"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			reader := &fakeObjectReader{stat: objectInfo(100, `"etag"`)}
			reader.get = reader.stat
			if tc.rangeHdr != "" && strings.Contains(tc.contentRng, "-") {
				var lo, hi int64
				if _, err := fmt.Sscanf(tc.contentRng, "bytes %d-%d/100", &lo, &hi); err != nil {
					t.Fatal(err)
				}
				reader.get.Range = storage.ByteRange{Offset: lo, Length: hi - lo + 1}
			}
			reader.body = []byte(strings.Repeat("x", int(tc.length)))
			headers := map[string]string{}
			if tc.rangeHdr != "" {
				headers["Range"] = tc.rangeHdr
			}
			if tc.mode != "" {
				headers["x-amz-checksum-mode"] = tc.mode
			}
			r := objectRequest(http.MethodGet, "demo", "k", nil, headers)
			rec := httptest.NewRecorder()
			if err := (&Objects{Reader: reader}).GetObject(rec, r); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.status || rec.Body.Len() != int(tc.length) {
				t.Fatalf("status %d body %d, want %d/%d", rec.Code, rec.Body.Len(), tc.status, tc.length)
			}
			if got := rec.Header().Get("Content-Length"); got != strconv.FormatInt(tc.length, 10) {
				t.Fatalf("Content-Length %s", got)
			}
			if got := rec.Header().Get("Content-Range"); got != tc.contentRng {
				t.Fatalf("Content-Range %q, want %q", got, tc.contentRng)
			}
			hasChecksum := rec.Header().Get("x-amz-checksum-crc64nvme") != ""
			if hasChecksum != tc.checksum {
				t.Fatalf("checksum headers %v, want %v", rec.Header(), tc.checksum)
			}
			if rec.Header().Get("ETag") != `"etag"` || rec.Header().Get("Last-Modified") != "Sun, 18 Oct 2026 00:00:00 GMT" || rec.Header().Get("Accept-Ranges") != "bytes" {
				t.Fatalf("headers %v", rec.Header())
			}
		})
	}
}

func TestGetObjectRangeAndPreconditionErrors(t *testing.T) {
	reader := &fakeObjectReader{stat: objectInfo(100, `"etag"`)}
	reader.get = reader.stat
	for name, tc := range map[string]struct {
		header map[string]string
		code   ErrorCode
		calls  int
	}{
		"past object":   {header: map[string]string{"Range": "bytes=100-"}, code: ErrInvalidRange},
		"multi range":   {header: map[string]string{"Range": "bytes=0-1,5-6"}, code: ErrInvalidRange},
		"zero suffix":   {header: map[string]string{"Range": "bytes=-0"}, code: ErrInvalidRange},
		"if-match miss": {header: map[string]string{"If-Match": `"other"`}, code: ErrPreconditionFailed},
	} {
		t.Run(name, func(t *testing.T) {
			reader.getCalls = 0
			r := objectRequest(http.MethodGet, "demo", "k", nil, tc.header)
			err := (&Objects{Reader: reader}).GetObject(httptest.NewRecorder(), r)
			if got := code(t, err); got != tc.code {
				t.Fatalf("code %s, want %s", got, tc.code)
			}
			if reader.getCalls != tc.calls {
				t.Fatalf("GetObject called %d times", reader.getCalls)
			}
		})
	}
}

func TestHeadObjectHeadersAndRange(t *testing.T) {
	for name, tc := range map[string]struct {
		header   map[string]string
		status   int
		length   int64
		checksum bool
	}{
		"full":          {status: 200, length: 100},
		"checksum mode": {header: map[string]string{"X-Amz-Checksum-Mode": "ENABLED"}, status: 200, length: 100, checksum: true},
		"range":         {header: map[string]string{"Range": "bytes=10-19"}, status: 200, length: 10},
		"suffix":        {header: map[string]string{"Range": "bytes=-10"}, status: 200, length: 10},
	} {
		t.Run(name, func(t *testing.T) {
			reader := &fakeObjectReader{stat: objectInfo(100, `"etag"`)}
			r := objectRequest(http.MethodHead, "demo", "k", nil, tc.header)
			rec := httptest.NewRecorder()
			if err := (&Objects{Reader: reader}).HeadObject(rec, r); err != nil {
				t.Fatal(err)
			}
			if rec.Code != tc.status || rec.Body.Len() != 0 || rec.Header().Get("Content-Length") != strconv.FormatInt(tc.length, 10) {
				t.Fatalf("status %d body %d length %s", rec.Code, rec.Body.Len(), rec.Header().Get("Content-Length"))
			}
			if rec.Header().Get("Content-Range") != "" {
				t.Fatal("HEAD must not send Content-Range")
			}
			hasChecksum := rec.Header().Get("x-amz-checksum-crc64nvme") != ""
			if hasChecksum != tc.checksum {
				t.Fatalf("checksum header mismatch %v", rec.Header())
			}
		})
	}
}

func TestHeadObjectRangePastObject(t *testing.T) {
	reader := &fakeObjectReader{stat: objectInfo(100, `"etag"`)}
	r := objectRequest(http.MethodHead, "demo", "k", nil, map[string]string{"Range": "bytes=100-"})
	err := (&Objects{Reader: reader}).HeadObject(httptest.NewRecorder(), r)
	if got := code(t, err); got != ErrInvalidRange {
		t.Fatalf("code %s", got)
	}
}

func TestEmptyObjectGetAndRange416(t *testing.T) {
	reader := &fakeObjectReader{stat: objectInfo(0, `"etag"`)}
	reader.get = reader.stat
	r := objectRequest(http.MethodGet, "demo", "empty", nil, nil)
	rec := httptest.NewRecorder()
	if err := (&Objects{Reader: reader}).GetObject(rec, r); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || rec.Header().Get("Content-Length") != "0" || rec.Body.Len() != 0 {
		t.Fatalf("empty GET %d %v", rec.Code, rec.Header())
	}
	r = objectRequest(http.MethodGet, "demo", "empty", nil, map[string]string{"Range": "bytes=0-"})
	err := (&Objects{Reader: reader}).GetObject(httptest.NewRecorder(), r)
	if got := code(t, err); got != ErrInvalidRange {
		t.Fatalf("empty range code %s", got)
	}
}
