package s3

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

// Owner or system admin may delete; the name is then free and a new
// CreateBucket gets a new incarnation that inherits no grant.
func TestDeleteBucketFreesTheName(t *testing.T) {
	for _, principal := range []string{"owner", "admin"} {
		t.Run(principal, func(t *testing.T) {
			w := newBucketWorld(t)
			for _, denied := range []string{"reader", "writer", "plain"} {
				if _, e := w.send(t, http.MethodDelete, "/demo", denied, ""); e == nil || e.Code != ErrAccessDenied {
					t.Fatalf("%s: %v", denied, e)
				}
			}
			old, _ := w.store.GetBucket(context.Background(), "demo")
			rec, e := w.send(t, http.MethodDelete, "/demo", principal, "")
			if e != nil || rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
				t.Fatalf("delete: %d %v", rec.Code, e)
			}
			if _, e := w.send(t, http.MethodHead, "/demo", "owner", ""); e == nil || e.Code != ErrNoSuchBucket {
				t.Fatal("deleted bucket still answers HEAD", e)
			}
			if _, e := w.send(t, http.MethodDelete, "/demo", principal, ""); e == nil || e.Code != ErrNoSuchBucket {
				t.Fatal("second delete", e)
			}
			if rec, e := w.send(t, http.MethodPut, "/demo", "writer", ""); e != nil || rec.Code != http.StatusOK {
				t.Fatal("name not reusable", e)
			}
			now, _ := w.store.GetBucket(context.Background(), "demo")
			if now.ID == old.ID || now.OwnerID != "user-writer" {
				t.Fatalf("recreated %+v, old %+v", now, old)
			}
			if _, e := w.send(t, http.MethodHead, "/demo", "reader", ""); e == nil || e.Code != ErrAccessDenied {
				t.Fatal("old read grant survived the new incarnation", e)
			}
		})
	}
}

type deleteBucketStore struct {
	BucketStore
	err error
	got storage.BucketDelete
}

func (s *deleteBucketStore) DeleteBucket(_ context.Context, r storage.BucketDelete) error {
	s.got = r
	return s.err
}

func TestDeleteBucketErrors(t *testing.T) {
	for err, code := range map[error]ErrorCode{
		storage.ErrBucketNotEmpty: ErrBucketNotEmpty,
		storage.ErrBucketNotFound: ErrNoSuchBucket,
		storage.ErrAccessDenied:   ErrAccessDenied,
		storage.ErrConflict:       ErrOperationAborted,
		storage.ErrUnavailable:    ErrServiceUnavailable,
		storage.ErrFenced:         ErrServiceUnavailable,
	} {
		w := newBucketWorld(t)
		store := &deleteBucketStore{BucketStore: w.store, err: err}
		w.router.Handlers = NewBucketHandlers(store)
		_, e := w.send(t, http.MethodDelete, "/demo", "owner", "")
		if e == nil || e.Code != code || store.got.CredentialID != accessKey("owner") || store.got.Bucket.ID != "bkt-demo-0001" || store.got.Bucket.Revision != 1 {
			t.Fatalf("%v: error %v, want %s; store saw %+v", err, e, code, store.got)
		}
	}
}

func TestGetBucketLocation(t *testing.T) {
	w := newBucketWorld(t)
	rec, e := w.send(t, http.MethodGet, "/demo?location", "reader", "")
	var out locationConstraint
	if e != nil || rec.Code != http.StatusOK || xml.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Region != "" {
		t.Fatalf("us-east-1 is the empty constraint: %d %q %v", rec.Code, rec.Body.String(), e)
	}
	if _, e := w.send(t, http.MethodGet, "/demo?location", "plain", ""); e == nil || e.Code != ErrAccessDenied {
		t.Fatal("location without read", e)
	}
	rec = httptest.NewRecorder()
	if err := (&Buckets{}).GetBucketLocation(rec, &Request{region: "ap-southeast-1"}); err != nil ||
		xml.Unmarshal(rec.Body.Bytes(), &out) != nil || out.Region != "ap-southeast-1" {
		t.Fatalf("other region: %q %v", rec.Body.String(), err)
	}
}
