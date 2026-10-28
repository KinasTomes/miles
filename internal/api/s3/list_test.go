package s3

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// fakeLister lists sorted keys like the store: exclusive StartAfter, a cursor
// that overrides it, prefix filtering and a Next cursor only when a further
// key exists. Delimiter grouping is the store's contract, not the handler's.
type fakeLister struct {
	keys []string
	got  []storage.ListObjectsRequest
	err  error
}

func (f *fakeLister) ListObjects(_ context.Context, r storage.ListObjectsRequest) (object.ListPage, error) {
	f.got = append(f.got, r)
	page := object.ListPage{Objects: []object.ListEntry{}, CommonPrefixes: []string{}}
	if f.err != nil || r.MaxKeys == 0 {
		return page, f.err
	}
	after := r.StartAfter
	if r.Cursor != nil {
		after = r.Cursor.LastScannedKey
	}
	for _, k := range f.keys {
		if k <= after || !strings.HasPrefix(k, r.Prefix) {
			continue
		}
		if len(page.Objects) == r.MaxKeys {
			page.Next = &storage.ObjectListCursor{PrincipalID: r.PrincipalID, Bucket: r.Bucket, BucketID: r.BucketID, Prefix: r.Prefix, Delimiter: r.Delimiter, LastScannedKey: page.Objects[len(page.Objects)-1].Key}
			break
		}
		page.Objects = append(page.Objects, object.ListEntry{Key: k, ETag: `"etag-` + k + `"`, Size: int64(len(k)), LastModified: authNow})
	}
	return page, nil
}

func newListWorld(t *testing.T, keys ...string) (*bucketWorld, *fakeLister, *fakeObjectWriter) {
	t.Helper()
	w := newBucketWorld(t)
	lister := &fakeLister{keys: keys}
	writer := &fakeObjectWriter{}
	w.router.Handlers = NewObjectHandlers(writer, nil, lister)
	return w, lister, writer
}

func (w *bucketWorld) list(t *testing.T, principal, query string) (listBucketResult, *Error) {
	t.Helper()
	rec, e := w.send(t, http.MethodGet, "/demo?list-type=2"+query, principal, "")
	var out listBucketResult
	if e != nil {
		return out, e
	}
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/xml" {
		t.Fatalf("status %d, Content-Type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("ListBucketResult: %v: %s", err, rec.Body.String())
	}
	return out, nil
}

func TestListObjectsV2Parameters(t *testing.T) {
	for _, tc := range []struct {
		query string
		code  ErrorCode
		max   int
	}{
		{"", "", 1000},
		{"&max-keys=0", "", 0},
		{"&max-keys=7", "", 7},
		{"&max-keys=5000", "", 1000},
		{"&max-keys=-1", ErrInvalidArgument, 0},
		{"&max-keys=ten", ErrInvalidArgument, 0},
		{"&max-keys=", ErrInvalidArgument, 0},
		{"&encoding-type=xml", ErrInvalidArgument, 0},
		{"&encoding-type=", ErrInvalidArgument, 0},
		{"&fetch-owner=maybe", ErrInvalidArgument, 0},
		{"&continuation-token=", ErrInvalidArgument, 0},
		{"&continuation-token=not-a-token", ErrInvalidArgument, 0},
	} {
		t.Run(tc.query, func(t *testing.T) {
			w, lister, _ := newListWorld(t, "a")
			out, e := w.list(t, "owner", tc.query)
			if tc.code != "" {
				if e == nil || e.Code != tc.code || len(lister.got) != 0 {
					t.Fatalf("error %v, want %s before the store", e, tc.code)
				}
				return
			}
			if e != nil || len(lister.got) != 1 || lister.got[0].MaxKeys != tc.max || out.MaxKeys != tc.max {
				t.Fatalf("error %v, store request %+v, MaxKeys %d", e, lister.got, out.MaxKeys)
			}
		})
	}
}

// Every key and prefix field of the XML is URL-encoded under encoding-type=url
// and left as stored otherwise; fetch-owner reports the bucket's DSG owner.
func TestListObjectsV2ResponseAndKeyEncoding(t *testing.T) {
	keys := []string{"a b", "c+d", "e%f", "k/ü.txt"}
	w, lister, _ := newListWorld(t, keys...)
	out, e := w.list(t, "reader", "&encoding-type=url&fetch-owner=true&prefix=&start-after=%20")
	if e != nil {
		t.Fatal(e)
	}
	r := lister.got[0]
	if r.PrincipalID != "user-reader" || r.Bucket != "demo" || r.BucketID != "bkt-demo-0001" || r.StartAfter != " " || r.Cursor != nil {
		t.Fatalf("store request %+v", r)
	}
	if out.Name != "demo" || out.EncodingType != "url" || out.StartAfter != "+" || out.KeyCount != 4 || out.IsTruncated || out.NextContinuationToken != "" {
		t.Fatalf("result %+v", out)
	}
	want := []string{"a+b", "c%2Bd", "e%25f", "k%2F%C3%BC.txt"}
	for i, c := range out.Contents {
		decoded, err := url.QueryUnescape(c.Key)
		if c.Key != want[i] || err != nil || decoded != keys[i] {
			t.Fatalf("key %d = %q, want %q decoding to %q", i, c.Key, want[i], keys[i])
		}
		if c.ETag != `"etag-`+keys[i]+`"` || c.Size != int64(len(keys[i])) || c.StorageClass != "STANDARD" || c.LastModified != "2026-10-22T10:00:00.000Z" {
			t.Fatalf("content %+v", c)
		}
		if c.Owner == nil || c.Owner.ID != "user-owner" {
			t.Fatalf("fetch-owner: %+v", c.Owner)
		}
	}

	out, e = w.list(t, "owner", "&prefix=k%2F")
	if e != nil || len(out.Contents) != 1 || out.Contents[0].Key != "k/ü.txt" || out.Prefix != "k/" || out.Contents[0].Owner != nil {
		t.Fatalf("plain result %+v %v", out, e)
	}
}

// Pages follow NextContinuationToken until IsTruncated is false; the token
// works only for the principal, bucket incarnation, prefix and delimiter it
// was issued for, only unmodified and only until it expires.
func TestListObjectsV2ContinuationToken(t *testing.T) {
	w, lister, _ := newListWorld(t, "p/1", "p/2", "p/3", "p/4", "p/5")
	var keys []string
	query := "&prefix=p%2F&max-keys=2"
	var token string
	for page := 0; ; page++ {
		q := query
		if token != "" {
			q += "&continuation-token=" + url.QueryEscape(token) + "&start-after=zzz"
		}
		out, e := w.list(t, "owner", q)
		if e != nil {
			t.Fatal(e)
		}
		if out.ContinuationToken != token || out.KeyCount != len(out.Contents) {
			t.Fatalf("page %d: echoed token %q, KeyCount %d", page, out.ContinuationToken, out.KeyCount)
		}
		for _, c := range out.Contents {
			keys = append(keys, c.Key)
		}
		if !out.IsTruncated {
			if out.NextContinuationToken != "" {
				t.Fatal("last page carries a token")
			}
			break
		}
		token = out.NextContinuationToken
		if token == "" || page > 3 {
			t.Fatalf("page %d truncated without a usable token", page)
		}
	}
	if fmt.Sprint(keys) != "[p/1 p/2 p/3 p/4 p/5]" {
		t.Fatalf("keys %v", keys)
	}

	// A fresh token for the cursor after p/2.
	r := lister.got[0]
	cursor := storage.ObjectListCursor{PrincipalID: r.PrincipalID, Bucket: r.Bucket, BucketID: r.BucketID, Prefix: r.Prefix, LastScannedKey: "p/2"}
	valid := sealListToken(cursor, time.Now())
	payload, tag, _ := strings.Cut(valid, ".")
	other := cursor
	other.LastScannedKey = "p/4"
	otherPayload, _, _ := strings.Cut(sealListToken(other, time.Now()), ".")
	for name, tc := range map[string]struct{ principal, query string }{
		"other prefix":    {"owner", "&prefix=p&continuation-token=" + url.QueryEscape(valid)},
		"other delimiter": {"owner", "&prefix=p%2F&delimiter=%2F&continuation-token=" + url.QueryEscape(valid)},
		"other principal": {"reader", "&prefix=p%2F&continuation-token=" + url.QueryEscape(valid)},
		"moved position":  {"owner", "&prefix=p%2F&continuation-token=" + url.QueryEscape(otherPayload+"."+tag)},
		"no tag":          {"owner", "&prefix=p%2F&continuation-token=" + url.QueryEscape(payload)},
		"expired":         {"owner", "&prefix=p%2F&continuation-token=" + url.QueryEscape(sealListToken(cursor, time.Now().Add(-listTokenTTL)))},
	} {
		t.Run(name, func(t *testing.T) {
			before := len(lister.got)
			if _, e := w.list(t, tc.principal, tc.query); e == nil || e.Code != ErrInvalidArgument || len(lister.got) != before {
				t.Fatalf("error %v, want InvalidArgument before the store", e)
			}
		})
	}
	out, e := w.list(t, "owner", "&prefix=p%2F&continuation-token="+url.QueryEscape(valid))
	if e != nil || len(out.Contents) != 3 || out.Contents[0].Key != "p/3" {
		t.Fatalf("valid token: %+v %v", out, e)
	}
}

// LIST needs read on the bucket: the router answers before the store, and
// store-side losses of the bucket or the right map to their S3 codes.
func TestListObjectsV2Authorization(t *testing.T) {
	for principal, code := range map[string]ErrorCode{"owner": "", "reader": "", "writer": ErrAccessDenied, "plain": ErrAccessDenied, "admin": ErrAccessDenied} {
		w, lister, _ := newListWorld(t, "k")
		_, e := w.list(t, principal, "")
		if code == "" && e != nil || code != "" && (e == nil || e.Code != code || len(lister.got) != 0) {
			t.Fatalf("%s: error %v, want %q", principal, e, code)
		}
	}
	w, _, _ := newListWorld(t)
	if _, e := w.send(t, http.MethodGet, "/absent?list-type=2", "owner", ""); e == nil || e.Code != ErrNoSuchBucket {
		t.Fatalf("absent bucket: %v", e)
	}
	for err, code := range map[error]ErrorCode{
		storage.ErrNotFound:       ErrNoSuchBucket,
		storage.ErrBucketNotFound: ErrNoSuchBucket,
		storage.ErrAccessDenied:   ErrAccessDenied,
		storage.ErrUnavailable:    ErrServiceUnavailable,
	} {
		w, lister, _ := newListWorld(t)
		lister.err = fmt.Errorf("list: %w", err)
		if _, e := w.list(t, "owner", ""); e == nil || e.Code != code {
			t.Fatalf("store %v: error %v, want %s", err, e, code)
		}
	}
}

// The AWS SDK paginator walks more than one page of real tokens through the
// HTTP Router and parses every entry.
func TestListObjectsV2PaginatorSDK(t *testing.T) {
	keys := make([]string, 2503)
	for i := range keys {
		keys[i] = fmt.Sprintf("dir/key %04d+ü", i)
	}
	w, _, _ := newListWorld(t, keys...)
	w.router.Now = time.Now
	w.router.Credentials = credentialMap{accessKey("owner"): {UserID: "user-owner", Secret: "secret-owner", Active: true}}
	srv := httptest.NewServer(&w.router)
	defer srv.Close()
	client := awss3.New(awss3.Options{
		Region: authRegion, BaseEndpoint: aws.String(srv.URL), UsePathStyle: true,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: accessKey("owner"), SecretAccessKey: "secret-owner"}, nil
		}),
	})
	var got []string
	pages := 0
	p := awss3.NewListObjectsV2Paginator(client, &awss3.ListObjectsV2Input{Bucket: aws.String("demo"), Prefix: aws.String("dir/")})
	for p.HasMorePages() {
		page, err := p.NextPage(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, c := range page.Contents {
			if c.LastModified == nil || !c.LastModified.Equal(authNow) || aws.ToString(c.ETag) == "" {
				t.Fatalf("entry %+v", c)
			}
			got = append(got, aws.ToString(c.Key))
		}
	}
	if pages != 3 || fmt.Sprint(got) != fmt.Sprint(keys) {
		t.Fatalf("%d pages, %d keys", pages, len(got))
	}
}

// DELETE answers 204 to every caller with write, whether or not the key
// exists, and hands the service the router's bucket incarnation.
func TestDeleteObjectThroughRouter(t *testing.T) {
	for principal, code := range map[string]ErrorCode{"owner": "", "writer": "", "reader": ErrAccessDenied, "plain": ErrAccessDenied, "admin": ErrAccessDenied} {
		w, _, writer := newListWorld(t)
		rec, e := w.send(t, http.MethodDelete, "/demo/dir/a%20b%2Bc", principal, "")
		if code != "" {
			if e == nil || e.Code != code || writer.deleted.Key != "" {
				t.Fatalf("%s: error %v, want %s before the service", principal, e, code)
			}
			continue
		}
		d := writer.deleted
		if e != nil || rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("%s: status %d error %v", principal, rec.Code, e)
		}
		if d.Key != "dir/a b+c" || d.Bucket.ID != "bkt-demo-0001" || d.UserID != "user-"+principal || d.CredentialID != accessKey(principal) {
			t.Fatalf("%s: service request %+v", principal, d)
		}
	}
	w, _, writer := newListWorld(t)
	if _, e := w.send(t, http.MethodDelete, "/absent/k", "owner", ""); e == nil || e.Code != ErrNoSuchBucket || writer.deleted.Key != "" {
		t.Fatalf("absent bucket: %v", e)
	}
}

// A payload that fails its x-amz-content-sha256 check fails the DELETE
// before the tombstone transaction.
func TestDeleteObjectChecksPayload(t *testing.T) {
	w, _, writer := newListWorld(t)
	r := httptest.NewRequest(http.MethodDelete, "/demo/k", strings.NewReader("x"))
	sign(r, accessKey("owner"), "secret-owner")
	err := w.router.serve(httptest.NewRecorder(), r, "DELETETEST")
	if code(t, err) != ErrXAmzContentSHA256Mismatch || writer.deleted.Key != "" {
		t.Fatalf("error %v, service request %+v", err, writer.deleted)
	}
}

func TestDeleteObjectErrors(t *testing.T) {
	for err, want := range map[error]ErrorCode{
		storage.ErrBucketNotFound: ErrNoSuchBucket,
		storage.ErrAccessDenied:   ErrAccessDenied,
		storage.ErrConflict:       ErrOperationAborted,
		storage.ErrUnavailable:    ErrServiceUnavailable,
		storage.ErrQuotaExceeded:  ErrQuotaExceeded,
	} {
		w, _, writer := newListWorld(t)
		writer.err = fmt.Errorf("delete: %w", err)
		if _, e := w.send(t, http.MethodDelete, "/demo/k", "owner", ""); e == nil || e.Code != want {
			t.Fatalf("service %v: error %v, want %s", err, e, want)
		}
	}
}
