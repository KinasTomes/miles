package live_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/storage"
)

type wireIdentity struct{}

func (wireIdentity) Credential(context.Context, string) (s3.Credential, error) {
	return s3.Credential{UserID: "owner", Secret: "synthetic-fixture-secret", Active: true}, nil
}
func (wireIdentity) Authorize(_ context.Context, _ string, bucket string) (storage.Authorization, error) {
	return storage.Authorization{UserID: "owner", Read: true, Write: true,
		Bucket: storage.Bucket{Name: bucket, ID: "bucket-wire", OwnerID: "owner", Domain: "wire", Active: true}}, nil
}

// Exercise the live client's exact request construction through the real router
// before using it on a paid deployment. No backend or network fixture is needed.
func TestLiveFixtureWireRequests(t *testing.T) {
	f := fixture{t: t, config: liveConfig{Region: "ap-southeast-1"},
		ep: endpoint{URL: "https://cloud.example:443"}, bucket: "acceptance-wire"}
	c := credential{ID: "ABCDEFGHIJKLMNOPQRST", Secret: "synthetic-fixture-secret"}
	body := []byte("exact synthetic payload")
	sum := sha256.Sum256(body)
	key := "/caf%C3%A9%20%2B%25/encoded%2Fslash//dots/./../kept"
	for _, presigned := range []bool{false, true} {
		for _, corrupt := range []bool{false, true} {
			query := "x-amz-checksum-sha256=" + url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:]))
			headers := map[string]string{"X-Amz-Meta-With_Underscore": "kept"}
			if !presigned {
				query = ""
				headers["X-Amz-Checksum-Sha256"] = base64.StdEncoding.EncodeToString(sum[:])
			}
			payload := bytes.Clone(body)
			if corrupt {
				payload[0] ^= 1
			}
			r := f.signed(c, "PUT", key, query, payload, headers, presigned)
			wire := httptest.NewRequest(r.Method, r.URL.String(), r.Body)
			wire.Header = r.Header.Clone()
			wire.ContentLength = r.ContentLength
			wire.Host = r.Host
			wire.Header.Set("Content-Length", fmt.Sprint(r.ContentLength))
			called := false
			router := &s3.Router{Region: f.config.Region, Credentials: wireIdentity{}, Access: wireIdentity{}, Now: time.Now,
				Handlers: map[s3.Operation]s3.Handler{s3.PutObject: func(w http.ResponseWriter, q *s3.Request) error {
					called = true
					if q.HTTP.Host != "cloud.example:443" || q.HTTP.URL.EscapedPath() != "/acceptance-wire"+key || q.HTTP.Header.Get("X-Amz-Meta-With_Underscore") != "kept" {
						t.Fatal("live fixture changed signed Host, path or metadata")
					}
					_, err := io.ReadAll(q.Body)
					return err
				}}}
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, wire)
			want := 200
			if corrupt {
				want = 400
			}
			if !called || recorder.Code != want {
				t.Fatalf("presigned=%t corrupt=%t returned %d, expected %d: %s", presigned, corrupt, recorder.Code, want, recorder.Body.String())
			}
		}
	}
}
