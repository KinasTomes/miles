package s3

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestObjectRouterRejectsUnsupportedConditionsBeforeService(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut} {
		for _, header := range []string{"If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since"} {
			if method == http.MethodGet && header == "If-Match" {
				continue
			}
			for _, value := range []string{"unsupported-condition", ""} {
				t.Run(method+"/"+header+"/"+value, func(t *testing.T) {
					w := newBucketWorld(t)
					reader := &fakeObjectReader{get: objectInfo(3, `"etag"`), stat: objectInfo(3, `"etag"`), body: []byte("abc")}
					writer := &fakeObjectWriter{}
					w.router.Handlers = NewObjectHandlers(writer, reader, nil)
					r := httptest.NewRequest(method, "/demo/k", nil)
					r.Header.Set(header, value)
					sign(r, accessKey("owner"), "secret-owner")
					rec := httptest.NewRecorder()
					w.router.ServeHTTP(rec, r)
					if rec.Code != http.StatusNotImplemented {
						t.Fatalf("status %d, want 501", rec.Code)
					}
					if reader.getCalls != 0 || reader.statCalls != 0 || writer.got.Key != "" {
						t.Fatal("unsupported condition reached object I/O")
					}
					if method == http.MethodHead {
						if rec.Body.Len() != 0 {
							t.Fatal("HEAD error must be bodyless")
						}
					} else if got := checkErrorBody(t, rec); got != ErrNotImplemented {
						t.Fatalf("code %s, want NotImplemented", got)
					}
				})
			}
		}
	}
}

func TestObjectRouterPreservesGetIfMatch(t *testing.T) {
	for _, tc := range []struct {
		name, match, span string
		status            int
	}{
		{"match", `"etag"`, "", http.StatusOK},
		{"mismatch", `"other"`, "", http.StatusPreconditionFailed},
		{"range match", `"etag"`, "bytes=0-2", http.StatusPartialContent},
		{"range mismatch", `"other"`, "bytes=0-2", http.StatusPreconditionFailed},
		{"wildcard", "*", "", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newBucketWorld(t)
			reader := &fakeObjectReader{get: objectInfo(3, `"etag"`), body: []byte("abc")}
			w.router.Handlers = NewObjectHandlers(nil, reader, nil)
			r := httptest.NewRequest(http.MethodGet, "/demo/k", nil)
			r.Header.Set("If-Match", tc.match)
			if tc.span != "" {
				r.Header.Set("Range", tc.span)
			}
			sign(r, accessKey("owner"), "secret-owner")
			rec := httptest.NewRecorder()
			w.router.ServeHTTP(rec, r)
			if rec.Code != tc.status || reader.getCalls != 1 || reader.statCalls != 0 {
				t.Fatalf("status %d, want %d; Get=%d Stat=%d", rec.Code, tc.status, reader.getCalls, reader.statCalls)
			}
			if tc.status == http.StatusPreconditionFailed {
				if got := checkErrorBody(t, rec); got != ErrPreconditionFailed || rec.Header().Get("ETag") != "" {
					t.Fatal("failed condition emitted object headers/body")
				}
			} else if rec.Body.String() != "abc" {
				t.Fatalf("body %q, want abc", rec.Body.String())
			}
		})
	}
}
