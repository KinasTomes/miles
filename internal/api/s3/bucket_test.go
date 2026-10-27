package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

// buckets.json is the pinned contract of the P0 bucket operations (API v2
// 4.2 "Thao tác bucket"): bucket naming, CreateBucket with and against the
// configured region, the collision codes of a taken name, HeadBucket per
// principal and the ListBuckets XML of every access shape. Each row runs
// through the real Router and bucket handlers on the fake metadata store, in
// a fresh world per row like authorization.json.

type bucketFixture struct {
	Region    string
	CreatedAt string `json:"created_at"`
	Names     []struct {
		Name  string
		Valid bool
	}
	Create []struct {
		Name      string
		Principal string
		Body      string
		Status    int
		Code      ErrorCode
		Location  string
	}
	Head []struct {
		Name      string
		Principal string
		Status    int
		Region    string
	}
	List []struct {
		Principal string
		OwnerID   string `json:"owner_id"`
		XML       string
	}
}

// bucketPrincipals seeds every access shape of the fixture: the owner of
// demo, a read and a write grantee, a plain user, a system admin without
// grants, and the two auth-failure shapes (disabled user, revoked key).
func bucketPrincipals() (users []storage.User, credentials []storage.Credential) {
	shapes := []struct {
		id, name   string
		admin      bool
		inactive   bool
		revokedKey bool
	}{
		{"user-owner", "owner", false, false, false},
		{"user-reader", "reader", false, false, false},
		{"user-writer", "writer", false, false, false},
		{"user-plain", "plain", false, false, false},
		{"user-admin", "admin", true, false, false},
		{"user-disabled", "disabled", false, true, false},
		{"user-revoked", "revoked", false, false, true},
	}
	for _, s := range shapes {
		users = append(users, storage.User{ID: s.id, Active: !s.inactive, Revision: 1, DefaultDomain: "alpha"})
		credentials = append(credentials, storage.Credential{ID: accessKey(s.name), UserID: s.id, Active: !s.revokedKey, SystemAdmin: s.admin, Revision: 1})
	}
	return users, credentials
}

type bucketWorld struct {
	store  *fake.Metadata
	router Router
	users  []storage.User
}

func newBucketWorld(t *testing.T) *bucketWorld {
	t.Helper()
	users, credentials := bucketPrincipals()
	demo := storage.Bucket{Name: "demo", ID: "bkt-demo-0001", OwnerID: "user-owner", Domain: "alpha", Revision: 1, Active: true, CreatedAt: authNow}
	seed := fake.Seed{
		Now:         func() time.Time { return authNow },
		Buckets:     []storage.Bucket{demo},
		Users:       users,
		Credentials: credentials,
		Grants: []storage.Grant{
			{BucketID: demo.ID, UserID: "user-reader", Read: true, Revision: 1},
			{BucketID: demo.ID, UserID: "user-writer", Write: true, Revision: 1},
		},
	}
	secrets := map[string]string{}
	for _, c := range credentials {
		secrets[c.ID] = "secret-" + strings.ToLower(strings.TrimPrefix(c.ID, "AK"))
	}
	w := &bucketWorld{store: fake.NewMetadata(seed), users: users}
	// Access is the store itself, not auth.Snapshots: these tests change
	// fixtures between requests and must see the change immediately, while the
	// 5-second snapshot window is the auth package's own contract.
	w.router = Router{
		Region:      authRegion,
		Credentials: storeCredentials{w.store, secrets},
		Access:      w.store,
		Now:         func() time.Time { return authNow },
		Handlers:    NewBucketHandlers(w.store),
	}
	return w
}

// send signs and serves one bucket request of principal. Like the
// authorization suite it calls serve, so the S3 error of a rejected request
// comes back as an *Error with no body written, and a HEAD error still has a
// code to read.
func (w *bucketWorld) send(t *testing.T, method, path, principal, body string) (*httptest.ResponseRecorder, *Error) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	payloadHash := emptySHA256
	if body != "" {
		sum := sha256.Sum256([]byte(body))
		payloadHash = hex.EncodeToString(sum[:])
	}
	signPayload(r, accessKey(principal), "secret-"+principal, payloadHash)
	rec := httptest.NewRecorder()
	err := w.router.serve(rec, r, "BUCKETTEST")
	var e *Error
	if err != nil && !errors.As(err, &e) {
		t.Fatalf("%s %s by %s: %v", method, path, principal, err)
	}
	return rec, e
}

// checkErrorBody decodes the XML error body of an S3 response.
func checkErrorBody(t *testing.T, rec *httptest.ResponseRecorder) ErrorCode {
	t.Helper()
	var body struct {
		Code ErrorCode `xml:"Code"`
	}
	if err := xml.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body is not the S3 error document: %q", rec.Body.String())
	}
	return body.Code
}

func TestBucketNameRules(t *testing.T) {
	var f bucketFixture
	readFixture(t, "buckets.json", &f)
	for _, n := range f.Names {
		if got := validBucketName(n.Name); got != n.Valid {
			t.Errorf("validBucketName(%q) = %v, want %v", n.Name, got, n.Valid)
		}
	}
}

func TestBucketCreateFixtures(t *testing.T) {
	var f bucketFixture
	readFixture(t, "buckets.json", &f)
	if f.Region != authRegion {
		t.Fatalf("fixture region %q does not match the test region %q", f.Region, authRegion)
	}
	for _, c := range f.Create {
		t.Run(c.Name+" by "+c.Principal, func(t *testing.T) {
			w := newBucketWorld(t)
			rec, e := w.send(t, http.MethodPut, "/"+c.Name, c.Principal, c.Body)
			switch {
			case c.Code == "" && e != nil:
				t.Fatalf("unexpected error %v", e)
			case c.Code != "" && e == nil:
				t.Fatalf("no error, want %s (status %d)", c.Code, rec.Code)
			case e != nil:
				if e.Code != c.Code {
					t.Fatalf("code %s, want %s", e.Code, c.Code)
				}
				if e.Status() != c.Status {
					t.Fatalf("status %d, want %d", e.Status(), c.Status)
				}
			}
			if e == nil {
				if rec.Code != c.Status {
					t.Fatalf("status %d, want %d (body %q)", rec.Code, c.Status, rec.Body.String())
				}
				if got := rec.Header().Get("Location"); got != c.Location {
					t.Errorf("Location %q, want %q", got, c.Location)
				}
			}
		})
	}
}

func TestBucketHeadFixtures(t *testing.T) {
	var f bucketFixture
	readFixture(t, "buckets.json", &f)
	for _, h := range f.Head {
		t.Run(h.Name+" by "+h.Principal, func(t *testing.T) {
			w := newBucketWorld(t)
			rec, e := w.send(t, http.MethodHead, "/"+h.Name, h.Principal, "")
			if h.Status >= 400 {
				if e == nil || e.Status() != h.Status {
					t.Fatalf("error %v, want status %d", e, h.Status)
				}
				if rec.Body.Len() != 0 {
					t.Errorf("HEAD error carries a body: %q", rec.Body.String())
				}
				return
			}
			if e != nil {
				t.Fatalf("unexpected error %v", e)
			}
			if rec.Code != h.Status {
				t.Fatalf("status %d, want %d", rec.Code, h.Status)
			}
			if rec.Body.Len() != 0 {
				t.Errorf("HEAD response carries a body: %q", rec.Body.String())
			}
			if rec.Header().Get("x-amz-bucket-region") != f.Region {
				t.Errorf("x-amz-bucket-region %q, want %q", rec.Header().Get("x-amz-bucket-region"), f.Region)
			}
		})
	}
}

// TestBucketListFixtures pins the whole ListBuckets XML of every principal:
// only the buckets the caller owns or holds a grant on appear, and a system
// admin without grants lists nothing — admin is not implicit data access.
func TestBucketListFixtures(t *testing.T) {
	var f bucketFixture
	readFixture(t, "buckets.json", &f)
	for _, l := range f.List {
		t.Run(l.Principal, func(t *testing.T) {
			w := newBucketWorld(t)
			rec, e := w.send(t, http.MethodGet, "/", l.Principal, "")
			if e != nil {
				t.Fatalf("unexpected error %v", e)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d, want 200 (body %q)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/xml" {
				t.Errorf("Content-Type %q", got)
			}
			if rec.Body.String() != l.XML {
				t.Fatalf("body\n got %q\nwant %q", rec.Body.String(), l.XML)
			}
		})
	}
}

// TestBucketCreateIdentity pins the create-time identity of API v2 3.5: the
// owner is the creating user, the domain is a copy of that user's
// default_domain at create time (later changes never reach the bucket), and
// the bucket_id is a fresh 32-hex incarnation with CreatedAt set once.
func TestBucketCreateIdentity(t *testing.T) {
	w := newBucketWorld(t)
	rec, e := w.send(t, http.MethodPut, "/fresh", "owner", "")
	if e != nil || rec.Code != http.StatusOK {
		t.Fatalf("create: e=%v status=%d body=%q", e, rec.Code, rec.Body.String())
	}
	ctx := context.Background()
	b, err := w.store.GetBucket(ctx, "fresh")
	if err != nil {
		t.Fatal(err)
	}
	if b.OwnerID != "user-owner" || b.Domain != "alpha" || b.Revision != 1 || !b.Active || !b.CreatedAt.Equal(authNow) {
		t.Fatalf("bucket=%+v", b)
	}
	if len(b.ID) != 32 || strings.ToLower(b.ID) != b.ID {
		t.Fatalf("bucket_id %q is not 32 lowercase hex characters", b.ID)
	}
	// A later default-domain change only affects buckets created after it.
	changed := w.user(t, "user-owner")
	changed.DefaultDomain = "beta"
	w.store.SetUser(changed)
	rec, e = w.send(t, http.MethodPut, "/fresh-2", "owner", "")
	if e != nil || rec.Code != http.StatusOK {
		t.Fatalf("second create: e=%v status=%d body=%q", e, rec.Code, rec.Body.String())
	}
	if b2, err := w.store.GetBucket(ctx, "fresh-2"); err != nil || b2.Domain != "beta" {
		t.Fatalf("fresh-2 domain: %+v %v", b2, err)
	}
	if b1, err := w.store.GetBucket(ctx, "fresh"); err != nil || b1.Domain != "alpha" {
		t.Fatalf("fresh domain changed after create: %+v %v", b1, err)
	}
}

// TestBucketRecreateChangesIdentity pins the incarnation rule: a recreated
// name gets a new bucket_id and the grants of the deleted incarnation stop
// working, because grants bind the bucket_id, never the name.
func TestBucketRecreateChangesIdentity(t *testing.T) {
	w := newBucketWorld(t)
	if rec, e := w.send(t, http.MethodPut, "/recycled", "owner", ""); e != nil || rec.Code != http.StatusOK {
		t.Fatalf("create: e=%v status=%d", e, rec.Code)
	}
	ctx := context.Background()
	first, err := w.store.GetBucket(ctx, "recycled")
	if err != nil {
		t.Fatal(err)
	}
	w.store.SetGrant(storage.Grant{BucketID: first.ID, UserID: "user-reader", Read: true, Revision: 1})
	if rec, e := w.send(t, http.MethodHead, "/recycled", "reader", ""); e != nil || rec.Code != http.StatusOK {
		t.Fatalf("granted HEAD: e=%v status=%d", e, rec.Code)
	}
	w.store.RemoveBucket("recycled")
	if rec, e := w.send(t, http.MethodPut, "/recycled", "owner", ""); e != nil || rec.Code != http.StatusOK {
		t.Fatalf("recreate: e=%v status=%d body=%q", e, rec.Code, rec.Body.String())
	}
	second, err := w.store.GetBucket(ctx, "recycled")
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("recreated name reused the bucket_id")
	}
	if rec, e := w.send(t, http.MethodHead, "/recycled", "reader", ""); e == nil || e.Code != ErrAccessDenied {
		t.Fatalf("stale grant survived the recreation: e=%v status=%d", e, rec.Code)
	}
}

// TestBucketCreateRechecksCaller pins the transaction-level recheck of API v2
// 2.2.3: a user disabled after the router admitted the request is
// AccessDenied from the CreateBucket transaction, not a published bucket.
func TestBucketCreateRechecksCaller(t *testing.T) {
	w := newBucketWorld(t)
	r := httptest.NewRequest(http.MethodPut, "/fresh", nil)
	req := &Request{Route: Route{Op: CreateBucket, Bucket: "fresh"}, HTTP: r, Caller: Caller{AccessKeyID: accessKey("owner"), UserID: "user-owner"}, Body: newPayload(r, CreateBucket, nil), region: authRegion}
	disabled := w.user(t, "user-owner")
	disabled.Active = false
	w.store.SetUser(disabled)
	err := (&Buckets{Store: w.store}).CreateBucket(httptest.NewRecorder(), req)
	e, ok := err.(*Error)
	if !ok || e.Code != ErrAccessDenied {
		t.Fatalf("err = %v, want AccessDenied", err)
	}
	if _, err := w.store.GetBucket(context.Background(), "fresh"); err == nil {
		t.Fatal("denied create published a bucket")
	}
}

// user returns the seeded user record so a test can change it with
// store.SetUser; the fake store itself exposes no user reader.
func (w *bucketWorld) user(t *testing.T, id string) storage.User {
	t.Helper()
	for _, u := range w.users {
		if u.ID == id {
			return u
		}
	}
	t.Fatalf("user %s not seeded", id)
	return storage.User{}
}
