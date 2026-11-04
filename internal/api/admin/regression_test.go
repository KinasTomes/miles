package admin

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
)

// TestUnknownPrivilegeFieldsWriteNothing: every Admin write body that names a
// field outside its schema, privilege-shaped or nested, is 400 before any
// write, for the system admin too (API §6.5.1).
func TestUnknownPrivilegeFieldsWriteNothing(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	w.opts.Operations = testOperations{w.store, jobs.Service{Store: w.store, Policies: w.store}}
	var err error
	if w.h, err = NewServer(w.opts); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	count := func() string {
		tokens, err := w.store.ListAdminTokens(ctx, storage.IdentityListRequest{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		creds, err := w.store.ListCredentials(ctx, storage.IdentityListRequest{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		u, err := w.store.GetUser(ctx, "user-writer")
		if err != nil {
			t.Fatal(err)
		}
		p, err := w.store.GetPolicy(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		list, err := w.opts.Operations.ListJobs(ctx, jobs.PageRequest{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		return fmt.Sprint(len(tokens.Items), len(creds.Items), u.Revision, u.Active, p.Revision, len(list.Jobs))
	}
	before := count()
	for _, r := range []struct{ method, path, body string }{
		{"PATCH", "/admin/v1/users/user-writer", `{"status":"active","system_admin":true,"expected_revision":1}`},
		{"PATCH", "/admin/v1/users/user-writer", `{"logical_limit":5,"scope":"system","expected_revision":1}`},
		{"POST", "/admin/v1/admin-tokens", `{"user_id":"user-writer","scope":"system"}`},
		{"POST", "/admin/v1/admin-tokens", `{"user_id":"user-writer","owner":true}`},
		{"POST", "/admin/v1/credentials", `{"user_id":"user-writer","owner":"user-owner"}`},
		{"POST", "/admin/v1/credentials", `{"user_id":"user-writer","grants":{"demo":"rw"}}`},
		{"PUT", "/admin/v1/policy", `{"admin":true,"expected_revision":1}`},
		{"PUT", "/admin/v1/policy", `{"tiering":{"enabled":false,"owner":"user-writer"},"expected_revision":1}`},
		{"POST", "/admin/v1/jobs", `{"type":"gc","actor":"system-root"}`},
		{"POST", "/admin/v1/cache/purge", `{"scope":"cluster","force":true}`},
		{"POST", "/admin/v1/cluster/nodes/node-a/drain", `{"system_admin":true}`},
	} {
		w.expect(w.do(r.method, r.path, w.root, r.body), http.StatusBadRequest, "invalid_request")
	}
	if after := count(); after != before {
		t.Fatalf("rejected bodies wrote state: %s -> %s", before, after)
	}
}

// TestRecreatedBucketRejectsStaleBucketID: a bucket deleted and created again
// under the same name is a new bucket_id. Writes that carry the old id are
// 409 bucket_id_mismatch; grants of the old incarnation do not reappear.
func TestRecreatedBucketRejectsStaleBucketID(t *testing.T) {
	w := newS3World(t)
	ctx := context.Background()
	key := w.credential("user-owner", false)
	old, err := w.store.GetBucket(ctx, "demo")
	if err != nil {
		t.Fatal(err)
	}
	admit := func() context.Context {
		m, err := w.opts.Admit(ctx, tokenID(w.root))
		if err != nil {
			t.Fatal(err)
		}
		return storage.WithMutation(ctx, m)
	}
	if err = w.store.DeleteBucket(admit(), storage.BucketDelete{CredentialID: key.id, Bucket: old}); err != nil {
		t.Fatal(err)
	}
	fresh, created, err := w.store.CreateBucket(admit(), storage.BucketCreate{CredentialID: key.id, UserID: "user-owner", Name: "demo"})
	if err != nil || !created || fresh.ID == old.ID {
		t.Fatal(fresh, created, err)
	}
	owner := w.bearer["owner"]
	grant := "/admin/v1/buckets/demo/grants/user-stranger"
	page := decode[struct {
		Items []grantJSON `json:"items"`
	}](t, w.must(w.do("GET", "/admin/v1/buckets/demo/grants", owner, ""), http.StatusOK))
	if len(page.Items) != 0 {
		t.Fatalf("old incarnation grants listed on the new bucket: %+v", page.Items)
	}
	if g, err := w.store.GetGrant(ctx, fresh.ID, "user-reader"); err != storage.ErrNotFound {
		t.Fatal("old grant inherited", g, err)
	}
	// A missing grant stays 404 before the bucket_id check (API §6.5.1 order);
	// once the new incarnation has it, the stale id is a conflict.
	w.expect(w.do("DELETE", "/admin/v1/buckets/demo/grants/user-reader?expected_bucket_id="+old.ID, owner, ""), http.StatusNotFound, "grant_not_found")
	w.must(w.do("PUT", "/admin/v1/buckets/demo/grants/user-reader", owner, `{"read":true,"expected_revision":0,"expected_bucket_id":"`+fresh.ID+`"}`), http.StatusOK)
	for _, r := range []struct{ method, path, body string }{
		{"PUT", grant, `{"read":true,"expected_revision":0,"expected_bucket_id":"` + old.ID + `"}`},
		{"DELETE", "/admin/v1/buckets/demo/grants/user-reader?expected_bucket_id=" + old.ID, ""},
		{"PUT", "/admin/v1/buckets/demo/policy", `{"cache":{"readahead_chunks":2},"expected_revision":1,"expected_bucket_id":"` + old.ID + `"}`},
	} {
		w.expect(w.do(r.method, r.path, owner, r.body), http.StatusConflict, "bucket_id_mismatch")
	}
	if g := w.grant("user-stranger"); g.Revision != 0 {
		t.Fatal("stale bucket_id wrote the old incarnation", g)
	}
	if g, err := w.store.GetGrant(ctx, fresh.ID, "user-reader"); err != nil || !g.Read {
		t.Fatal("stale bucket_id removed the new grant", g, err)
	}
}

// TestConcurrentCASHasOneWinner: parallel writers with the same expected
// revision of a grant, the global policy or a bucket policy; exactly one
// commits and every other gets 409 revision_conflict.
func TestConcurrentCASHasOneWinner(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	owner := w.bearer["owner"]
	w.must(w.do("GET", "/admin/v1/buckets/demo/policy", owner, ""), http.StatusOK) // materializes revision 1
	for _, r := range []struct {
		name, path, bearer string
		body               func(int) string
		revision           func() uint64
	}{
		{"grant create", "/admin/v1/buckets/demo/grants/user-stranger", owner,
			func(i int) string {
				return fmt.Sprintf(`{"read":%t,"write":%t,"expected_revision":0,"expected_bucket_id":%q}`, i%2 == 0, i%2 == 1, demoID)
			},
			func() uint64 { return w.grant("user-stranger").Revision }},
		{"global policy", "/admin/v1/policy", w.root,
			func(i int) string {
				return fmt.Sprintf(`{"tiering":{"cold_after_days":%d},"expected_revision":1}`, 31+i)
			},
			func() uint64 {
				p, err := w.store.GetPolicy(context.Background(), 0)
				if err != nil {
					t.Fatal(err)
				}
				return p.Revision
			}},
		{"bucket policy", "/admin/v1/buckets/demo/policy", owner,
			func(i int) string {
				return fmt.Sprintf(`{"cache":{"readahead_chunks":%d},"expected_revision":1,"expected_bucket_id":%q}`, i+1, demoID)
			},
			func() uint64 {
				p, err := w.store.GetBucketPolicy(context.Background(), "demo", demoID, 0)
				if err != nil {
					t.Fatal(err)
				}
				return p.Revision
			}},
	} {
		t.Run(r.name, func(t *testing.T) {
			before := r.revision()
			const writers = 16
			codes := make([]string, writers)
			var wg sync.WaitGroup
			for i := range writers {
				wg.Go(func() {
					rec := w.do("PUT", r.path, r.bearer, r.body(i))
					codes[i] = fmt.Sprint(rec.Code)
					if rec.Code != http.StatusOK {
						codes[i] += " " + code(t, rec)
					}
				})
			}
			wg.Wait()
			won := 0
			for _, c := range codes {
				switch c {
				case "200":
					won++
				case "409 revision_conflict":
				default:
					t.Errorf("unexpected outcome %s", c)
				}
			}
			if won != 1 || r.revision() != before+1 {
				t.Fatalf("winners %d, revision %d -> %d: %v", won, before, r.revision(), codes)
			}
		})
	}
}
