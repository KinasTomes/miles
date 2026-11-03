package live_test

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// This opt-in fixture mutates only fresh logical identities and buckets. The
// operator provisions infrastructure and approves its teardown independently.
type liveConfig struct {
	Region    string     `json:"region"`
	CAFile    string     `json:"ca_file"`
	TokenFile string     `json:"token_file"`
	Result    string     `json:"result_file"`
	SlowState string     `json:"slow_state_directory"`
	Endpoints []endpoint `json:"endpoints"`
}
type endpoint struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Connect string `json:"connect"`
	Admin   string `json:"admin"`
}
type observation struct {
	Case      string `json:"case"`
	Endpoint  string `json:"endpoint"`
	Method    string `json:"method"`
	Status    int    `json:"status"`
	ElapsedMS int64  `json:"elapsed_ms"`
	FirstMS   int64  `json:"first_byte_ms"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
}
type fixture struct {
	t        *testing.T
	config   liveConfig
	ep       endpoint
	client   *http.Client
	admin    *http.Client
	token    string
	prefix   string
	bucket   string
	bucketID string
	results  *[]observation
}
type credential struct {
	ID     string `json:"access_key_id"`
	Secret string `json:"secret_access_key"`
}

func privateFile(t *testing.T, path string) []byte {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 16384 {
		t.Fatal("fixture configuration/token requires a regular 0600 file of at most 16 KiB")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read fixture configuration/token")
	}
	return b
}

func validateEndpoint(ep endpoint) error {
	u, err := url.Parse(ep.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Port() != "443" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("front door requires an HTTPS origin with explicit :443")
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,9}$`).MatchString(ep.Name) {
		return fmt.Errorf("invalid endpoint label")
	}
	if _, port, err := net.SplitHostPort(ep.Connect); err != nil || port != "443" {
		return fmt.Errorf("connect requires a host and port 443")
	}
	a, err := url.Parse(ep.Admin)
	if err != nil || a.Scheme != "http" || a.Hostname() != "127.0.0.1" || a.Port() == "" || a.User != nil || a.Path != "" || a.RawQuery != "" || a.Fragment != "" {
		return fmt.Errorf("Admin requires an operator-owned loopback HTTP tunnel")
	}
	return nil
}

func TestPresignedLiveFrontDoors(t *testing.T) {
	path := os.Getenv("MILES_PRESIGNED_LIVE_CONFIG")
	if path == "" {
		t.Skip("set MILES_PRESIGNED_LIVE_CONFIG only after live test approval and deployment")
	}
	var c liveConfig
	d := json.NewDecoder(bytes.NewReader(privateFile(t, path)))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || c.Region == "" || c.Result == "" || len(c.Endpoints) != 2 {
		t.Fatal("fixture requires a region, a fresh result path and two front doors")
	}
	for _, ep := range c.Endpoints {
		if err := validateEndpoint(ep); err != nil {
			t.Fatal(err)
		}
	}
	if c.Endpoints[0].Name == c.Endpoints[1].Name {
		t.Fatal("front doors require distinct labels")
	}
	phase := os.Getenv("MILES_PRESIGNED_LIVE_PHASE")
	if phase != "" && phase != "prepare" && phase != "complete" && phase != "preflight" {
		t.Fatal("live phase must be prepare, complete or preflight")
	}
	if phase != "" {
		info, err := os.Lstat(c.SlowState)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
			t.Fatal("slow operation state requires an existing private 0700 directory")
		}
	}
	ca, err := os.ReadFile(c.CAFile)
	roots := x509.NewCertPool()
	if err != nil || !roots.AppendCertsFromPEM(ca) {
		t.Fatal("cannot load reviewed TLS trust")
	}
	out, err := os.OpenFile(c.Result, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal("result path must be new")
	}
	results := []observation{}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	prefix := "acceptance-" + hex.EncodeToString(nonce[:])
	t.Cleanup(func() {
		defer out.Close()
		err := json.NewEncoder(out).Encode(struct {
			Prefix         string        `json:"resource_prefix"`
			Passed         bool          `json:"passed"`
			TimeoutSeconds int           `json:"timeout_seconds"`
			Cases          []observation `json:"cases"`
		}{prefix, !t.Failed(), 180, results})
		if err != nil {
			t.Error("cannot save sanitized evidence")
		}
	})
	token := strings.TrimSpace(string(privateFile(t, c.TokenFile)))
	if token == "" {
		t.Fatal("empty system Admin token")
	}
	for _, ep := range c.Endpoints {
		t.Run(ep.Name, func(t *testing.T) {
			transport := &http.Transport{Proxy: nil, DisableCompression: true,
				TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
				DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
					return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, ep.Connect)
				}}
			defer transport.CloseIdleConnections()
			noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			adminTransport := &http.Transport{Proxy: nil}
			defer adminTransport.CloseIdleConnections()
			f := fixture{t: t, config: c, ep: ep, prefix: prefix + "-" + ep.Name, token: token, results: &results,
				client: &http.Client{Transport: transport, Timeout: 180 * time.Second, CheckRedirect: noRedirect},
				admin:  &http.Client{Transport: adminTransport, Timeout: 30 * time.Second, CheckRedirect: noRedirect}}
			if phase == "complete" || phase == "preflight" {
				f.slowOperation(phase)
				return
			}
			owner := f.createUser("owner")
			f.bucket = f.prefix
			f.s3("create bucket", owner, "PUT", "", "", nil, nil, false, 200, "")
			var policy struct {
				BucketID string `json:"bucket_id"`
			}
			f.adminRequest("GET", "buckets/"+f.bucket+"/policy", nil, token, 200, &policy)
			f.bucketID = policy.BucketID
			if f.bucketID == "" {
				t.Fatal("missing bucket identity")
			}
			if phase == "prepare" {
				f.prepareSlow(owner, filepath.Join(c.SlowState, ep.Name+".json"))
				return
			}
			f.compatibility(owner)
			f.authorization(owner)
		})
	}
}

func (f *fixture) adminRequest(method, path string, body any, token string, status int, output any) {
	f.t.Helper()
	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			f.t.Fatal("invalid Admin fixture payload")
		}
	}
	r, err := http.NewRequest(method, f.ep.Admin+"/admin/v1/"+path, bytes.NewReader(payload))
	if err != nil {
		f.t.Fatal("cannot build Admin request")
	}
	r.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	response, err := f.admin.Do(r)
	if err != nil {
		f.t.Fatal("Admin transport failed")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil || response.StatusCode != status {
		f.t.Fatalf("Admin %s returned %d, expected %d", method, response.StatusCode, status)
	}
	if output != nil && json.Unmarshal(data, output) != nil {
		f.t.Fatal("invalid Admin response")
	}
}

func (f *fixture) createUser(label string) credential {
	f.t.Helper()
	id := f.prefix + "-" + label
	f.adminRequest("POST", "users", map[string]any{"user_id": id, "default_domain": f.prefix, "logical_limit": 1 << 30}, f.token, 201, nil)
	var c credential
	f.adminRequest("POST", "credentials", map[string]any{"user_id": id}, f.token, 201, &c)
	if c.ID == "" || c.Secret == "" {
		f.t.Fatal("missing synthetic credential")
	}
	return c
}

func (f *fixture) grant(label string, read bool) {
	f.t.Helper()
	f.adminRequest("PUT", "buckets/"+f.bucket+"/grants/"+f.prefix+"-"+label,
		map[string]any{"read": read, "write": true, "expected_revision": 0, "expected_bucket_id": f.bucketID}, f.token, 200, nil)
}

func (f *fixture) signed(c credential, method, key, query string, body []byte, headers map[string]string, presigned bool) *http.Request {
	f.t.Helper()
	r, err := http.NewRequest(method, f.ep.URL+"/"+f.bucket+key, bytes.NewReader(body))
	if err != nil {
		f.t.Fatal("cannot build S3 fixture request")
	}
	r.URL.RawQuery = query
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	creds := aws.Credentials{AccessKeyID: c.ID, SecretAccessKey: c.Secret}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if presigned {
		q := r.URL.Query()
		q.Set("X-Amz-Expires", "604800")
		r.URL.RawQuery = q.Encode()
		uri, signedHeaders, err := signer.PresignHTTP(context.Background(), creds, r, "UNSIGNED-PAYLOAD", "s3", f.config.Region, time.Now().UTC())
		if err != nil {
			f.t.Fatal("cannot presign S3 fixture request")
		}
		r, err = http.NewRequest(method, uri, bytes.NewReader(body))
		if err != nil {
			f.t.Fatal("invalid presigned request")
		}
		r.Header = signedHeaders
	} else {
		sum := sha256.Sum256(body)
		hash := hex.EncodeToString(sum[:])
		r.Header.Set("X-Amz-Content-Sha256", hash)
		if signer.SignHTTP(context.Background(), creds, r, hash, "s3", f.config.Region, time.Now().UTC()) != nil {
			f.t.Fatal("cannot sign S3 fixture request")
		}
	}
	return r
}

func (f *fixture) send(label string, r *http.Request, status int, code string) ([]byte, http.Header) {
	f.t.Helper()
	start := time.Now()
	var first time.Duration
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), &httptrace.ClientTrace{GotFirstResponseByte: func() { first = time.Since(start) }}))
	response, err := f.client.Do(r)
	if err != nil {
		f.t.Fatalf("%s transport failed", label)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 16<<20))
	sum := sha256.Sum256(body)
	*f.results = append(*f.results, observation{label, f.ep.Name, r.Method, response.StatusCode, time.Since(start).Milliseconds(), first.Milliseconds(), len(body), hex.EncodeToString(sum[:])})
	if err != nil || response.StatusCode != status {
		f.t.Fatalf("%s returned HTTP %d, expected %d", label, response.StatusCode, status)
	}
	if code != "" {
		var e struct {
			Code string `xml:"Code"`
		}
		if xml.Unmarshal(body, &e) != nil || e.Code != code {
			f.t.Fatalf("%s returned an unexpected S3 error", label)
		}
	}
	return body, response.Header
}

func (f *fixture) s3(label string, c credential, method, key, query string, body []byte, headers map[string]string, presigned bool, status int, code string) ([]byte, http.Header) {
	f.t.Helper()
	return f.send(label, f.signed(c, method, key, query, body, headers, presigned), status, code)
}

func (f *fixture) compatibility(c credential) {
	payload := []byte("native presigned payload\x00\xff\n")
	for _, key := range []string{"/unicode/caf%C3%A9%20%2B%25", "/encoded%2Fslash", "/double//slash", "/dots/./../kept"} {
		f.s3("presigned special-path PUT", c, "PUT", key, "", payload, map[string]string{"X-Amz-Meta-With_Underscore": "kept"}, true, 200, "")
		got, _ := f.s3("presigned special-path GET", c, "GET", key, "", nil, nil, true, 200, "")
		if !bytes.Equal(got, payload) {
			f.t.Fatal("presigned read-back differs")
		}
		got, headers := f.s3("HEAD metadata", c, "HEAD", key, "", nil, nil, false, 200, "")
		if len(got) != 0 || headers.Get("Content-Length") != fmt.Sprint(len(payload)) || headers.Get("X-Amz-Meta-With_Underscore") != "kept" {
			f.t.Fatal("HEAD metadata differs")
		}
		got, _ = f.s3("Range", c, "GET", key, "", nil, map[string]string{"Range": "bytes=2-8"}, true, 206, "")
		if !bytes.Equal(got, payload[2:9]) {
			f.t.Fatal("Range bytes differ")
		}
	}
	f.s3("CopyObject", c, "PUT", "/copy", "", nil, map[string]string{"X-Amz-Copy-Source": "/" + f.bucket + "/unicode/caf%C3%A9%20%2B%25"}, true, 200, "")
	got, _ := f.s3("copy read-back", c, "GET", "/copy", "", nil, nil, true, 200, "")
	if !bytes.Equal(got, payload) {
		f.t.Fatal("copy bytes differ")
	}
	f.s3("DeleteObject", c, "DELETE", "/copy", "", nil, nil, false, 204, "")
	f.s3("missing GET", c, "GET", "/copy", "", nil, nil, true, 404, "NoSuchKey")
	got, _ = f.s3("missing HEAD", c, "HEAD", "/copy", "", nil, nil, false, 404, "")
	if len(got) != 0 {
		f.t.Fatal("HEAD error has a body")
	}
	sum := sha256.Sum256(payload)
	query := "x-amz-checksum-sha256=" + url.QueryEscape(base64.StdEncoding.EncodeToString(sum[:]))
	f.s3("signed query checksum", c, "PUT", "/checksum", query, bytes.Repeat([]byte("x"), len(payload)), nil, true, 400, "BadDigest")
	f.s3("bad checksum not published", c, "HEAD", "/checksum", "", nil, nil, false, 404, "")
	for _, key := range []string{"/batch-a", "/batch-b"} {
		f.s3("batch fixture PUT", c, "PUT", key, "", payload, nil, true, 200, "")
	}
	batch := []byte("<Delete><Object><Key>batch-a</Key></Object><Object><Key>batch-b</Key></Object></Delete>")
	md5sum := md5.Sum(batch)
	deleted, _ := f.s3("DeleteObjects", c, "POST", "", "delete=", batch,
		map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(md5sum[:])}, false, 200, "")
	var deletion struct {
		Deleted []struct{} `xml:"Deleted"`
		Errors  []struct{} `xml:"Error"`
	}
	if xml.Unmarshal(deleted, &deletion) != nil || len(deletion.Deleted) != 2 || len(deletion.Errors) != 0 {
		f.t.Fatal("batch deletion did not delete both keys")
	}
	f.s3("batch deletion visible", c, "HEAD", "/batch-a", "", nil, nil, false, 404, "")
	f.multipart(c, c, false)
	body, _ := f.s3("abort fixture create", c, "POST", "/abort", "uploads=", nil, nil, true, 200, "")
	var upload struct {
		ID string `xml:"UploadId"`
	}
	if xml.Unmarshal(body, &upload) != nil || upload.ID == "" {
		f.t.Fatal("missing abort upload identity")
	}
	f.s3("AbortMultipartUpload", c, "DELETE", "/abort", "uploadId="+url.QueryEscape(upload.ID), nil, nil, true, 204, "")
	f.s3("aborted upload unavailable", c, "GET", "/abort", "uploadId="+url.QueryEscape(upload.ID), nil, nil, true, 404, "NoSuchUpload")
}

func (f *fixture) multipart(c, other credential, crossUser bool) {
	f.t.Helper()
	key := "/multipart"
	if crossUser {
		key = "/write-only"
	}
	body, _ := f.s3("CreateMultipartUpload", c, "POST", key, "uploads=", nil, nil, true, 200, "")
	var upload struct {
		ID string `xml:"UploadId"`
	}
	if xml.Unmarshal(body, &upload) != nil || upload.ID == "" {
		f.t.Fatal("missing upload identity")
	}
	query := "uploadId=" + url.QueryEscape(upload.ID)
	payload := []byte("multipart actual read-back\x00\xff")
	if crossUser {
		f.s3("cross-user UploadPart denied", other, "PUT", key, query+"&partNumber=1", payload, nil, true, 403, "AccessDenied")
		f.s3("write-only ListParts denied", c, "GET", key, query, nil, nil, true, 403, "AccessDenied")
	}
	_, head := f.s3("UploadPart", c, "PUT", key, query+"&partNumber=1", payload, nil, true, 200, "")
	if head.Get("ETag") == "" {
		f.t.Fatal("missing part ETag")
	}
	if !crossUser {
		f.s3("ListParts", c, "GET", key, query, nil, nil, true, 200, "")
	}
	complete := []byte("<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>" + head.Get("ETag") + "</ETag></Part></CompleteMultipartUpload>")
	if crossUser {
		f.s3("cross-user Complete denied", other, "POST", key, query, complete, nil, true, 403, "AccessDenied")
	}
	f.s3("CompleteMultipartUpload", c, "POST", key, query, complete, nil, true, 200, "")
	reader := c
	if crossUser {
		reader = other
		f.s3("write-only GET denied", c, "GET", key, "", nil, nil, true, 403, "AccessDenied")
	}
	got, _ := f.s3("multipart read-back", reader, "GET", key, "", nil, nil, true, 200, "")
	if !bytes.Equal(got, payload) {
		f.t.Fatal("multipart read-back differs")
	}
}

func (f *fixture) authorization(owner credential) {
	f.t.Helper()
	payload := []byte("authorization snapshot fixture")
	for _, mutation := range []string{"credential", "grant", "user"} {
		c := f.createUser(mutation)
		f.grant(mutation, true)
		key := "/revocation-" + mutation
		get := f.signed(c, "GET", key, "", nil, nil, true)
		put := f.signed(c, "PUT", key, "", payload, nil, true)
		f.send("before "+mutation+" revoke PUT", put.Clone(context.Background()), 200, "")
		f.send("before "+mutation+" revoke GET", get.Clone(context.Background()), 200, "")
		switch mutation {
		case "credential":
			f.adminRequest("DELETE", "credentials/"+c.ID, nil, f.token, 204, nil)
		case "grant":
			f.adminRequest("DELETE", "buckets/"+f.bucket+"/grants/"+f.prefix+"-grant?expected_bucket_id="+url.QueryEscape(f.bucketID), nil, f.token, 204, nil)
		case "user":
			f.adminRequest("PATCH", "users/"+f.prefix+"-user", map[string]any{"status": "disabled", "expected_revision": 1}, f.token, 200, nil)
		}
		time.Sleep(5100 * time.Millisecond)
		code := "AccessDenied"
		if mutation == "credential" {
			code = "InvalidAccessKeyId"
		}
		f.send("replayed URL after "+mutation+" revoke GET", get.Clone(context.Background()), 403, code)
		retry := put.Clone(context.Background())
		var err error
		retry.Body, err = put.GetBody()
		if err != nil {
			f.t.Fatal("cannot replay PUT body")
		}
		f.send("replayed URL after "+mutation+" revoke PUT", retry, 403, code)
	}
	writer := f.createUser("writer")
	f.grant("writer", false)
	f.multipart(writer, owner, true)
	body, _ := f.s3("write-only HEAD error", writer, "HEAD", "/write-only", "", nil, nil, true, 403, "")
	if len(body) != 0 {
		f.t.Fatal("write-only HEAD error has a body")
	}
	var token struct {
		ID    string `json:"token_id"`
		Token string `json:"token"`
	}
	f.adminRequest("POST", "admin-tokens", map[string]any{"user_id": f.prefix + "-owner", "expires_in_seconds": 900}, f.token, 201, &token)
	if token.ID == "" || token.Token == "" {
		f.t.Fatal("missing synthetic owner token")
	}
	f.adminRequest("DELETE", "admin-tokens/"+token.ID, nil, f.token, 204, nil)
	time.Sleep(5100 * time.Millisecond)
	f.adminRequest("PUT", "buckets/"+f.bucket+"/grants/"+f.prefix+"-writer", map[string]any{"read": true, "write": true, "expected_revision": 1, "expected_bucket_id": f.bucketID}, token.Token, 401, nil)
	f.s3("revoked bearer did not elevate data grant", writer, "GET", "/write-only", "", nil, nil, true, 403, "AccessDenied")
}

func TestLiveEndpointTrustBoundaries(t *testing.T) {
	valid := endpoint{Name: "cloud", URL: "https://cloud.example:443", Connect: "alb.example:443", Admin: "http://127.0.0.1:9001"}
	if err := validateEndpoint(valid); err != nil {
		t.Fatal(err)
	}
	for _, modify := range []func(*endpoint){
		func(e *endpoint) { e.URL = "http://cloud.example:443" },
		func(e *endpoint) { e.URL = "https://cloud.example" },
		func(e *endpoint) { e.Admin = "http://external.example:9001" },
		func(e *endpoint) { e.Admin = "http://127.0.0.1:9001?redirect=1" },
		func(e *endpoint) { e.Connect = "alb.example:80" },
	} {
		ep := valid
		modify(&ep)
		if validateEndpoint(ep) == nil {
			t.Fatal("unsafe fixture endpoint accepted")
		}
	}
}
