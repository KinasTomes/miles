package s3

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func TestPresignedAuthorizationFixture(t *testing.T) {
	var f authFixture
	readFixture(t, "authorization.json", &f)
	runQuery := func(t *testing.T, c authCase, want string) {
		t.Helper()
		w := newAuthWorld(t, f, c)
		r := presignRequest(t, c.request(), c.Principal, maxPresignExpires, authNow)
		rec := httptest.NewRecorder()
		err := w.router.serve(rec, r, "PRESIGNEDAUTH")
		got := rec.Body.String()
		if err != nil {
			var e *Error
			if !errors.As(err, &e) {
				t.Fatal(err)
			}
			got = string(e.Code)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
	for _, row := range f.S3 {
		for principal, want := range row.Expect {
			t.Run(string(row.Op)+"/"+principal, func(t *testing.T) {
				runQuery(t, authCase{Op: row.Op, Principal: principal}, want)
			})
		}
	}
	for _, c := range f.S3Cases {
		t.Run(c.Name, func(t *testing.T) { runQuery(t, c, c.Expect) })
	}
}

func TestPresignedExpiryAndTampering(t *testing.T) {
	for _, method := range []string{"GET", "PUT"} {
		for _, tc := range []struct {
			name    string
			expires int
			age     time.Duration
			mutate  func(*http.Request)
			want    ErrorCode
		}{
			{name: "one second", expires: 1, age: time.Second},
			{name: "seven days inclusive", expires: 604800, age: 7 * 24 * time.Hour},
			{name: "past expiry", expires: 604800, age: 7*24*time.Hour + time.Nanosecond, want: ErrAccessDenied},
			{name: "zero", expires: 0, want: ErrInvalidArgument},
			{name: "over cap", expires: 604801, want: ErrInvalidArgument},
			{name: "negative", expires: -1, want: ErrInvalidArgument},
			{name: "future skew", expires: 900, age: -15*time.Minute - time.Second, want: ErrRequestTimeTooSkewed},
			{name: "host", expires: 900, mutate: func(r *http.Request) { r.Host = "changed.test:443" }, want: ErrSignatureDoesNotMatch},
			{name: "path", expires: 900, mutate: func(r *http.Request) { r.URL.Path += "x"; r.RequestURI = r.URL.RequestURI() }, want: ErrSignatureDoesNotMatch},
			{name: "method", expires: 900, mutate: func(r *http.Request) { r.Method = "HEAD" }, want: ErrSignatureDoesNotMatch},
			{name: "scope", expires: 900, mutate: func(r *http.Request) {
				q := r.URL.Query()
				q.Set("X-Amz-Credential", strings.Replace(q.Get("X-Amz-Credential"), authRegion, "other-region", 1))
				r.URL.RawQuery = q.Encode()
			}, want: ErrInvalidArgument},
		} {
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				w := newBucketWorld(t)
				w.router.Now = func() time.Time { return authNow.Add(tc.age) }
				for _, op := range []Operation{GetObject, PutObject} {
					w.router.Handlers[op] = func(rw http.ResponseWriter, r *Request) error { _, err := io.Copy(io.Discard, r.Body); return err }
				}
				r := presignRequest(t, httptest.NewRequest(method, "http://s3.test:443/demo/a//b/../caf%C3%A9+%25%2F", nil), "owner", tc.expires, authNow)
				if tc.mutate != nil {
					tc.mutate(r)
				}
				rec := httptest.NewRecorder()
				w.router.ServeHTTP(rec, r)
				if tc.want == "" {
					if rec.Code != 200 {
						t.Fatalf("got %d %s", rec.Code, rec.Body.String())
					}
				} else if r.Method == "HEAD" {
					if rec.Code != 403 || rec.Body.Len() != 0 {
						t.Fatalf("HEAD got %d %q", rec.Code, rec.Body.String())
					}
				} else if got := errorCode(rec); got != tc.want {
					t.Fatalf("got %d %s, want %s", rec.Code, got, tc.want)
				}
			})
		}
	}
}

func TestPresignedQueryHeaderSemantics(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
		want  ErrorCode
	}{
		{"metadata", "X-Amz-Meta-With_Underscore=value", ""},
		{"unsupported ACL", "X-Amz-Acl=public-read", ErrNotImplemented},
		{"unsupported checksum", "X-Amz-Checksum-Sha512=AAAA", ErrInvalidRequest},
		{"case aliases", "X-Amz-Meta-Foo=a&x-amz-meta-foo=b", ErrInvalidArgument},
		{"invalid name", "X-Amz-Meta-Foo%3Abar=value", ErrInvalidArgument},
		{"invalid value", "X-Amz-Meta-Foo=a%0D%0Ab", ErrInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBucketWorld(t)
			called := false
			w.router.Handlers[PutObject] = func(_ http.ResponseWriter, r *Request) error {
				called = true
				if r.HTTP.Header.Get("X-Amz-Meta-With_Underscore") != "value" {
					t.Fatal("signed metadata was lost")
				}
				return nil
			}
			r := presignRequest(t, httptest.NewRequest("PUT", "http://s3.test/demo/k?"+tc.query, nil), "owner", 900, authNow)
			rec := httptest.NewRecorder()
			w.router.ServeHTTP(rec, r)
			if tc.want == "" {
				if rec.Code != 200 || !called {
					t.Fatalf("got %d %s", rec.Code, rec.Body.String())
				}
			} else if called || errorCode(rec) != tc.want {
				t.Fatalf("got %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// Use the independent SDK signer, including its header hoisting, rather than
// signing with the verifier's own canonical-request implementation.
func presignRequest(t *testing.T, r *http.Request, principal string, expires int, at time.Time) *http.Request {
	t.Helper()
	q := r.URL.Query()
	q.Set("X-Amz-Expires", strconv.Itoa(expires))
	r.URL.RawQuery = q.Encode()
	uri, headers, err := v4.NewSigner().PresignHTTP(context.Background(),
		aws.Credentials{AccessKeyID: accessKey(principal), SecretAccessKey: "secret-" + principal},
		r, unsignedPayload, "s3", authRegion, at,
		func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err != nil {
		t.Fatal(err)
	}
	signed := httptest.NewRequest(r.Method, uri, r.Body)
	signed.Header = headers
	signed.Header.Del("Host")
	signed.ContentLength = r.ContentLength
	return signed
}

func TestPresignedHeaderQueryConflict(t *testing.T) {
	for _, match := range []bool{false, true} {
		t.Run(strconv.FormatBool(match), func(t *testing.T) {
			w := newBucketWorld(t)
			called := false
			w.router.Handlers[PutObject] = func(_ http.ResponseWriter, r *Request) error {
				called = true
				if r.HTTP.Header.Get("X-Amz-Meta-Test") != "query" {
					t.Fatal("metadata changed")
				}
				return nil
			}
			r := httptest.NewRequest("PUT", "http://s3.test/demo/k?X-Amz-Meta-Test=query", nil)
			value := "header"
			if match {
				value = "query"
			}
			r.Header.Set("X-Amz-Meta-Test", value)
			r = presignRequest(t, r, "owner", 900, authNow)
			rec := httptest.NewRecorder()
			w.router.ServeHTTP(rec, r)
			if match {
				if !called || rec.Code != 200 {
					t.Fatalf("matching values rejected: %d", rec.Code)
				}
			} else if called || errorCode(rec) != ErrInvalidArgument {
				t.Fatalf("conflict accepted: %d", rec.Code)
			}
		})
	}
}

func TestPresignedQueryCopyRouting(t *testing.T) {
	w := newBucketWorld(t)
	called := false
	w.router.Handlers[CopyObject] = func(_ http.ResponseWriter, r *Request) error {
		called = true
		_, err := r.AuthorizeCopySource("demo")
		return err
	}
	for _, principal := range []string{"owner", "writer"} {
		called = false
		r := httptest.NewRequest("PUT", "http://s3.test/demo/dst?X-Amz-Copy-Source=%2Fdemo%2Fsrc", nil)
		r = presignRequest(t, r, principal, 900, authNow)
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, r)
		if !called {
			t.Fatal("query copy source did not select CopyObject")
		}
		if principal == "owner" && rec.Code != 200 || principal == "writer" && errorCode(rec) != ErrAccessDenied {
			t.Fatalf("copy source read authorization bypassed: %s, %d", principal, rec.Code)
		}
	}
}

func TestPresignedHoistedChecksum(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(strconv.FormatBool(corrupt), func(t *testing.T) {
			w := newBucketWorld(t)
			w.router.Handlers[PutObject] = func(rw http.ResponseWriter, r *Request) error {
				_, err := io.Copy(io.Discard, r.Body)
				return err
			}
			r := httptest.NewRequest("PUT", "http://s3.test/demo/key", strings.NewReader("hello"))
			sum := sha256.Sum256([]byte("hello"))
			q := r.URL.Query()
			q.Set("X-Amz-Checksum-Sha256", base64.StdEncoding.EncodeToString(sum[:]))
			r.URL.RawQuery = q.Encode()
			r = presignRequest(t, r, "owner", 900, authNow)
			if !r.URL.Query().Has("X-Amz-Checksum-Sha256") {
				t.Fatal("signed checksum missing")
			}
			if corrupt {
				r.Body = io.NopCloser(strings.NewReader("jello"))
			}
			rec := httptest.NewRecorder()
			w.router.ServeHTTP(rec, r)
			if corrupt {
				if rec.Code != 400 || errorCode(rec) != ErrBadDigest {
					t.Fatalf("corrupted presigned PUT: %d %s", rec.Code, rec.Body.String())
				}
			} else if rec.Code != 200 {
				t.Fatalf("presigned PUT: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}
