package s3

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestObjectMissingBucketAndKeyErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want ErrorCode
	}{
		{"bucket", storage.ErrBucketNotFound, ErrNoSuchBucket},
		{"wrapped bucket", fmt.Errorf("object operation: %w", storage.ErrBucketNotFound), ErrNoSuchBucket},
		{"key", storage.ErrNotFound, ErrNoSuchKey},
		{"wrapped key", fmt.Errorf("object lookup: %w", storage.ErrNotFound), ErrNoSuchKey},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut} {
			if method == http.MethodPut && tc.want == ErrNoSuchKey {
				continue
			}
			t.Run(method+"/"+tc.name, func(t *testing.T) {
				w := newBucketWorld(t)
				reader := &fakeObjectReader{statErr: tc.err, getErr: tc.err}
				w.router.Handlers = NewObjectHandlers(&fakeObjectWriter{err: tc.err}, reader)
				r := httptest.NewRequest(method, "/demo/k", nil)
				sign(r, accessKey("owner"), "secret-owner")
				rec := httptest.NewRecorder()
				// The bucket authorizes successfully; its loss is observed by the service.
				err := w.router.serve(rec, r, "OBJECTERROR")
				if got := code(t, err); got != tc.want {
					t.Fatalf("code %s, want %s", got, tc.want)
				}
				if err := WriteError(rec, r, err.(*Error), "OBJECTERROR"); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusNotFound {
					t.Fatalf("status %d, want 404", rec.Code)
				}
				if method == http.MethodHead {
					if rec.Body.Len() != 0 {
						t.Fatal("HEAD error must be bodyless")
					}
				} else if got := checkErrorBody(t, rec); got != tc.want {
					t.Fatalf("XML code %s, want %s", got, tc.want)
				}
			})
		}
	}
}
