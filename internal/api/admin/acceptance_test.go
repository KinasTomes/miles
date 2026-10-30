package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/auth"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

const (
	s3Region    = "us-east-1"
	emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// s3World is the S3 router over the same Badger store as the Admin
// server: authorization snapshots and SigV4 secrets (sealed by the Admin
// server) are read through auth caches on the same virtual clock.
type s3World struct {
	*world
	router *s3.Router
}

func newS3World(t *testing.T) *s3World {
	w := newWorld(t, badger.Bootstrap{})
	creds := auth.NewCache(w.clk, func(ctx context.Context, id string) (s3.Credential, time.Time, error) {
		c, err := w.store.GetCredential(ctx, id)
		if err != nil {
			return s3.Credential{}, time.Time{}, err
		}
		sec, err := w.store.GetCredentialSecurity(ctx, id)
		if err != nil {
			return s3.Credential{}, time.Time{}, err
		}
		secret, err := w.sealer.Open(ctx, id, sec)
		if err != nil {
			return s3.Credential{}, time.Time{}, err
		}
		return s3.Credential{UserID: c.UserID, Secret: string(secret), Active: c.Active, SystemAdmin: c.SystemAdmin, DebugAllowed: c.DebugAllowed}, time.Time{}, nil
	})
	allow := func(rw http.ResponseWriter, _ *s3.Request) error { rw.WriteHeader(http.StatusOK); return nil }
	return &s3World{world: w, router: &s3.Router{
		Region:      s3Region,
		Credentials: credentialsFunc(creds.Get),
		Access:      auth.NewSnapshots(w.store, w.clk),
		Now:         func() time.Time { return w.clk.Now().WallUTC },
		Handlers:    map[s3.Operation]s3.Handler{s3.GetObject: allow, s3.PutObject: allow},
	}}
}

type credentialsFunc func(context.Context, string) (s3.Credential, error)

func (f credentialsFunc) Credential(ctx context.Context, id string) (s3.Credential, error) {
	return f(ctx, id)
}

type s3Key struct{ id, secret string }

// credential creates a SigV4 credential through the Admin API.
func (w *s3World) credential(user string, admin bool) s3Key {
	w.t.Helper()
	rec := w.must(w.do("POST", "/admin/v1/credentials", w.root, fmt.Sprintf(`{"user_id":%q,"system_admin":%t}`, user, admin)), http.StatusCreated)
	c := decode[struct {
		AccessKeyID     string `json:"access_key_id"`
		SecretAccessKey string `json:"secret_access_key"`
	}](w.t, rec)
	return s3Key{c.AccessKeyID, c.SecretAccessKey}
}

var s3Code = regexp.MustCompile(`<Code>([^<]+)</Code>`)

// call sends a signed GET or PUT of demo/k: "allow" or the S3 error code.
func (w *s3World) call(method string, k s3Key) string {
	w.t.Helper()
	r := httptest.NewRequest(method, "http://s3.test/demo/k", nil)
	r.Header.Set("X-Amz-Content-Sha256", emptySHA256)
	if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: k.id, SecretAccessKey: k.secret}, r, emptySHA256, "s3", s3Region, w.clk.Now().WallUTC); err != nil {
		w.t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	w.router.ServeHTTP(rec, r)
	if rec.Code == http.StatusOK {
		return "allow"
	}
	if m := s3Code.FindStringSubmatch(rec.Body.String()); m != nil {
		return m[1]
	}
	return fmt.Sprintf("HTTP %d", rec.Code)
}

func (w *s3World) advance(d time.Duration) {
	w.t.Helper()
	if err := w.clk.Advance(d); err != nil {
		w.t.Fatal(err)
	}
}

// TestAcceptance is checklist 4 of W04-CHIEN-01: Admin writes reach S3
// admission on the same store within the 5 s snapshot bound.
func TestAcceptance(t *testing.T) {
	w := newS3World(t)
	check := func(what, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %s, want %s", what, got, want)
		}
	}

	// A system-admin credential carries no data right.
	admin := w.credential("system-root", true)
	check("admin GetObject", w.call("GET", admin), "AccessDenied")
	check("admin PutObject", w.call("PUT", admin), "AccessDenied")

	// A write-only grant made by the owner over the Admin API.
	w.must(w.do("PUT", "/admin/v1/buckets/demo/grants/user-stranger", w.bearer["owner"], `{"write":true,"expected_revision":0,"expected_bucket_id":"`+demoID+`"}`), http.StatusOK)
	stranger := w.credential("user-stranger", false)
	check("write-only PutObject", w.call("PUT", stranger), "allow")
	check("write-only GetObject", w.call("GET", stranger), "AccessDenied")

	// Unknown privilege fields: 400 and nothing written.
	for _, field := range []string{`"system_admin":true`, `"owner":true`} {
		w.expect(w.do("PUT", "/admin/v1/buckets/demo/grants/user-stranger", w.bearer["owner"], `{"read":true,`+field+`,"expected_revision":1,"expected_bucket_id":"`+demoID+`"}`), http.StatusBadRequest, "invalid_request")
	}
	if g := w.grant("user-stranger"); g.Read || !g.Write || g.Revision != 1 {
		t.Fatalf("rejected privilege field wrote %+v", g)
	}
	check("write-only GetObject after rejected elevation", w.call("GET", stranger), "AccessDenied")

	// Revokes at T are seen from T + 5 s; snapshots read before T may admit
	// until then.
	rw := w.credential("user-rw", false)
	reader := w.credential("user-reader", false)
	writerBearer := w.mintToken("user-writer", false)
	check("rw PutObject", w.call("PUT", rw), "allow")
	check("reader GetObject", w.call("GET", reader), "allow")
	w.must(w.do("GET", "/admin/v1/users/user-writer/usage", writerBearer, ""), http.StatusOK)

	w.must(w.do("DELETE", "/admin/v1/credentials/"+rw.id, w.root, ""), http.StatusNoContent)
	w.must(w.do("DELETE", "/admin/v1/buckets/demo/grants/user-reader?expected_bucket_id="+demoID, w.bearer["owner"], ""), http.StatusNoContent)
	w.must(w.do("DELETE", "/admin/v1/admin-tokens/"+tokenID(writerBearer), w.root, ""), http.StatusNoContent)

	w.advance(5*time.Second - time.Millisecond)
	check("revoked credential before 5 s", w.call("PUT", rw), "allow")
	check("revoked grant before 5 s", w.call("GET", reader), "allow")
	w.must(w.do("GET", "/admin/v1/users/user-writer/usage", writerBearer, ""), http.StatusOK)

	w.advance(time.Millisecond)
	if got := w.call("PUT", rw); got == "allow" {
		t.Error("revoked credential admitted at 5 s")
	}
	check("revoked grant at 5 s", w.call("GET", reader), "AccessDenied")
	w.expect(w.do("GET", "/admin/v1/users/user-writer/usage", writerBearer, ""), http.StatusUnauthorized, "unauthorized")
}
