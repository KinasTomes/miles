package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
)

var testNow = time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)

const demoID = "bkt-demo-0001"

// authFixture mirrors tests/fixtures/s3/authorization.json (API §2.2.3).
type authFixture struct {
	Bucket struct {
		Bucket      string
		BucketID    string `json:"bucket_id"`
		OwnerID     string `json:"owner_id"`
		DedupDomain string `json:"dedup_domain"`
	}
	Principals map[string]struct {
		UserID      string `json:"user_id"`
		SystemAdmin bool   `json:"system_admin"`
		Read, Write bool
		UserStatus  string `json:"user_status"`
		AuthStatus  string `json:"auth_status"`
	}
	Admin []struct {
		Endpoint, Requires string
		UserID             string `json:"user_id"`
		Expect             map[string]string
	}
}

// world is the Admin server over a fresh Badger store holding the
// authorization fixture: bucket demo of user-owner, one user per principal
// with its grant, a bearer per principal (revoked for "revoked") and the
// system token of system-root from the offline bootstrap.
type world struct {
	t      *testing.T
	f      authFixture
	store  *badger.Store
	clk    *clock.Virtual
	sealer Sealer
	h      http.Handler
	log    *bytes.Buffer
	root   string
	bearer map[string]string
	opts   Options
}

func newWorld(t *testing.T, extra badger.Bootstrap) *world {
	t.Helper()
	ctx := context.Background()
	w := &world{t: t, log: &bytes.Buffer{}, bearer: map[string]string{}}
	raw, err := os.ReadFile("../../../tests/fixtures/s3/authorization.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &w.f); err != nil {
		t.Fatal(err)
	}
	if w.clk, err = clock.NewVirtual(testNow, 100); err != nil {
		t.Fatal(err)
	}
	w.store, err = badger.Open(badger.Options{Path: t.TempDir(), RequireProducers: true, Now: func() time.Time { return w.clk.Now().WallUTC }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.store.Close() })

	b := w.f.Bucket
	seed := extra
	seed.Buckets = append(seed.Buckets, storage.Bucket{Name: b.Bucket, ID: b.BucketID, OwnerID: b.OwnerID, Domain: b.DedupDomain, Revision: 1, Active: true, CreatedAt: testNow})
	for _, p := range w.f.Principals {
		seed.Users = append(seed.Users, storage.User{ID: p.UserID, Active: p.UserStatus != "disabled", Revision: 1, DefaultDomain: "default"})
		if p.UserID != b.OwnerID && (p.Read || p.Write) {
			seed.Grants = append(seed.Grants, storage.Grant{BucketID: b.BucketID, UserID: p.UserID, Read: p.Read, Write: p.Write, Revision: 1})
		}
	}
	seeded := map[string]bool{}
	for _, q := range extra.Quotas {
		seeded[q.OwnerID] = true
	}
	for _, u := range seed.Users {
		if !seeded[u.ID] {
			seed.Quotas = append(seed.Quotas, storage.LogicalQuota{OwnerID: u.ID, Limit: 1 << 40})
		}
	}
	if err := w.store.Bootstrap(ctx, seed); err != nil {
		t.Fatal(err)
	}

	v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		t.Fatal(err)
	}
	g, err := v.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	policy, err := g.StorageSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	policy.Revision = 0
	id, secret, token, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(secret))
	if err := w.store.BootstrapAdmin(ctx, storage.AdminBootstrap{QuotaLimit: 1 << 40, Policy: policy, Token: storage.AdminToken{
		ID: id, UserID: "system-root", Scope: storage.TokenScopeSystem, Hash: hash[:], Active: true, IssuedAt: testNow, ExpiresAt: testNow.Add(24 * time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	w.root = token
	authority, err := w.store.StartLocalRuntime(ctx, "system-root", 1<<40)
	if err != nil {
		t.Fatal(err)
	}

	if w.sealer, err = NewAESSealer("test-aes-gcm", bytes.Repeat([]byte{7}, 32)); err != nil {
		t.Fatal(err)
	}
	w.opts = Options{
		Store: w.store,
		Admit: func(ctx context.Context, tokenID string) (storage.MutationContext, error) {
			var op [16]byte
			rand.Read(op[:])
			return authority.AdmitAdmin(ctx, tokenID, hex.EncodeToString(op[:]), w.clk.Now().WallUTC.Add(30*time.Second))
		},
		Sealer:  w.sealer,
		Cursors: Keyring{Current: "k1", Keys: map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32)}},
		Clock:   w.clk,
		Logger:  slog.New(slog.NewTextHandler(w.log, nil)),
	}
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}

	for name, p := range w.f.Principals {
		if p.UserID == "system-root" {
			w.bearer[name] = w.root
			continue
		}
		w.bearer[name] = w.mintToken(p.UserID, p.SystemAdmin)
		if p.AuthStatus == "revoked" {
			w.must(w.do("DELETE", "/admin/v1/admin-tokens/"+tokenID(w.bearer[name]), w.root, ""), http.StatusNoContent)
		}
	}
	return w
}

func tokenID(bearer string) string { id, _, _ := strings.Cut(bearer, "."); return id }

func (w *world) mintToken(user string, admin bool) string {
	w.t.Helper()
	rec := w.must(w.do("POST", "/admin/v1/admin-tokens", w.root, fmt.Sprintf(`{"user_id":%q,"system_admin":%t}`, user, admin)), http.StatusCreated)
	return decode[struct{ Token string }](w.t, rec).Token
}

// do sends one Admin request; header holds name/value pairs.
func (w *world) do(method, target, bearer, body string, header ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	for i := 0; i+1 < len(header); i += 2 {
		r.Header.Add(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, r)
	return rec
}

func (w *world) must(rec *httptest.ResponseRecorder, status int) *httptest.ResponseRecorder {
	w.t.Helper()
	if rec.Code != status {
		w.t.Fatalf("got %d %s, want %d", rec.Code, rec.Body, status)
	}
	return rec
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return v
}

// code is the error code of an Admin error response.
func code(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	e := decode[struct {
		Error struct {
			Code, Message string
			RequestID     string `json:"request_id"`
		}
	}](t, rec)
	if e.Error.RequestID == "" || e.Error.RequestID != rec.Header().Get("X-Request-Id") {
		t.Errorf("error without request id: %s", rec.Body)
	}
	return e.Error.Code
}

// expect checks status and error code of an Admin response.
func (w *world) expect(rec *httptest.ResponseRecorder, status int, want string) {
	w.t.Helper()
	if rec.Code != status {
		w.t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body, status, want)
	}
	if want != "" {
		if got := code(w.t, rec); got != want {
			w.t.Fatalf("got code %s, want %s", got, want)
		}
	}
}

func (w *world) grant(user string) storage.Grant {
	g, err := w.store.GetGrant(context.Background(), demoID, user)
	if err != nil && err != storage.ErrNotFound {
		w.t.Fatal(err)
	}
	return g
}

// TestAdminAuthorizationFixture runs every Admin row of authorization.json
// through the server for every principal: allow is a 2xx answer.
func TestAdminAuthorizationFixture(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	w.opts.Statistics = &fixtureStatistics{}
	var err error
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range w.f.Admin {
		method, _, _ := strings.Cut(row.Endpoint, " ")
		for name := range w.f.Principals {
			if _, ok := row.Expect[name]; !ok {
				t.Errorf("%s: no cell for %s", row.Endpoint, name)
			}
		}
		for name, want := range row.Expect {
			target, body := w.prepare(row.Endpoint)
			rec := w.do(method, target, w.bearer[name], body)
			got := strconv.Itoa(rec.Code)
			if rec.Code/100 == 2 {
				got = "allow"
			}
			if got != want {
				t.Errorf("%s by %s: got %s (%s), want %s", row.Endpoint, name, got, strings.TrimSpace(rec.Body.String()), want)
			}
		}
	}

}

// prepare returns the target and body of a fixture row that succeeds for
// an allowed caller in the current state of the world.
func (w *world) prepare(endpoint string) (string, string) {
	w.t.Helper()
	method, target, _ := strings.Cut(endpoint, " ")
	switch endpoint {
	case "PUT /admin/v1/buckets/demo/grants/user-reader":
		g := w.grant("user-reader")
		rev := g.Revision
		if !g.Read && !g.Write {
			rev = 0
		}
		return target, fmt.Sprintf(`{"read":true,"expected_revision":%d,"expected_bucket_id":%q}`, rev, demoID)
	case "DELETE /admin/v1/buckets/demo/grants/user-reader":
		if g := w.grant("user-reader"); !g.Read && !g.Write {
			w.must(w.do("PUT", target, w.root, fmt.Sprintf(`{"read":true,"expected_revision":0,"expected_bucket_id":%q}`, demoID)), http.StatusOK)
		}
		return target + "?expected_bucket_id=" + demoID, ""
	case "PUT /admin/v1/buckets/demo/policy":
		p := decode[struct{ Revision uint64 }](w.t, w.must(w.do("GET", target, w.root, ""), http.StatusOK))
		return target, fmt.Sprintf(`{"expected_revision":%d,"expected_bucket_id":%q}`, p.Revision, demoID)
	case "DELETE /admin/v1/admin-tokens/{token of user-writer}":
		return "/admin/v1/admin-tokens/" + tokenID(w.mintToken("user-writer", false)), ""
	case "PATCH /admin/v1/users/user-writer":
		u, err := w.store.GetUser(context.Background(), "user-writer")
		if err != nil {
			w.t.Fatal(err)
		}
		return target, fmt.Sprintf(`{"status":"active","expected_revision":%d}`, u.Revision)
	case "POST /admin/v1/credentials":
		return target, `{"user_id":"user-writer"}`
	}
	if method != "GET" {
		w.t.Fatalf("%s: no request body for this row", endpoint)
	}
	return target, ""
}

// TestErrorOrder pins API §6.5.1: 401, 404 bucket, 403, 400, 404 resource,
// 409 bucket_id_mismatch, 409 revision_conflict.
func TestErrorOrder(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	owner, stranger, writer := w.bearer["owner"], w.bearer["stranger"], w.bearer["writer"]
	grant := "/admin/v1/buckets/demo/grants/user-stranger"
	good := fmt.Sprintf(`{"read":true,"expected_revision":0,"expected_bucket_id":%q}`, demoID)

	rec := w.do("PUT", "/admin/v1/buckets/missing/grants/x", "", "{")
	w.expect(rec, http.StatusUnauthorized, "unauthorized")
	if rec.Header().Get("WWW-Authenticate") != "Bearer" {
		t.Error("401 without WWW-Authenticate: Bearer")
	}
	bad := w.do("GET", "/admin/v1/users", tokenID(owner)+"."+strings.Repeat("A", 43), "")
	w.expect(bad, http.StatusUnauthorized, "unauthorized")
	if code(t, rec) != code(t, bad) || decode[map[string]map[string]string](t, rec)["error"]["message"] != decode[map[string]map[string]string](t, bad)["error"]["message"] {
		t.Error("401 responses differ by reason")
	}
	w.expect(w.do("PUT", "/admin/v1/buckets/missing/grants/x", stranger, "{"), http.StatusNotFound, "bucket_not_found")
	w.expect(w.do("PUT", grant, stranger, "{"), http.StatusForbidden, "forbidden")
	w.expect(w.do("PUT", "/admin/v1/buckets/demo/grants/nobody", owner, `{"read":1,"expected_revision":0,"expected_bucket_id":"other"}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PUT", "/admin/v1/buckets/demo/grants/nobody", owner, `{"read":true,"expected_revision":5,"expected_bucket_id":"other"}`), http.StatusNotFound, "user_not_found")
	w.expect(w.do("PUT", grant, owner, `{"read":true,"expected_revision":5,"expected_bucket_id":"other"}`), http.StatusConflict, "bucket_id_mismatch")
	w.expect(w.do("PUT", grant, owner, fmt.Sprintf(`{"read":true,"expected_revision":5,"expected_bucket_id":%q}`, demoID)), http.StatusConflict, "revision_conflict")
	w.expect(w.do("PUT", grant, owner, good), http.StatusOK, "")

	// DELETE token: the token is looked up for the role check (step 3).
	missing := strings.Repeat("0", 32)
	w.expect(w.do("DELETE", "/admin/v1/admin-tokens/"+missing, writer, ""), http.StatusForbidden, "forbidden")
	w.expect(w.do("DELETE", "/admin/v1/admin-tokens/"+tokenID(owner), writer, ""), http.StatusForbidden, "forbidden")
	w.expect(w.do("DELETE", "/admin/v1/admin-tokens/not-hex", writer, ""), http.StatusForbidden, "forbidden")
	w.expect(w.do("DELETE", "/admin/v1/admin-tokens/"+missing, w.root, ""), http.StatusNotFound, "token_not_found")
	w.expect(w.do("DELETE", "/admin/v1/admin-tokens/not-hex", w.root, ""), http.StatusBadRequest, "invalid_request")

	// Unknown routes are a plain 404, a wrong method 405.
	if rec := w.do("GET", "/admin/v1/unknown", w.root, ""); rec.Code != http.StatusNotFound || rec.Header().Get("Content-Type") == "application/json" {
		t.Errorf("unknown route: %d %s", rec.Code, rec.Body)
	}
	if rec := w.do("DELETE", "/admin/v1/users", w.root, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: %d", rec.Code)
	}
	w.expect(w.do("GET", "/admin/v1/users?limit=10&x=1", w.root, ""), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("GET", "/admin/v1/users/user-writer/usage?detail=yes", w.root, ""), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("GET", "/admin/v1/users/user-owner/usage", writer, ""), http.StatusForbidden, "forbidden")
	w.expect(w.do("GET", "/admin/v1/users/nobody", w.root, ""), http.StatusNotFound, "user_not_found")
	w.expect(w.do("DELETE", "/admin/v1/credentials/AAAAAAAAAAAAAAAAAAAA", w.root, ""), http.StatusNotFound, "credential_not_found")
}

// TestRevisions covers If-Match/expected_revision, no-op, CAS conflict and
// revision_exhausted (API §6.5.2) on users, grants and both policies.
func TestRevisions(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{
		Users:  []storage.User{{ID: "user-max", Active: true, Revision: MaxRevision, DefaultDomain: "default"}},
		Grants: []storage.Grant{{BucketID: demoID, UserID: "user-stranger", Read: true, Revision: MaxRevision}},
	})
	type user struct {
		Status        string
		DefaultDomain string `json:"default_domain"`
		LogicalLimit  int64  `json:"logical_limit"`
		Revision      uint64
	}
	path := "/admin/v1/users/user-writer"
	w.expect(w.do("PATCH", path, w.root, `{"status":"active"}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PATCH", path, w.root, `{"status":"active","expected_revision":0}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PATCH", path, w.root, `{"expected_revision":1}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PATCH", path, w.root, `{"status":"active","expected_revision":1}`, "If-Match", `"2"`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PATCH", path, w.root, `{"status":"active"}`, "If-Match", `W/"1"`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PATCH", path, w.root, `{"status":"bogus","expected_revision":1}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PATCH", path, w.root, `{"status":"active","expected_revision":2}`), http.StatusConflict, "revision_conflict")
	rec := w.must(w.do("PATCH", path, w.root, `{"status":"active"}`, "If-Match", `"1"`), http.StatusOK)
	if u := decode[user](t, rec); u.Revision != 1 || rec.Header().Get("ETag") != `"1"` {
		t.Fatalf("no-op changed revision: %+v %s", u, rec.Header().Get("ETag"))
	}
	rec = w.must(w.do("PATCH", path, w.root, `{"default_domain":"team-a","logical_limit":1024,"expected_revision":1}`), http.StatusOK)
	if u := decode[user](t, rec); u.Revision != 2 || u.DefaultDomain != "team-a" || u.LogicalLimit != 1024 || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("patch: %+v", u)
	}
	w.expect(w.do("PATCH", path, w.root, `{"status":"disabled","expected_revision":1}`), http.StatusConflict, "revision_conflict")
	if u := decode[user](t, w.must(w.do("GET", path, w.root, ""), http.StatusOK)); u.Revision != 2 || u.LogicalLimit != 1024 || u.Status != "active" {
		t.Fatalf("get after patch: %+v", u)
	}

	max := strconv.FormatUint(MaxRevision, 10)
	w.must(w.do("PATCH", "/admin/v1/users/user-max", w.root, `{"status":"active","expected_revision":`+max+`}`), http.StatusOK)
	w.expect(w.do("PATCH", "/admin/v1/users/user-max", w.root, `{"status":"disabled","expected_revision":`+max+`}`), http.StatusConflict, "revision_exhausted")

	// Grants: no-op at max is 200, a change 409; a tombstone is no grant.
	stranger := "/admin/v1/buckets/demo/grants/user-stranger"
	w.must(w.do("PUT", stranger, w.root, `{"read":true,"expected_revision":`+max+`,"expected_bucket_id":"`+demoID+`"}`), http.StatusOK)
	w.expect(w.do("PUT", stranger, w.root, `{"write":true,"expected_revision":`+max+`,"expected_bucket_id":"`+demoID+`"}`), http.StatusConflict, "revision_exhausted")
	w.expect(w.do("DELETE", stranger+"?expected_bucket_id="+demoID, w.root, ""), http.StatusConflict, "revision_exhausted")

	reader := "/admin/v1/buckets/demo/grants/user-reader"
	w.expect(w.do("DELETE", reader, w.root, ""), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("DELETE", reader+"?expected_bucket_id=other", w.root, ""), http.StatusConflict, "bucket_id_mismatch")
	w.must(w.do("DELETE", reader+"?expected_bucket_id="+demoID, w.root, ""), http.StatusNoContent)
	w.expect(w.do("DELETE", reader+"?expected_bucket_id="+demoID, w.root, ""), http.StatusNotFound, "grant_not_found")
	if g := w.grant("user-reader"); g.Read || g.Write || g.Revision != 2 {
		t.Fatalf("tombstone: %+v", g)
	}
	w.expect(w.do("PUT", reader, w.root, `{"read":true,"expected_revision":2,"expected_bucket_id":"`+demoID+`"}`), http.StatusConflict, "revision_conflict")
	w.expect(w.do("PUT", reader, w.root, `{"expected_revision":0,"expected_bucket_id":"`+demoID+`"}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PUT", reader, w.root, `{"read":false,"write":false,"expected_revision":0,"expected_bucket_id":"`+demoID+`"}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PUT", "/admin/v1/buckets/demo/grants/user-owner", w.root, `{"read":true,"expected_revision":0,"expected_bucket_id":"`+demoID+`"}`), http.StatusBadRequest, "invalid_request")
	rec = w.must(w.do("PUT", reader, w.root, `{"write":true,"expected_bucket_id":"`+demoID+`"}`, "If-Match", `"0"`), http.StatusBadRequest)
	rec = w.must(w.do("PUT", reader, w.bearer["owner"], `{"write":true,"expected_revision":0,"expected_bucket_id":"`+demoID+`"}`), http.StatusOK)
	if g := decode[grantJSON](t, rec); g.Read || !g.Write || g.Revision != 3 || g.Bucket != "demo" || g.BucketID != demoID || rec.Header().Get("ETag") != `"3"` {
		t.Fatalf("grant over tombstone: %+v", g)
	}

	// Bucket policy: first use materializes revision 1, the default.
	bp := "/admin/v1/buckets/demo/policy"
	type bucketPolicy struct {
		Bucket   string
		BucketID string `json:"bucket_id"`
		Revision uint64
		Cache    config.CachePolicy
	}
	if p := decode[bucketPolicy](t, w.must(w.do("GET", bp, w.bearer["owner"], ""), http.StatusOK)); p.Revision != 1 || p.Cache != config.DefaultCachePolicy() || p.BucketID != demoID {
		t.Fatalf("first bucket policy: %+v", p)
	}
	w.expect(w.do("PUT", bp, w.bearer["owner"], `{"cache":{"readahead_chunks":8},"expected_revision":1,"expected_bucket_id":"other"}`), http.StatusConflict, "bucket_id_mismatch")
	w.expect(w.do("PUT", bp, w.bearer["owner"], `{"cache":{"readahead_chunks":8},"expected_revision":2,"expected_bucket_id":"`+demoID+`"}`), http.StatusConflict, "revision_conflict")
	w.expect(w.do("PUT", bp, w.bearer["owner"], `{"cache":{"readahead_chunks":65},"expected_revision":1,"expected_bucket_id":"`+demoID+`"}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("PUT", bp, w.bearer["owner"], `{"cache":{"readahead_chunks":8},"expected_bucket_id":"`+demoID+`"}`), http.StatusBadRequest, "invalid_request")
	if p := decode[bucketPolicy](t, w.must(w.do("PUT", bp, w.bearer["owner"], `{"cache":{"readahead_chunks":8},"expected_bucket_id":"`+demoID+`"}`, "If-Match", `"1"`), http.StatusOK)); p.Revision != 2 || p.Cache.ReadaheadChunks != 8 || !p.Cache.AdmitOnWrite {
		t.Fatalf("bucket policy put: %+v", p)
	}
	if p := decode[bucketPolicy](t, w.must(w.do("PUT", bp, w.bearer["owner"], `{"cache":{"readahead_chunks":8},"expected_revision":2,"expected_bucket_id":"`+demoID+`"}`), http.StatusOK)); p.Revision != 2 {
		t.Fatalf("bucket policy no-op: %+v", p)
	}
	b := decode[map[string]any](t, w.must(w.do("GET", "/admin/v1/buckets/demo", w.bearer["owner"], ""), http.StatusOK))
	if b["cache_policy_revision"] != float64(2) || b["owner_id"] != "user-owner" || b["state"] != "active" || b["dedup_domain"] != "default" {
		t.Fatalf("bucket: %v", b)
	}

	// Global policy: revision 1 from the bootstrap.
	type global struct {
		Revision uint64
		config.GlobalPolicy
	}
	if p := decode[global](t, w.must(w.do("GET", "/admin/v1/policy", w.root, ""), http.StatusOK)); p.Revision != 1 || p.GlobalPolicy != config.DefaultGlobalPolicy() {
		t.Fatalf("global policy: %+v", p)
	}
	w.expect(w.do("GET", "/admin/v1/policy", w.bearer["owner"], ""), http.StatusForbidden, "forbidden")
	w.expect(w.do("PUT", "/admin/v1/policy", w.root, `{"tiering":{"cold_storage_class":"GLACIER","auto_restore_tier":"Bulk"},"expected_revision":2}`), http.StatusConflict, "revision_conflict")
	w.expect(w.do("PUT", "/admin/v1/policy", w.root, `{"tiering":{"cold_storage_class":"DEEP_ARCHIVE"},"expected_revision":1}`), http.StatusBadRequest, "invalid_request")
	w.must(w.do("PUT", "/admin/v1/policy", w.root, `{"chunking":{"min_bytes":262144},"expected_revision":1}`), http.StatusOK)
	rec = w.must(w.do("PUT", "/admin/v1/policy", w.root, `{"tiering":{"cold_storage_class":"GLACIER","auto_restore_tier":"Bulk"},"expected_revision":1}`), http.StatusOK)
	if p := decode[global](t, rec); p.Revision != 2 || p.Tiering.ColdStorageClass != storage.Glacier || rec.Header().Get("ETag") != `"2"` {
		t.Fatalf("global policy put: %+v", p)
	}
}

// TestStrictJSON rejects every malformed or over-privileged body with 400
// and writes nothing (API §6.5.1).
func TestStrictJSON(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	for _, body := range []string{
		``, `[]`, `"x"`, `{`, `{"user_id":"u1"} {}`, `{"user_id":"u1","user_id":"u2"}`, `{"user_id":null}`,
		`{"user_id":"u1","admin":true}`, `{"user_id":"U1"}`, `{"user_id":"u1","logical_limit":1.0}`,
		`{"user_id":"u1","logical_limit":1e3}`, `{"user_id":"u1","logical_limit":-1}`, `{"user_id":"u1","logical_limit":9007199254740992}`,
		`{"user_id":"u1","logical_limit":"5"}`, `{"default_domain":"d"}`, `{"user_id":"u1","pad":"` + strings.Repeat("x", 16<<10) + `"}`,
	} {
		w.expect(w.do("POST", "/admin/v1/users", w.root, body), http.StatusBadRequest, "invalid_request")
	}
	if _, err := w.store.GetUser(context.Background(), "u1"); err != storage.ErrNotFound {
		t.Fatal("rejected body wrote a user", err)
	}
	grant := "/admin/v1/buckets/demo/grants/user-stranger"
	for _, body := range []string{
		`{"read":true,"owner":true,"expected_revision":0,"expected_bucket_id":"` + demoID + `"}`,
		`{"read":true,"system_admin":true,"expected_revision":0,"expected_bucket_id":"` + demoID + `"}`,
		`{"read":"true","expected_revision":0,"expected_bucket_id":"` + demoID + `"}`,
		`{"read":true,"expected_revision":0.0,"expected_bucket_id":"` + demoID + `"}`,
		`{"read":true,"expected_revision":0}`,
	} {
		w.expect(w.do("PUT", grant, w.bearer["owner"], body), http.StatusBadRequest, "invalid_request")
	}
	for _, body := range []string{
		`{"tiering":{"enabled":false},"expected_revision":1,"expected_bucket_id":"` + demoID + `"}`,
		`{"dedup_domain":"x","expected_revision":1,"expected_bucket_id":"` + demoID + `"}`,
		`{"compression":{},"expected_revision":1,"expected_bucket_id":"` + demoID + `"}`,
		`{"cache":{"readahead_chunks":4.0},"expected_revision":1,"expected_bucket_id":"` + demoID + `"}`,
		`{"cache":{"admit_on_write":null},"expected_revision":1,"expected_bucket_id":"` + demoID + `"}`,
		`{"cache":{"admit_on_write":true,"admit_on_write":false},"expected_revision":1,"expected_bucket_id":"` + demoID + `"}`,
	} {
		w.expect(w.do("PUT", "/admin/v1/buckets/demo/policy", w.bearer["owner"], body), http.StatusBadRequest, "invalid_request")
	}
	if g := w.grant("user-stranger"); g.Revision != 0 {
		t.Fatalf("rejected grant body wrote %+v", g)
	}
	if _, err := w.store.GetBucketPolicy(context.Background(), "demo", demoID, 0); err != storage.ErrNotFound {
		t.Fatal("rejected bucket policy body wrote a policy", err)
	}
}

// TestPagination pages users and credentials and checks the cursor binding
// (API §6.5.3).
func TestPagination(t *testing.T) {
	var users []storage.User
	for i := range 5 {
		users = append(users, storage.User{ID: fmt.Sprintf("page-%d", i), Active: true, Revision: 1})
	}
	w := newWorld(t, badger.Bootstrap{Users: users})
	type page struct {
		Items []struct {
			UserID string `json:"user_id"`
		}
		NextCursor string `json:"next_cursor"`
	}
	var all []string
	target := "/admin/v1/users?limit=4"
	var first string
	for {
		p := decode[page](t, w.must(w.do("GET", target, w.root, ""), http.StatusOK))
		for _, u := range p.Items {
			all = append(all, u.UserID)
		}
		if first == "" {
			first = p.NextCursor
		}
		if p.NextCursor == "" {
			break
		}
		target = "/admin/v1/users?limit=4&cursor=" + p.NextCursor
	}
	if len(all) != len(w.f.Principals)+5 || !slices.IsSorted(all) {
		t.Fatalf("users pages: %v", all)
	}
	other := w.bearer["admin_reader"]
	w.expect(w.do("GET", "/admin/v1/users?cursor="+first, other, ""), http.StatusBadRequest, "invalid_cursor")
	w.expect(w.do("GET", "/admin/v1/credentials?cursor="+first, w.root, ""), http.StatusBadRequest, "invalid_cursor")
	w.expect(w.do("GET", "/admin/v1/users?cursor=k1.e30.AAAA", w.root, ""), http.StatusBadRequest, "invalid_cursor")
	w.must(w.do("GET", "/admin/v1/users?limit=1000&cursor="+first, w.root, ""), http.StatusOK)
	for _, q := range []string{"limit=0", "limit=1001", "limit=01", "limit=x", "limit=1&limit=2"} {
		w.expect(w.do("GET", "/admin/v1/users?"+q, w.root, ""), http.StatusBadRequest, "invalid_request")
	}

	for range 3 {
		w.must(w.do("POST", "/admin/v1/credentials", w.root, `{"user_id":"user-writer"}`), http.StatusCreated)
	}
	w.must(w.do("POST", "/admin/v1/credentials", w.root, `{"user_id":"user-reader"}`), http.StatusCreated)
	p := decode[page](t, w.must(w.do("GET", "/admin/v1/credentials?user_id=user-writer&limit=2", w.root, ""), http.StatusOK))
	if len(p.Items) != 2 || p.NextCursor == "" {
		t.Fatalf("credentials page: %+v", p)
	}
	w.expect(w.do("GET", "/admin/v1/credentials?user_id=user-reader&cursor="+p.NextCursor, w.root, ""), http.StatusBadRequest, "invalid_cursor")
	last := decode[page](t, w.must(w.do("GET", "/admin/v1/credentials?user_id=user-writer&cursor="+p.NextCursor, w.root, ""), http.StatusOK))
	if len(last.Items) != 1 || last.NextCursor != "" {
		t.Fatalf("credentials last page: %+v", last)
	}

	grants := decode[page](t, w.must(w.do("GET", "/admin/v1/buckets/demo/grants?limit=2", w.bearer["owner"], ""), http.StatusOK))
	w.expect(w.do("GET", "/admin/v1/buckets/demo/grants?cursor="+grants.NextCursor, w.root, ""), http.StatusBadRequest, "invalid_cursor")
	w.must(w.do("GET", "/admin/v1/buckets/demo/grants?cursor="+grants.NextCursor, w.bearer["owner"], ""), http.StatusOK)
	w.clk.Advance(CursorTTL)
	w.root = w.mintToken("system-root", true) // the bootstrap token outlives the cursor anyway
	w.expect(w.do("GET", "/admin/v1/buckets/demo/grants?cursor="+grants.NextCursor, w.bearer["owner"], ""), http.StatusBadRequest, "invalid_cursor")
}

// TestUsersAndSecrets covers user create/usage and that secrets and tokens
// are returned only by create, never listed or logged.
func TestUsersAndSecrets(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	rec := w.must(w.do("POST", "/admin/v1/users", w.root, `{"user_id":"new-user"}`), http.StatusCreated)
	if u := decode[userJSON](t, rec); u != (userJSON{"new-user", "active", "default", 1 << 40, 1}) {
		t.Fatalf("created user: %+v", u)
	}
	w.expect(w.do("POST", "/admin/v1/users", w.root, `{"user_id":"new-user"}`), http.StatusConflict, "user_exists")
	w.expect(w.do("POST", "/admin/v1/users", w.bearer["owner"], `{"user_id":"x"}`), http.StatusForbidden, "forbidden")
	usage := decode[map[string]any](t, w.must(w.do("GET", "/admin/v1/users/user-writer/usage?detail=true", w.bearer["writer"], ""), http.StatusOK))
	if len(usage) != 6 || usage["bytes_logical_limit"] != float64(1<<40) || usage["active_upload_count"] != float64(0) {
		t.Fatalf("usage detail: %v", usage)
	}
	if usage := decode[map[string]any](t, w.must(w.do("GET", "/admin/v1/users/user-writer/usage", w.root, ""), http.StatusOK)); len(usage) != 4 {
		t.Fatalf("usage: %v", usage)
	}

	w.expect(w.do("POST", "/admin/v1/credentials", w.root, `{"user_id":"user-writer","debug_allowed":true}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("POST", "/admin/v1/credentials", w.root, `{"user_id":"nobody"}`), http.StatusNotFound, "user_not_found")
	rec = w.must(w.do("POST", "/admin/v1/credentials", w.root, `{"user_id":"user-writer","system_admin":true,"debug_allowed":true}`), http.StatusCreated)
	cred := decode[struct {
		credentialJSON
		SecretAccessKey string `json:"secret_access_key"`
	}](t, rec)
	if !accessKeyPattern.MatchString(cred.AccessKeyID) || len(cred.SecretAccessKey) != 40 || cred.Status != "active" || !cred.SystemAdmin || !cred.DebugAllowed || cred.CreatedAt != rfc3339(testNow) {
		t.Fatalf("created credential: %+v", cred)
	}
	sec, err := w.store.GetCredentialSecurity(context.Background(), cred.AccessKeyID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sec.Ciphertext, []byte(cred.SecretAccessKey)) {
		t.Fatal("secret stored in clear")
	}
	if secret, err := w.sealer.Open(context.Background(), cred.AccessKeyID, sec); err != nil || string(secret) != cred.SecretAccessKey {
		t.Fatal("sealed secret does not open", err)
	}
	if _, err := w.sealer.Open(context.Background(), "OTHERKEYOTHERKEY0000", sec); err == nil {
		t.Fatal("sealed secret opens under another access key")
	}
	list := w.must(w.do("GET", "/admin/v1/credentials", w.root, ""), http.StatusOK).Body.String()
	if strings.Contains(list, cred.SecretAccessKey) || strings.Contains(list, "secret") || !strings.Contains(list, cred.AccessKeyID) {
		t.Fatalf("credential list: %s", list)
	}
	w.must(w.do("DELETE", "/admin/v1/credentials/"+cred.AccessKeyID, w.root, ""), http.StatusNoContent)
	w.must(w.do("DELETE", "/admin/v1/credentials/"+cred.AccessKeyID, w.root, ""), http.StatusNoContent)
	if c, err := w.store.GetCredential(context.Background(), cred.AccessKeyID); err != nil || c.Active || !c.SystemAdmin {
		t.Fatal("credential not revoked", c, err)
	}

	w.expect(w.do("POST", "/admin/v1/admin-tokens", w.root, `{"user_id":"user-writer","expires_in_seconds":299}`), http.StatusBadRequest, "invalid_request")
	w.expect(w.do("POST", "/admin/v1/admin-tokens", w.root, `{"user_id":"user-writer","expires_in_seconds":604801}`), http.StatusBadRequest, "invalid_request")
	rec = w.must(w.do("POST", "/admin/v1/admin-tokens", w.root, `{"user_id":"user-writer","expires_in_seconds":300}`), http.StatusCreated)
	tok := decode[map[string]any](t, rec)
	token := tok["token"].(string)
	if tok["token_id"] != tokenID(token) || tok["request_id"] != rec.Header().Get("X-Request-Id") || tok["expires_at"] != rfc3339(testNow.Add(300*time.Second)) || tok["system_admin"] != false {
		t.Fatalf("created token: %v", tok)
	}
	w.must(w.do("GET", "/admin/v1/users/user-writer/usage", token, ""), http.StatusOK)
	w.expect(w.do("GET", "/admin/v1/users", token, ""), http.StatusForbidden, "forbidden")
	list = w.must(w.do("GET", "/admin/v1/admin-tokens?user_id=user-writer", w.root, ""), http.StatusOK).Body.String()
	_, secret, _ := strings.Cut(token, ".")
	if strings.Contains(list, secret) || strings.Contains(list, SecretHash(secret)) || strings.Contains(list, `"token"`) || !strings.Contains(list, tokenID(token)) {
		t.Fatalf("token list: %s", list)
	}
	// The owner of a token revokes it; revoked is terminal and idempotent.
	w.must(w.do("DELETE", "/admin/v1/admin-tokens/"+tokenID(token), token, ""), http.StatusNoContent)
	w.must(w.do("DELETE", "/admin/v1/admin-tokens/"+tokenID(token), w.root, ""), http.StatusNoContent)

	log := w.log.String()
	if strings.Contains(log, secret) || strings.Contains(log, cred.SecretAccessKey) || strings.Contains(log, SecretHash(secret)) || !strings.Contains(log, "admin mutation") {
		t.Fatalf("audit log leaks or is missing: %s", log)
	}
}

// TestBucketPolicyRefusedWritesNothing: a refused PUT on a bucket without
// a policy leaves it without one; the first-use default is merged in memory.
func TestBucketPolicyRefusedWritesNothing(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	bp, owner := "/admin/v1/buckets/demo/policy", w.bearer["owner"]
	for _, tc := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"cache":{"readahead_chunks":65},"expected_revision":1,"expected_bucket_id":"` + demoID + `"}`, http.StatusBadRequest, "invalid_request"},
		{`{"tiering":{"enabled":false},"expected_revision":1,"expected_bucket_id":"` + demoID + `"}`, http.StatusBadRequest, "invalid_request"},
		{`{"cache":{"readahead_chunks":8},"expected_revision":1,"expected_bucket_id":"BUCKET"}`, http.StatusBadRequest, "invalid_request"},
		{`{"cache":{"readahead_chunks":8},"expected_revision":1,"expected_bucket_id":"other"}`, http.StatusConflict, "bucket_id_mismatch"},
		{`{"cache":{"readahead_chunks":8},"expected_revision":2,"expected_bucket_id":"` + demoID + `"}`, http.StatusConflict, "revision_conflict"},
	} {
		w.expect(w.do("PUT", bp, owner, tc.body), tc.status, tc.code)
		if _, err := w.store.GetBucketPolicy(context.Background(), "demo", demoID, 0); err != storage.ErrNotFound {
			t.Fatalf("%s: refused request wrote a bucket policy (%v)", tc.body, err)
		}
	}
	type bucketPolicy struct {
		Revision uint64
		Cache    config.CachePolicy
	}
	if p := decode[bucketPolicy](t, w.must(w.do("PUT", bp, owner, `{"cache":{"readahead_chunks":8},"expected_revision":1,"expected_bucket_id":"`+demoID+`"}`), http.StatusOK)); p.Revision != 2 || p.Cache.ReadaheadChunks != 8 {
		t.Fatalf("first put: %+v", p)
	}
	if p, err := w.store.GetBucketPolicy(context.Background(), "demo", demoID, 1); err != nil || !strings.Contains(string(p.Document), `"readahead_chunks":4`) {
		t.Fatalf("revision 1 is not the default: %s %v", p.Document, err)
	}
}

// maxTokenStore reports one token at the maximum revision.
type maxTokenStore struct {
	*badger.Store
	id string
}

func (m maxTokenStore) GetAdminToken(ctx context.Context, id string) (storage.AdminToken, error) {
	t, err := m.Store.GetAdminToken(ctx, id)
	if id == m.id {
		t.Revision = MaxRevision
	}
	return t, err
}

// TestRevokeAtMaxRevision: revoking an active credential or token at
// 2^53-1 is revision_exhausted; an already revoked one stays 204.
func TestRevokeAtMaxRevision(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{Credentials: []storage.Credential{
		{ID: "MAXACTIVE00000000000", UserID: "user-writer", Active: true, Revision: MaxRevision},
		{ID: "MAXREVOKED0000000000", UserID: "user-writer", Revision: MaxRevision},
	}})
	w.expect(w.do("DELETE", "/admin/v1/credentials/MAXACTIVE00000000000", w.root, ""), http.StatusConflict, "revision_exhausted")
	w.must(w.do("DELETE", "/admin/v1/credentials/MAXREVOKED0000000000", w.root, ""), http.StatusNoContent)

	active, revoked := tokenID(w.bearer["writer"]), tokenID(w.bearer["revoked"])
	for _, id := range []string{active, revoked} {
		o := w.opts
		o.Store = maxTokenStore{w.store, id}
		h, err := NewServer(o)
		if err != nil {
			t.Fatal(err)
		}
		w.h = h
		rec := w.do("DELETE", "/admin/v1/admin-tokens/"+id, w.root, "")
		if id == active {
			w.expect(rec, http.StatusConflict, "revision_exhausted")
		} else {
			w.must(rec, http.StatusNoContent)
		}
	}
}

// TestBodyRoutesRejectQuery: every route that takes a body has no query
// parameter (API §6.5.1 step 4), so any is 400 before admission or write.
func TestBodyRoutesRejectQuery(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	ctx := context.Background()
	tokens := func() int {
		p, err := w.store.ListAdminTokens(ctx, storage.IdentityListRequest{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		return len(p.Items)
	}
	before := tokens()
	for _, q := range []string{"?unexpected=1", "?expected_revision=1", "?a=1&a=2"} {
		for _, tc := range []struct{ method, path, body string }{
			{"POST", "/admin/v1/users", `{"user_id":"query-user"}`},
			{"PATCH", "/admin/v1/users/user-writer", `{"status":"disabled","expected_revision":1}`},
			{"POST", "/admin/v1/credentials", `{"user_id":"user-stranger"}`},
			{"POST", "/admin/v1/admin-tokens", `{"user_id":"user-stranger"}`},
			{"PUT", "/admin/v1/buckets/demo/grants/user-stranger", `{"read":true,"expected_revision":0,"expected_bucket_id":"` + demoID + `"}`},
			{"PUT", "/admin/v1/policy", `{"expected_revision":1,"compression":{"enabled":false}}`},
			{"PUT", "/admin/v1/buckets/demo/policy", `{"cache":{"readahead_chunks":8},"expected_revision":1,"expected_bucket_id":"` + demoID + `"}`},
		} {
			w.expect(w.do(tc.method, tc.path+q, w.root, tc.body), http.StatusBadRequest, "invalid_request")
		}
	}
	if _, err := w.store.GetUser(ctx, "query-user"); err != storage.ErrNotFound {
		t.Fatal("POST users wrote", err)
	}
	if u, err := w.store.GetUser(ctx, "user-writer"); err != nil || !u.Active || u.Revision != 1 {
		t.Fatal("PATCH user wrote", u, err)
	}
	if p, err := w.store.ListCredentials(ctx, storage.IdentityListRequest{UserID: "user-stranger", Limit: 10}); err != nil || len(p.Items) != 0 {
		t.Fatal("POST credentials wrote", p, err)
	}
	if tokens() != before {
		t.Fatal("POST admin-tokens wrote")
	}
	if g := w.grant("user-stranger"); g.Revision != 0 {
		t.Fatal("PUT grant wrote", g)
	}
	if p, err := w.store.GetPolicy(ctx, 0); err != nil || p.Revision != 1 {
		t.Fatal("PUT policy wrote", p.Revision, err)
	}
	if _, err := w.store.GetBucketPolicy(ctx, "demo", demoID, 0); err != storage.ErrNotFound {
		t.Fatal("PUT bucket policy wrote", err)
	}
}

// quotaHookStore runs hook once, between the user and quota reads of a request.
type quotaHookStore struct {
	*badger.Store
	hook func()
}

func (m *quotaHookStore) GetQuota(ctx context.Context, id string) (storage.LogicalQuota, error) {
	if h := m.hook; h != nil {
		m.hook = nil
		h()
	}
	return m.Store.GetQuota(ctx, id)
}

// TestPatchUserNoOpNeedsCoherentRevision: a PATCH changed by a concurrent
// commit between its user and quota reads is stale, never a no-op at the
// old revision.
func TestPatchUserNoOpNeedsCoherentRevision(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	direct := w.h
	store := &quotaHookStore{Store: w.store}
	o := w.opts
	o.Store = store
	h, err := NewServer(o)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"logical_limit":1024,"expected_revision":1}`
	store.hook = func() {
		w.h = direct
		w.must(w.do("PATCH", "/admin/v1/users/user-writer", w.root, body), http.StatusOK)
		w.h = h
	}
	w.h = h
	w.expect(w.do("PATCH", "/admin/v1/users/user-writer", w.root, body), http.StatusConflict, "revision_conflict")
	if u, err := w.store.GetUser(context.Background(), "user-writer"); err != nil || u.Revision != 2 {
		t.Fatal(u, err)
	}
}

// TestPatchUserLimitKeepsLedgerValid: a limit below used+reserved would
// block every later reservation (DELETE included), so it is refused whole.
func TestPatchUserLimitKeepsLedgerValid(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{
		Users:  []storage.User{{ID: "user-full", Active: true, Revision: 1, DefaultDomain: "default"}},
		Quotas: []storage.LogicalQuota{{OwnerID: "user-full", Limit: 8, Used: 6, Reserved: 2}},
	})
	ctx := context.Background()
	for _, limit := range []int{0, 5, 7} {
		w.expect(w.do("PATCH", "/admin/v1/users/user-full", w.root, fmt.Sprintf(`{"logical_limit":%d,"expected_revision":1}`, limit)), http.StatusBadRequest, "invalid_request")
	}
	if u, err := w.store.GetUser(ctx, "user-full"); err != nil || u.Revision != 1 {
		t.Fatal("refused limit changed the user", u, err)
	}
	if q, err := w.store.GetQuota(ctx, "user-full"); err != nil || q.Limit != 8 || q.Used != 6 || q.Reserved != 2 {
		t.Fatal("refused limit changed the ledger", q, err)
	}
	w.must(w.do("PATCH", "/admin/v1/users/user-full", w.root, `{"logical_limit":9,"expected_revision":1}`), http.StatusOK)
	w.must(w.do("PATCH", "/admin/v1/users/user-full", w.root, `{"logical_limit":8,"expected_revision":2}`), http.StatusOK)
}
