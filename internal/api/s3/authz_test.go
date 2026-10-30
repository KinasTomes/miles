package s3

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/auth"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

// authorization.json is the identity contract of API v2 2.2.3. Its S3 rows
// run through the Router on the fake metadata store: each principal has its
// user, a credential that signs its requests and its grants on demo, and
// auth.Snapshots serves the router's Access. The handlers stand in for those
// of W03/W04 and run the checks that follow the router's with the fixture
// state: copy source, upload binding and key.

type principal struct {
	UserID      string `json:"user_id"`
	Read        bool
	Write       bool
	SystemAdmin bool   `json:"system_admin"`
	UserStatus  string `json:"user_status"`
	AuthStatus  string `json:"auth_status"`
}

type authCase struct {
	Name      string
	Op        Operation
	Principal string
	Bucket    string // "missing": no bucket of that name
	Key       string // "missing": no object at the key
	Initiator *bool  // nil: the caller started the upload
	Binding   string // other_key, other_bucket_id, unknown_id: upload_id does not bind
	Source    *struct {
		Bucket string // "missing": no bucket; empty: bucket src, readable when Read
		Key    string // "missing": no object at the key
		Read   bool
	}
	Expect string
}

type authFixture struct {
	Bucket struct {
		Bucket      string
		BucketID    string `json:"bucket_id"`
		OwnerID     string `json:"owner_id"`
		DedupDomain string `json:"dedup_domain"`
	}
	Principals map[string]principal
	S3         []struct {
		Op       Operation
		Requires string
		Expect   map[string]string
	}
	S3Cases []authCase `json:"s3_cases"`
}

const authRegion = "us-east-1"

var authNow = time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)

func accessKey(principal string) string {
	return "AK" + strings.ToUpper(strings.ReplaceAll(principal, "_", ""))
}

// authWorld is the metadata of one request: demo with key k, upload UP1 on
// demo/k, and for CopyObject cases a source bucket src with key k.
type authWorld struct {
	store   *fake.Metadata
	upload  *UploadBinding
	objects map[string]bool
	router  Router
}

func newAuthWorld(t *testing.T, f authFixture, c authCase) *authWorld {
	t.Helper()
	demo := storage.Bucket{Name: f.Bucket.Bucket, ID: f.Bucket.BucketID, OwnerID: f.Bucket.OwnerID, Domain: f.Bucket.DedupDomain, Revision: 1, Active: true}
	seed := fake.Seed{Now: func() time.Time { return authNow }, Buckets: []storage.Bucket{demo}}
	secrets := map[string]string{}
	for name, p := range f.Principals {
		seed.Users = append(seed.Users, storage.User{ID: p.UserID, Active: p.UserStatus != "disabled", Revision: 1})
		seed.Credentials = append(seed.Credentials, storage.Credential{ID: accessKey(name), UserID: p.UserID, Active: p.AuthStatus != "revoked", SystemAdmin: p.SystemAdmin, Revision: 1})
		secrets[accessKey(name)] = "secret-" + name
		if p.UserID != demo.OwnerID && (p.Read || p.Write) {
			seed.Grants = append(seed.Grants, storage.Grant{BucketID: demo.ID, UserID: p.UserID, Read: p.Read, Write: p.Write, Revision: 1})
		}
	}
	caller := f.Principals[c.Principal].UserID
	if c.Source != nil && c.Source.Bucket == "" {
		seed.Buckets = append(seed.Buckets, storage.Bucket{Name: "src", ID: "bkt-src-0001", OwnerID: "user-src", Domain: "default", Revision: 1, Active: true})
		seed.Users = append(seed.Users, storage.User{ID: "user-src", Active: true, Revision: 1})
		if c.Source.Read {
			seed.Grants = append(seed.Grants, storage.Grant{BucketID: "bkt-src-0001", UserID: caller, Read: true, Revision: 1})
		}
	}

	w := &authWorld{store: fake.NewMetadata(seed), objects: map[string]bool{"demo/k": true, "src/k": true}}
	u := UploadBinding{BucketID: demo.ID, Key: "k", Initiator: caller}
	if c.Initiator != nil && !*c.Initiator {
		u.Initiator = "user-other"
	}
	switch c.Binding {
	case "":
		w.upload = &u
	case "other_key":
		u.Key = "other"
		w.upload = &u
	case "other_bucket_id":
		u.BucketID = "bkt-demo-0000"
		w.upload = &u
	case "unknown_id":
	default:
		t.Fatalf("unknown binding %q", c.Binding)
	}

	handlers := map[Operation]Handler{}
	for _, r := range routes {
		handlers[r.op] = w.handle
	}
	w.router = Router{
		Region:      authRegion,
		Credentials: storeCredentials{w.store, secrets},
		Access:      auth.NewSnapshots(w.store, clock.NewReal()),
		Now:         func() time.Time { return authNow },
		Handlers:    handlers,
	}
	return w
}

type storeCredentials struct {
	store   *fake.Metadata
	secrets map[string]string
}

func (c storeCredentials) Credential(ctx context.Context, accessKeyID string) (Credential, error) {
	v, err := c.store.GetCredential(ctx, accessKeyID)
	if err != nil {
		return Credential{}, err
	}
	return Credential{UserID: v.UserID, Secret: c.secrets[accessKeyID], Active: v.Active, SystemAdmin: v.SystemAdmin}, nil
}

// handle answers how far an authorized request gets: listed or hidden for
// ListBuckets, allow for any other operation that would go on.
func (w *authWorld) handle(rw http.ResponseWriter, r *Request) error {
	switch r.Op {
	case ListBuckets:
		page, err := w.store.ListAccessibleBuckets(r.HTTP.Context(), storage.BucketListRequest{PrincipalID: r.Caller.UserID, Limit: 1000})
		if err != nil {
			return err
		}
		if slices.ContainsFunc(page.Buckets, func(b storage.Bucket) bool { return b.Name == "demo" }) {
			_, err = io.WriteString(rw, "listed")
		} else {
			_, err = io.WriteString(rw, "hidden")
		}
		return err
	case CopyObject:
		bucket, key, _ := strings.Cut(strings.TrimPrefix(r.HTTP.Header.Get("X-Amz-Copy-Source"), "/"), "/")
		if _, err := r.AuthorizeCopySource(bucket); err != nil {
			return err
		}
		if !w.objects[bucket+"/"+key] {
			return &Error{Code: ErrNoSuchKey}
		}
	case UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts:
		if e := r.CheckUpload(w.upload); e != nil {
			return e
		}
	case GetObject, HeadObject, RestoreObject:
		if !w.objects[r.Bucket+"/"+r.Key] {
			return &Error{Code: ErrNoSuchKey}
		}
	}
	_, err := io.WriteString(rw, "allow")
	return err
}

// request is c's operation on demo/k, or on the bucket and key c names.
func (c authCase) request() *http.Request {
	bucket, key := "demo", "k"
	if c.Bucket != "" {
		bucket = c.Bucket
	}
	if c.Key != "" {
		key = c.Key
	}
	target := map[Operation]string{
		ListBuckets:             "GET /",
		CreateBucket:            "PUT /B",
		HeadBucket:              "HEAD /B",
		GetBucketLocation:       "GET /B?location",
		ListObjectsV2:           "GET /B?list-type=2",
		DeleteObjects:           "POST /B?delete",
		DeleteBucket:            "DELETE /B",
		GetObject:               "GET /B/K",
		HeadObject:              "HEAD /B/K",
		RestoreObject:           "POST /B/K?restore",
		PutObject:               "PUT /B/K",
		DeleteObject:            "DELETE /B/K",
		CopyObject:              "PUT /B/copy",
		CreateMultipartUpload:   "POST /B/K?uploads",
		UploadPart:              "PUT /B/K?partNumber=1&uploadId=UP1",
		CompleteMultipartUpload: "POST /B/K?uploadId=UP1",
		AbortMultipartUpload:    "DELETE /B/K?uploadId=UP1",
		ListParts:               "GET /B/K?uploadId=UP1",
	}[c.Op]
	method, path, _ := strings.Cut(target, " ")
	path = strings.Replace(strings.Replace(path, "/B", "/"+bucket, 1), "/K", "/"+key, 1)
	r := httptest.NewRequest(method, path, nil)
	if c.Op == CopyObject {
		src, srcKey := "demo", "k"
		if c.Source != nil {
			src = "src"
			if c.Source.Bucket != "" {
				src = c.Source.Bucket
			}
			if c.Source.Key != "" {
				srcKey = c.Source.Key
			}
		}
		r.Header.Set("X-Amz-Copy-Source", "/"+src+"/"+srcKey)
	}
	return r
}

// sign adds the SigV4 header of the principal's credential, signing host and
// every x-amz-* header over an empty payload.
func sign(r *http.Request, accessKeyID, secret string) {
	signPayload(r, accessKeyID, secret, emptySHA256)
}

// signPayload is sign with the payload hash the request actually carries; the
// caller sets the matching x-amz-content-sha256 header value.
func signPayload(r *http.Request, accessKeyID, secret, payloadHash string) {
	amzDate := authNow.Format(amzDateLayout)
	r.Header.Set("X-Amz-Date", amzDate)
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	s := sigV4{accessKeyID: accessKeyID, scope: amzDate[:8] + "/" + authRegion + "/s3/aws4_request", amzDate: amzDate, signedHeaders: []string{"host"}, payloadHash: payloadHash}
	for name := range r.Header {
		if lower := strings.ToLower(name); strings.HasPrefix(lower, "x-amz-") {
			s.signedHeaders = append(s.signedHeaders, lower)
		}
	}
	slices.Sort(s.signedHeaders)
	sum := sha256.Sum256([]byte(canonicalRequest(r, r.URL.Query(), &s)))
	sig := hex.EncodeToString(hmacSHA256(signingKey(secret, s.scope), sigV4Algorithm+"\n"+amzDate+"\n"+s.scope+"\n"+hex.EncodeToString(sum[:])))
	r.Header.Set("Authorization", fmt.Sprintf("%s Credential=%s/%s, SignedHeaders=%s, Signature=%s", sigV4Algorithm, accessKeyID, s.scope, strings.Join(s.signedHeaders, ";"), sig))
}

// run sends c by principal through a fresh world and returns the S3 error
// code, or what the handler answered.
func run(t *testing.T, f authFixture, c authCase) string {
	t.Helper()
	w := newAuthWorld(t, f, c)
	r := c.request()
	sign(r, accessKey(c.Principal), "secret-"+c.Principal)
	rec := httptest.NewRecorder()
	// serve, not ServeHTTP: a HEAD error has no body to read the code from.
	err := w.router.serve(rec, r, "TESTREQUEST")
	var e *Error
	switch {
	case err == nil:
		return rec.Body.String()
	case errors.As(err, &e):
		return string(e.Code)
	}
	t.Fatalf("%s by %s: %v", c.Op, c.Principal, err)
	return ""
}

func TestAuthorizationFixture(t *testing.T) {
	var f authFixture
	readFixture(t, "authorization.json", &f)
	ops := map[Operation]bool{}
	for _, row := range f.S3 {
		ops[row.Op] = true
		for name := range f.Principals {
			if _, ok := row.Expect[name]; !ok {
				t.Errorf("%s: no cell for %s", row.Op, name)
			}
		}
		for name, want := range row.Expect {
			if got := run(t, f, authCase{Op: row.Op, Principal: name}); got != want {
				t.Errorf("%s by %s: got %s, want %s", row.Op, name, got, want)
			}
		}
	}
	for _, r := range routes {
		if r.scope <= p1 && !ops[r.op] {
			t.Errorf("%s has no row in s3", r.op)
		}
	}
	for _, c := range f.S3Cases {
		if _, ok := f.Principals[c.Principal]; !ok || !ops[c.Op] {
			t.Errorf("%s: unknown principal %q or operation %q", c.Name, c.Principal, c.Op)
			continue
		}
		if got := run(t, f, c); got != c.Expect {
			t.Errorf("%s: got %s, want %s", c.Name, got, c.Expect)
		}
	}
}
