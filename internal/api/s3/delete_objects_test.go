package s3

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

// batchWriter records every DeleteObject of a batch and fails chosen keys.
type batchWriter struct {
	fakeObjectWriter
	keys []string
	fail map[string]error
}

func (b *batchWriter) DeleteObject(_ context.Context, r object.DeleteRequest) error {
	b.keys = append(b.keys, r.Key)
	return b.fail[r.Key]
}

func deleteDoc(quiet bool, objects ...string) string {
	var b strings.Builder
	b.WriteString(`<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	for _, o := range objects {
		b.WriteString("<Object>" + o + "</Object>")
	}
	if quiet {
		b.WriteString("<Quiet>true</Quiet>")
	}
	b.WriteString("</Delete>")
	return b.String()
}

func contentMD5(body string) map[string]string {
	sum := md5.Sum([]byte(body))
	return map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:])}
}

func (w *bucketWorld) deleteObjects(t *testing.T, principal, body string, headers map[string]string) (deleteResult, *Error) {
	t.Helper()
	rec, e := w.sendHeaders(t, http.MethodPost, "/demo?delete", principal, body, headers)
	var out deleteResult
	if e == nil {
		if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("DeleteResult %d %q: %v", rec.Code, rec.Body.String(), err)
		}
	}
	return out, e
}

// Every key commits on its own: a missing key is Deleted, a failing key is
// its own Error, and Quiet drops only the Deleted entries.
func TestDeleteObjectsPerKeyResults(t *testing.T) {
	for _, quiet := range []bool{false, true} {
		w := newBucketWorld(t)
		writer := &batchWriter{fail: map[string]error{"locked": storage.ErrQuotaExceeded, "gone": storage.ErrBucketNotFound}}
		w.router.Handlers = NewObjectHandlers(writer, nil, nil)
		body := deleteDoc(quiet, "<Key>a</Key>", "<Key>locked</Key>", "<Key>missing</Key>", "<Key>gone</Key>", "<Key>a &amp; b</Key>")
		out, e := w.deleteObjects(t, "writer", body, contentMD5(body))
		if e != nil {
			t.Fatal(e)
		}
		if strings.Join(writer.keys, ",") != "a,locked,missing,gone,a & b" {
			t.Fatalf("deleted %q", writer.keys)
		}
		deleted := []string{}
		for _, d := range out.Deleted {
			deleted = append(deleted, d.Key)
		}
		if want := "a,missing,a & b"; quiet && len(deleted) != 0 || !quiet && strings.Join(deleted, ",") != want {
			t.Fatalf("quiet=%v Deleted %q", quiet, deleted)
		}
		if fmt.Sprint(out.Errors) != "[{locked QuotaExceeded "+errorCodes[ErrQuotaExceeded].message+"} {gone NoSuchBucket "+errorCodes[ErrNoSuchBucket].message+"}]" {
			t.Fatalf("quiet=%v Errors %+v", quiet, out.Errors)
		}
	}
}

// The whole request is validated before the first tombstone.
func TestDeleteObjectsRejectsBeforeAnyDelete(t *testing.T) {
	many := make([]string, maxDeleteKeys+1)
	for i := range many {
		many[i] = fmt.Sprintf("<Key>k%d</Key>", i)
	}
	cases := []struct {
		name, principal, body string
		headers               map[string]string
		code                  ErrorCode
	}{
		{name: "no Content-MD5 or checksum", principal: "owner", body: deleteDoc(false, "<Key>a</Key>"), code: ErrInvalidRequest},
		{name: "wrong Content-MD5", principal: "owner", body: deleteDoc(false, "<Key>a</Key>"), headers: contentMD5("other"), code: ErrBadDigest},
		{name: "no objects", principal: "owner", body: deleteDoc(false), code: ErrMalformedXML},
		{name: "1001 objects", principal: "owner", body: deleteDoc(false, many...), code: ErrMalformedXML},
		{name: "empty key", principal: "owner", body: deleteDoc(false, "<Key>a</Key>", "<Key></Key>"), code: ErrMalformedXML},
		{name: "key too long", principal: "owner", body: deleteDoc(false, "<Key>a</Key>", "<Key>"+strings.Repeat("k", 1025)+"</Key>"), code: ErrKeyTooLong},
		{name: "VersionId", principal: "owner", body: deleteDoc(false, "<Key>a</Key>", "<Key>b</Key><VersionId>v1</VersionId>"), code: ErrNotImplemented},
		{name: "conditional ETag", principal: "owner", body: deleteDoc(false, "<Key>a</Key>", "<Key>b</Key><ETag>\"x\"</ETag>"), code: ErrNotImplemented},
		{name: "not XML", principal: "owner", body: "<Delete><Object><Key>a</Key></Object>", code: ErrMalformedXML},
		{name: "reader has no write", principal: "reader", body: deleteDoc(false, "<Key>a</Key>"), code: ErrAccessDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newBucketWorld(t)
			writer := &batchWriter{}
			w.router.Handlers = NewObjectHandlers(writer, nil, nil)
			headers := tc.headers
			if headers == nil && tc.code != ErrInvalidRequest {
				headers = contentMD5(tc.body)
			}
			_, e := w.deleteObjects(t, tc.principal, tc.body, headers)
			if e == nil || e.Code != tc.code || len(writer.keys) != 0 {
				t.Fatalf("error %v, want %s; deleted %q", e, tc.code, writer.keys)
			}
		})
	}
}

// SDKs that no longer send Content-MD5 send a flexible checksum instead.
func TestDeleteObjectsAcceptsChecksumHeader(t *testing.T) {
	w := newBucketWorld(t)
	writer := &batchWriter{}
	w.router.Handlers = NewObjectHandlers(writer, nil, nil)
	body := deleteDoc(false, "<Key>a</Key>")
	sum, _ := ChecksumHash("CRC32")
	sum.Write([]byte(body))
	out, e := w.deleteObjects(t, "owner", body, map[string]string{"X-Amz-Checksum-Crc32": base64.StdEncoding.EncodeToString(sum.Sum(nil))})
	if e != nil || len(out.Deleted) != 1 || out.Deleted[0].Key != "a" {
		t.Fatal(out, e)
	}
}
