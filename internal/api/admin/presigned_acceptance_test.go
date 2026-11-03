package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// These requests reuse exactly the same URL before and after authoritative
// Admin mutations. A signature's seven-day lifetime must never become a grant.
func TestPresignedAdminRevocation(t *testing.T) {
	for _, mutation := range []string{"credential", "grant", "user"} {
		t.Run(mutation, func(t *testing.T) {
			w := newS3World(t)
			key := w.credential("user-rw", false)
			requests := map[string]*http.Request{}
			for _, method := range []string{"GET", "PUT"} {
				r := httptest.NewRequest(method, "http://s3.test:443/demo/k?X-Amz-Expires=604800", nil)
				uri, headers, err := v4.NewSigner().PresignHTTP(context.Background(), aws.Credentials{
					AccessKeyID: key.id, SecretAccessKey: key.secret,
				}, r, "UNSIGNED-PAYLOAD", "s3", s3Region, w.clk.Now().WallUTC)
				if err != nil {
					t.Fatal(err)
				}
				r = httptest.NewRequest(method, uri, nil)
				r.Header = headers
				requests[method] = r
			}
			check := func(status int, code string) {
				t.Helper()
				for method, r := range requests {
					rec := httptest.NewRecorder()
					w.router.ServeHTTP(rec, r.Clone(context.Background()))
					if rec.Code != status || code != "" && !s3Code.MatchString(rec.Body.String()) {
						t.Fatalf("%s: HTTP %d, expected %d", method, rec.Code, status)
					}
					if code != "" && s3Code.FindStringSubmatch(rec.Body.String())[1] != code {
						t.Fatalf("%s: wrong error code", method)
					}
				}
			}
			check(http.StatusOK, "")
			switch mutation {
			case "credential":
				w.must(w.do("DELETE", "/admin/v1/credentials/"+key.id, w.root, ""), http.StatusNoContent)
			case "grant":
				w.must(w.do("DELETE", "/admin/v1/buckets/demo/grants/user-rw?expected_bucket_id="+demoID, w.bearer["owner"], ""), http.StatusNoContent)
			case "user":
				w.must(w.do("PATCH", "/admin/v1/users/user-rw", w.root, `{"status":"disabled","expected_revision":1}`), http.StatusOK)
			}
			w.advance(5*time.Second - time.Millisecond)
			check(http.StatusOK, "")
			w.advance(time.Millisecond)
			want := "AccessDenied"
			if mutation == "credential" {
				want = "InvalidAccessKeyId"
			}
			check(http.StatusForbidden, want)
		})
	}
}

func TestPresignedWriteOnlyAndRevokedAdminToken(t *testing.T) {
	w := newS3World(t)
	w.must(w.do("PUT", "/admin/v1/buckets/demo/grants/user-stranger", w.bearer["owner"],
		fmt.Sprintf(`{"write":true,"expected_revision":0,"expected_bucket_id":%q}`, demoID)), http.StatusOK)
	key := w.credential("user-stranger", false)
	for _, method := range []string{"GET", "PUT"} {
		r := httptest.NewRequest(method, "http://s3.test/demo/k?X-Amz-Expires=900", nil)
		uri, headers, err := v4.NewSigner().PresignHTTP(context.Background(), aws.Credentials{
			AccessKeyID: key.id, SecretAccessKey: key.secret,
		}, r, "UNSIGNED-PAYLOAD", "s3", s3Region, w.clk.Now().WallUTC)
		if err != nil {
			t.Fatal(err)
		}
		r = httptest.NewRequest(method, uri, nil)
		r.Header = headers
		rec := httptest.NewRecorder()
		w.router.ServeHTTP(rec, r)
		want := http.StatusOK
		if method == "GET" {
			want = http.StatusForbidden
		}
		if rec.Code != want {
			t.Fatalf("write-only %s: HTTP %d", method, rec.Code)
		}
	}
	// A revoked bearer cannot grant read to make its user's presigned GET work.
	bearer := w.mintToken("user-owner", false)
	w.must(w.do("DELETE", "/admin/v1/admin-tokens/"+tokenID(bearer), w.root, ""), http.StatusNoContent)
	w.advance(5 * time.Second)
	w.expect(w.do("PUT", "/admin/v1/buckets/demo/grants/user-stranger", bearer,
		fmt.Sprintf(`{"read":true,"write":true,"expected_revision":1,"expected_bucket_id":%q}`, demoID)), http.StatusUnauthorized, "unauthorized")
	if g := w.grant("user-stranger"); g.Read || !g.Write {
		t.Fatal("revoked bearer changed grants")
	}
}
