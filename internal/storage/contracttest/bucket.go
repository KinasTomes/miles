package contracttest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
)

// runBucketCreate pins the CreateBucket transaction of API v2 2.2.3/3.5: the
// admitting credential and user are rechecked inside the write, the name is
// taken exactly once, and every incarnation owns a fresh bucket_id, the
// owner's default_domain at create time and CreatedAt.
func runBucketCreate(t *testing.T, newStore MetadataFactory) {
	ctx := context.Background()
	t.Run("create publishes one incarnation with create-time identity", func(t *testing.T) {
		f := NewFixture()
		f.Users[0].DefaultDomain = "alpha" // owner
		h := newStore(t, f)
		b, created, err := h.Store.CreateBucket(ctx, s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "fresh"})
		if err != nil || !created {
			t.Fatalf("created=%v err=%v", created, err)
		}
		if b.OwnerID != "owner" || b.Domain != "alpha" || b.Revision != 1 || !b.Active || !b.CreatedAt.Equal(f.Now) {
			t.Fatalf("bucket=%+v", b)
		}
		if !isLowerHex(b.ID) || len(b.ID) != 32 {
			t.Fatalf("bucket_id %q is not 32 lowercase hex characters", b.ID)
		}
		saved, err := h.Store.GetBucket(ctx, "fresh")
		if err != nil || saved != b {
			t.Fatalf("saved=%+v err=%v", saved, err)
		}
	})
	t.Run("empty default domain falls back to default", func(t *testing.T) {
		f := NewFixture() // fixture users carry no DefaultDomain
		h := newStore(t, f)
		b, created, err := h.Store.CreateBucket(ctx, s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "legacy"})
		if err != nil || !created || b.Domain != "default" {
			t.Fatalf("bucket=%+v created=%v err=%v", b, created, err)
		}
	})
	t.Run("taken name returns the existing incarnation", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		old, created, err := h.Store.CreateBucket(ctx, s.BucketCreate{CredentialID: "writer-key", UserID: "writer", Name: f.Bucket.Name})
		if err != nil || created || old != f.Bucket {
			t.Fatalf("collision: bucket=%+v created=%v err=%v", old, created, err)
		}
		saved, _ := h.Store.GetBucket(ctx, f.Bucket.Name)
		if saved != f.Bucket {
			t.Fatal("collision rewrote the existing bucket")
		}
	})
	for _, change := range []string{"revoked credential", "disabled user", "credential of another user"} {
		t.Run("create rechecks "+change, func(t *testing.T) {
			f := NewFixture()
			h := newStore(t, f)
			r := s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "fresh"}
			switch change {
			case "revoked credential":
				c := f.Credentials[0]
				c.Active = false
				h.SetCredential(c)
			case "disabled user":
				u := f.Users[0]
				u.Active = false
				h.SetUser(u)
			case "credential of another user":
				r.UserID = "writer"
			}
			_, created, err := h.Store.CreateBucket(ctx, r)
			requireError(t, err, s.ErrAccessDenied)
			if created {
				t.Fatal("denied create published a bucket")
			}
			if _, err := h.Store.GetBucket(ctx, r.Name); !errors.Is(err, s.ErrNotFound) {
				t.Fatalf("denied create left a bucket behind: %v", err)
			}
		})
	}
	t.Run("concurrent creates of one name have a single winner", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		var winners atomic.Int32
		ids := make(chan string, 12)
		var wg sync.WaitGroup
		for i := 0; i < 12; i++ {
			wg.Go(func() {
				b, created, err := h.Store.CreateBucket(ctx, s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "raced"})
				if err != nil {
					t.Error(err)
					return
				}
				if created {
					winners.Add(1)
				}
				ids <- b.ID
			})
		}
		wg.Wait()
		close(ids)
		if winners.Load() != 1 {
			t.Fatalf("create winners=%d", winners.Load())
		}
		first := ""
		for id := range ids {
			if first == "" {
				first = id
			} else if id != first {
				t.Fatal("loser did not receive the canonical bucket")
			}
		}
	})
	t.Run("recreated name is a new incarnation grants do not survive", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		first, _, err := h.Store.CreateBucket(ctx, s.BucketCreate{CredentialID: "writer-key", UserID: "writer", Name: "recycled"})
		if err != nil {
			t.Fatal(err)
		}
		// The writer owns it; the fixture owner receives a write grant on the
		// first incarnation, like the Admin API would issue.
		h.SetGrant(s.Grant{BucketID: first.ID, UserID: "owner", Write: true, Revision: 1})
		a, err := h.Store.Authorize(ctx, "owner-key", "recycled")
		if err != nil || !a.Write {
			t.Fatal("grant on the first incarnation is not effective", a, err)
		}
		h.RemoveBucket("recycled") // cleanup path, not the S3 DeleteBucket
		second, created, err := h.Store.CreateBucket(ctx, s.BucketCreate{CredentialID: "owner-key", UserID: "owner", Name: "recycled"})
		if err != nil || !created {
			t.Fatalf("recreate: created=%v err=%v", created, err)
		}
		if second.ID == first.ID {
			t.Fatal("recreated name reused the bucket_id")
		}
		a, err = h.Store.Authorize(ctx, "owner-key", "recycled")
		if err != nil || !a.Write {
			t.Fatal("owner does not own the recreation", a, err)
		}
		// The writer owned the first incarnation only: the second one keeps
		// the old grant ineffective even before its cleanup.
		a, err = h.Store.Authorize(ctx, "writer-key", "recycled")
		if err != nil || a.Read || a.Write || a.SystemAdmin {
			t.Fatal("second incarnation inherited access of the first", a, err)
		}
	})
}

func isLowerHex(id string) bool {
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
