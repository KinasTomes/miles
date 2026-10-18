// Command s3capture regenerates tests/fixtures/s3/transport.json, the SigV4,
// aws-chunked and checksum fixture of API v2 sections 2.2, 2.7 and 11.
//
// It runs the pinned AWS CLI, boto3 and curl against a local recording S3
// endpoint over verified TLS (a throwaway CA passed as AWS_CA_BUNDLE and curl
// --cacert), keeps every raw request with the canonical request the client
// logged, derives single-fault variants and adds the two AWS SigV4 streaming
// examples. Before writing it checks every captured signature, checksum and
// Content-MD5 against the stated payload, and fails if the router (s3.Resolve)
// does not give the expected error.
//
// Run from the repository root with AWS CLI and curl in PATH:
//
//	uv run --no-project --python 3.13 --with-requirements scripts/s3capture/requirements.txt -- go run ./scripts/s3capture
package main

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/KinasTomes/miles/internal/api/s3"
)

// AWS documentation example credentials: public, never valid on AWS.
const (
	accessKeyID     = "AKIAIOSFODNN7EXAMPLE"
	secretAccessKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	region          = "us-east-1"
	payload         = "Miles transport fixture payload\n"
	uploadID        = "UP1"
	amzDateLayout   = "20060102T150405Z"
	maxBody         = 64 << 10 // larger bodies are stored as body_omitted
	emptySHA256     = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// awsConfig is the AWS_CONFIG_FILE of every client. default keeps the CLI and
// botocore defaults (request_checksum_calculation and
// response_checksum_validation when_supported, multipart_threshold and
// multipart_chunksize 8MB); the other profiles change one setting each.
const awsConfig = `[default]
region = us-east-1
s3 =
  addressing_style = path

[profile when-required]
region = us-east-1
request_checksum_calculation = when_required
response_checksum_validation = when_required
s3 =
  addressing_style = path

[profile multipart-5mb]
region = us-east-1
s3 =
  addressing_style = path
  multipart_threshold = 5MB
  multipart_chunksize = 5MB
`

// boto3Prelude builds the client of every boto3 capture. botocore logs the
// canonical request and signature of each request it signs to stderr.
const boto3Prelude = `import logging, os, sys
import boto3
from botocore.config import Config
logging.basicConfig(stream=sys.stderr, format="%(asctime)s - %(threadName)s - %(name)s - %(levelname)s - %(message)s")
logging.getLogger("botocore.auth").setLevel(logging.DEBUG)
s3 = boto3.client("s3", endpoint_url=os.environ["ENDPOINT"], config=Config(s3={"addressing_style": "path"}))
`

type fixture struct {
	Clients     map[string]string `json:"clients"`
	Config      map[string]string `json:"config"`
	Credentials map[string]string `json:"credentials"`
	Cases       []*testCase       `json:"cases"`
}

// testCase is one request of transport.json. Request and Body are the bytes
// on the wire: head lines joined by CRLF, a blank line, then Body (still in
// HTTP chunked and aws-chunked framing); BodyOmitted is the length of a body
// too large to keep. Now is the server clock to verify it with. Error is the
// expected S3 error, empty when accepted; Op is the route when s3.Resolve
// accepts it. PayloadSHA256 is the decoded payload an accepted request
// carries.
type testCase struct {
	Name             string   `json:"name"`
	Client           string   `json:"client"`
	Source           string   `json:"source,omitempty"`
	From             string   `json:"from,omitempty"`
	Note             string   `json:"note,omitempty"`
	Now              string   `json:"now"`
	Op               string   `json:"op,omitempty"`
	Error            string   `json:"error,omitempty"`
	PayloadSHA256    string   `json:"payload_sha256,omitempty"`
	BodyOmitted      int      `json:"body_omitted,omitempty"`
	Request          []string `json:"request"`
	Body             string   `json:"body,omitempty"`
	CanonicalRequest string   `json:"canonical_request,omitempty"`
}

// capture is one client invocation. args are the AWS CLI arguments, or for
// boto3 the statements run after boto3Prelude. A presign capture prints a URL
// that curl then requests with curl arguments. error is the expected error of
// every request it sends; fail means the router rejects it, so the client
// exits non-zero.
type capture struct {
	name   string
	client string
	args   []string
	curl   []string
	plain  bool // plain HTTP endpoint instead of HTTPS
	error  s3.ErrorCode
	fail   bool
	note   string
}

var captures = []capture{
	{name: "cli-put-default", client: "aws-cli", args: []string{"s3", "cp", "small.txt", "s3://demo/small.txt"},
		note: "HTTPS default: CRC64NVME trailer, Transfer-Encoding chunked + aws-chunked, STREAMING-UNSIGNED-PAYLOAD-TRAILER"},
	{name: "cli-put-crc32", client: "aws-cli", args: []string{"s3", "cp", "small.txt", "s3://demo/small.txt", "--checksum-algorithm", "CRC32"}},
	{name: "cli-put-crc32c", client: "aws-cli", args: []string{"s3", "cp", "small.txt", "s3://demo/small.txt", "--checksum-algorithm", "CRC32C"}},
	{name: "cli-put-sha1", client: "aws-cli", args: []string{"s3", "cp", "small.txt", "s3://demo/small.txt", "--checksum-algorithm", "SHA1"}},
	{name: "cli-put-sha256", client: "aws-cli", args: []string{"s3", "cp", "small.txt", "s3://demo/small.txt", "--checksum-algorithm", "SHA256"}},
	{name: "cli-put-empty", client: "aws-cli", args: []string{"s3", "cp", "empty.txt", "s3://demo/empty.txt"}},
	{name: "cli-put-unicode-key", client: "aws-cli", args: []string{"s3", "cp", "small.txt", "s3://demo/café a+b%.txt"},
		note: "canonical URI is the request path as sent; the key is not encoded twice"},
	{name: "cli-put-http", client: "aws-cli", args: []string{"s3", "cp", "small.txt", "s3://demo/small.txt"}, plain: true,
		note: "plain HTTP: hex payload SHA-256 and CRC64NVME header, no aws-chunked"},
	{name: "cli-put-when-required", client: "aws-cli", args: []string{"s3", "cp", "small.txt", "s3://demo/small.txt", "--profile", "when-required"},
		note: "request_checksum_calculation = when_required: no checksum and no aws-chunked, so the CLI signs the hex payload SHA-256; DSG stores CRC64NVME (API v2 2.7)"},
	{name: "cli-put-content-md5", client: "aws-cli", args: []string{"s3api", "put-object", "--bucket", "demo", "--key", "md5.txt", "--body", "small.txt", "--content-md5", b64(md5.Sum([]byte(payload)))}},
	{name: "cli-put-content-md5-wrong", client: "aws-cli", args: []string{"s3api", "put-object", "--bucket", "demo", "--key", "md5.txt", "--body", "small.txt", "--content-md5", b64(md5.Sum([]byte("x")))},
		error: s3.ErrBadDigest, note: "Content-MD5 is checked apart from the checksum algorithm"},
	{name: "cli-put-sha512", client: "aws-cli", args: []string{"s3api", "put-object", "--bucket", "demo", "--key", "small.txt", "--body", "small.txt", "--checksum-algorithm", "SHA512"},
		error: s3.ErrInvalidRequest, fail: true, note: "algorithm outside API v2 2.7"},
	{name: "cli-put-xxhash64", client: "aws-cli", args: []string{"s3api", "put-object", "--bucket", "demo", "--key", "small.txt", "--body", "small.txt", "--checksum-algorithm", "XXHASH64"},
		error: s3.ErrInvalidRequest, fail: true, note: "algorithm outside API v2 2.7"},
	{name: "cli-get", client: "aws-cli", args: []string{"s3", "cp", "s3://demo/small.txt", "-"},
		note: "HEAD then GET with x-amz-checksum-mode ENABLED; bodiless requests sign the SHA-256 of the empty string"},
	{name: "cli-list", client: "aws-cli", args: []string{"s3", "ls", "s3://demo/"}},
	{name: "cli-mpu-create-crc64nvme-full-object", client: "aws-cli", args: []string{"s3api", "create-multipart-upload", "--bucket", "demo", "--key", "mpu.bin", "--checksum-algorithm", "CRC64NVME", "--checksum-type", "FULL_OBJECT"}},
	{name: "cli-mpu-create-crc32", client: "aws-cli", args: []string{"s3api", "create-multipart-upload", "--bucket", "demo", "--key", "mpu.bin", "--checksum-algorithm", "CRC32"},
		note: "no x-amz-checksum-type: COMPOSITE (API v2 2.7)"},
	{name: "cli-mpu-create-sha256-composite", client: "aws-cli", args: []string{"s3api", "create-multipart-upload", "--bucket", "demo", "--key", "mpu.bin", "--checksum-algorithm", "SHA256", "--checksum-type", "COMPOSITE"}},
	{name: "cli-mpu-create-sha1-full-object", client: "aws-cli", args: []string{"s3api", "create-multipart-upload", "--bucket", "demo", "--key", "mpu.bin", "--checksum-algorithm", "SHA1", "--checksum-type", "FULL_OBJECT"},
		error: s3.ErrInvalidRequest, fail: true, note: "SHA1 has no FULL_OBJECT type (API v2 2.7)"},
	{name: "cli-mpu-create-crc64nvme-composite", client: "aws-cli", args: []string{"s3api", "create-multipart-upload", "--bucket", "demo", "--key", "mpu.bin", "--checksum-algorithm", "CRC64NVME", "--checksum-type", "COMPOSITE"},
		error: s3.ErrInvalidRequest, fail: true, note: "CRC64NVME has no COMPOSITE type (API v2 2.7)"},
	{name: "cli-mpu-part-default", client: "aws-cli", args: []string{"s3api", "upload-part", "--bucket", "demo", "--key", "mpu.bin", "--upload-id", uploadID, "--part-number", "1", "--body", "small.txt"}},
	{name: "cli-mpu-part-sha256", client: "aws-cli", args: []string{"s3api", "upload-part", "--bucket", "demo", "--key", "mpu.bin", "--upload-id", uploadID, "--part-number", "1", "--body", "small.txt", "--checksum-algorithm", "SHA256"}},
	{name: "cli-mpu-complete-composite", client: "aws-cli", args: []string{"s3api", "complete-multipart-upload", "--bucket", "demo", "--key", "mpu.bin", "--upload-id", uploadID, "--checksum-type", "COMPOSITE", "--multipart-upload", "file://parts-sha256.json"}},
	{name: "cli-mpu-complete-full-object", client: "aws-cli", args: []string{"s3api", "complete-multipart-upload", "--bucket", "demo", "--key", "mpu.bin", "--upload-id", uploadID, "--checksum-type", "FULL_OBJECT", "--checksum-crc64-nvme", checksum("crc64nvme", []byte(payload)), "--multipart-upload", "file://parts-crc64nvme.json"}},
	{name: "cli-cp-multipart", client: "aws-cli", args: []string{"s3", "cp", "big.bin", "s3://demo/big.bin", "--profile", "multipart-5mb"},
		note: "multipart_threshold/chunksize 5MB, file 5 MiB + 32 bytes. Create sends only x-amz-checksum-algorithm CRC64NVME (FULL_OBJECT by default); parts use the trailer; Complete carries no part or object checksum"},
	{name: "cli-presign-get", client: "aws-cli", args: []string{"s3", "presign", "s3://demo/small.txt", "--expires-in", "900"}, curl: []string{}},
	{name: "boto3-put-default", client: "boto3", args: []string{`s3.put_object(Bucket="demo", Key="small.txt", Body=open("small.txt", "rb").read())`},
		note: "boto3 without awscrt defaults to CRC32, unlike the AWS CLI"},
	{name: "boto3-put-multichunk", client: "boto3", args: []string{`class ShortReads:
    def __init__(self, data): self.data = data
    def read(self, size=-1):
        chunk, self.data = self.data[:7], self.data[7:]
        return chunk
data = open("small.txt", "rb").read()
s3.put_object(Bucket="demo", Key="chunks.txt", Body=ShortReads(data), ContentLength=len(data))`},
		note: "botocore cuts one aws-chunked chunk per read(): chunk sizes are not fixed and may be below 8 KiB"},
	{name: "boto3-put-debug-signed", client: "boto3", args: []string{`def add_debug(request, **kwargs):
    request.headers["x-dsg-debug"] = "1"
s3.meta.events.register("before-sign.s3.PutObject", add_debug)
s3.put_object(Bucket="demo", Key="debug.bin", Body=b"demo")`},
		note: "API v2 11 example: x-dsg-debug is in SignedHeaders"},
	{name: "boto3-presign-default", client: "boto3", args: []string{`print(s3.generate_presigned_url("get_object", Params={"Bucket": "demo", "Key": "small.txt"}, ExpiresIn=900))`},
		curl: []string{}, error: s3.ErrNotImplemented, fail: true,
		note: "without signature_version boto3 presigns with SigV2 (AWSAccessKeyId/Signature/Expires); DSG has no SigV2"},
	{name: "boto3-presign-put", client: "boto3", args: []string{`v4 = boto3.client("s3", endpoint_url=os.environ["ENDPOINT"], config=Config(signature_version="s3v4", s3={"addressing_style": "path"}))
print(v4.generate_presigned_url("put_object", Params={"Bucket": "demo", "Key": "presigned.txt"}, ExpiresIn=900))`},
		curl: []string{"-T", "small.txt"}},
	{name: "boto3-presign-expires-over-7-days", client: "boto3", args: []string{`v4 = boto3.client("s3", endpoint_url=os.environ["ENDPOINT"], config=Config(signature_version="s3v4", s3={"addressing_style": "path"}))
print(v4.generate_presigned_url("get_object", Params={"Bucket": "demo", "Key": "small.txt"}, ExpiresIn=604801))`},
		curl: []string{}, error: s3.ErrInvalidArgument, note: "X-Amz-Expires must be 1..604800"},
}

func main() {
	out := flag.String("out", "tests/fixtures/s3/transport.json", "fixture file to write")
	flag.Parse()
	log.SetFlags(0)

	dir, err := os.MkdirTemp("", "s3capture")
	check(err)
	defer os.RemoveAll(dir)
	srv := newServer(dir)
	env := clientEnv(dir)

	f := fixture{
		Clients: map[string]string{
			"aws-cli": run(dir, env, "aws", "--version"),
			"boto3": run(dir, env, "python", "-c", `import importlib.util, platform, boto3, botocore, s3transfer, urllib3
print(f"boto3 {boto3.__version__}, botocore {botocore.__version__}, s3transfer {s3transfer.__version__}, urllib3 {urllib3.__version__}, "
      f"awscrt {'installed' if importlib.util.find_spec('awscrt') else 'not installed'}, Python {platform.python_version()}")`),
			"curl": strings.TrimSpace(strings.SplitN(run(dir, env, "curl", "--version"), "\n", 2)[0]),
		},
		Config: map[string]string{
			"aws_config":    awsConfig,
			"boto3_prelude": boto3Prelude,
			"endpoint":      "https://127.0.0.1:<port> (plain: http://127.0.0.1:<port>), path-style",
			"tls":           "throwaway CA via AWS_CA_BUNDLE and curl --cacert; chain and host verification on (curl --ssl-revoke-best-effort: the throwaway CA has no CRL, other curl TLS backends ignore the flag)",
		},
		Credentials: map[string]string{"access_key_id": accessKeyID, "secret_access_key": secretAccessKey, "region": region},
	}
	for _, c := range captures {
		f.Cases = append(f.Cases, srv.capture(c, dir, env)...)
	}
	f.Cases = append(f.Cases, awsDocsExamples()...)
	f.Cases = append(f.Cases, variants(f.Cases)...)
	if ua := userAgent(f.Cases); ua != "" {
		f.Clients["aws-cli"] += " (" + ua + ")"
	}
	writeFixture(*out, f)
	log.Printf("wrote %d cases to %s", len(f.Cases), *out)
}

func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

// userAgent returns the awscrt token the AWS CLI reports in its User-Agent.
func userAgent(cases []*testCase) string {
	re := regexp.MustCompile(`md/awscrt#[0-9.]+`)
	for _, c := range cases {
		if c.Client == "aws-cli" {
			for _, l := range c.Request {
				if m := re.FindString(l); m != "" {
					return strings.Replace(m, "md/awscrt#", "awscrt ", 1)
				}
			}
		}
	}
	return ""
}

// clientEnv isolates the clients from the user's AWS configuration: only the
// example credentials, awsConfig and the throwaway CA are visible. An
// explicit --profile ignores AWS_ACCESS_KEY_ID, so every profile gets the
// credentials in the shared credentials file.
func clientEnv(dir string) []string {
	var creds strings.Builder
	for _, p := range []string{"default", "when-required", "multipart-5mb"} {
		fmt.Fprintf(&creds, "[%s]\naws_access_key_id = %s\naws_secret_access_key = %s\n", p, accessKeyID, secretAccessKey)
	}
	files := map[string]string{
		"config":               awsConfig,
		"credentials":          creds.String(),
		"small.txt":            payload,
		"empty.txt":            "",
		"big.bin":              strings.Repeat(payload, (5<<20)/len(payload)) + payload,
		"parts-sha256.json":    `{"Parts":[{"PartNumber":1,"ETag":"\"` + md5hex(payload) + `\"","ChecksumSHA256":"` + checksum("sha256", []byte(payload)) + `"}]}`,
		"parts-crc64nvme.json": `{"Parts":[{"PartNumber":1,"ETag":"\"` + md5hex(payload) + `\"","ChecksumCRC64NVME":"` + checksum("crc64nvme", []byte(payload)) + `"}]}`,
	}
	for name, content := range files {
		check(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	env := []string{
		"HOME=" + dir,
		"USERPROFILE=" + dir,
		"AWS_CONFIG_FILE=" + filepath.Join(dir, "config"),
		"AWS_SHARED_CREDENTIALS_FILE=" + filepath.Join(dir, "credentials"),
		"AWS_CA_BUNDLE=" + filepath.Join(dir, "ca.pem"),
		"AWS_EC2_METADATA_DISABLED=true",
		"AWS_PAGER=",
	}
	for _, k := range []string{"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "TMPDIR"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func run(dir string, env []string, name string, args ...string) string {
	out, stderr, err := execute(dir, env, name, args...)
	if err != nil {
		log.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, tail(stderr))
	}
	return out
}

func execute(dir string, env []string, name string, args ...string) (stdout, stderr string, err error) {
	var out, errOut bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, &out, &errOut
	err = cmd.Run()
	return strings.TrimSpace(out.String()), errOut.String(), err
}

type server struct {
	https, http string
	mu          sync.Mutex
	got         []*captured
}

type captured struct {
	raw   []byte
	req   *http.Request
	body  []byte // after HTTP transfer decoding, still aws-chunked
	route s3.Route
	err   *s3.Error
}

func newServer(dir string) *server {
	caPEM, cert := certificates()
	check(os.WriteFile(filepath.Join(dir, "ca.pem"), caPEM, 0o600))
	tl, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	check(err)
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	check(err)
	s := &server{https: "https://" + tl.Addr().String(), http: "http://" + pl.Addr().String()}
	go s.accept(tl)
	go s.accept(pl)
	return s
}

// certificates returns a CA certificate and a server certificate for
// 127.0.0.1 signed by it; both expire in a day.
func certificates() ([]byte, tls.Certificate) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "s3capture CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	check(err)
	ca, err := x509.ParseCertificate(caDER)
	check(err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		NotBefore:   now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	check(err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (s *server) accept(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.serve(c)
	}
}

// serve records one request per connection exactly as read from the wire,
// then answers it and closes the connection.
func (s *server) serve(c net.Conn) {
	defer c.Close()
	var raw bytes.Buffer
	req, err := http.ReadRequest(bufio.NewReader(io.TeeReader(c, &raw)))
	if err != nil {
		return
	}
	if strings.EqualFold(req.Header.Get("Expect"), "100-continue") {
		io.WriteString(c, "HTTP/1.1 100 Continue\r\n\r\n")
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		log.Printf("%s %s: reading body: %v", req.Method, req.RequestURI, err)
		return
	}
	got := &captured{raw: slices.Clone(raw.Bytes()), req: req, body: body}
	got.route, got.err = s3.Resolve(req)
	rec := httptest.NewRecorder()
	respond(rec, got)
	s.mu.Lock()
	s.got = append(s.got, got)
	s.mu.Unlock()

	h := rec.Header()
	if h.Get("Content-Length") == "" {
		h.Set("Content-Length", strconv.Itoa(rec.Body.Len()))
	}
	h.Set("Connection", "close")
	fmt.Fprintf(c, "HTTP/1.1 %d %s\r\n", rec.Code, http.StatusText(rec.Code))
	h.Write(c)
	io.WriteString(c, "\r\n")
	if req.Method != http.MethodHead {
		c.Write(rec.Body.Bytes())
	}
}

// respond answers like S3 just enough for the client to finish. GET and HEAD
// serve payload, with its CRC64NVME when the client asks for checksums.
func respond(w *httptest.ResponseRecorder, g *captured) {
	if g.err != nil {
		s3.WriteError(w, g.req, g.err, "S3CAPTURE")
		return
	}
	h := w.Header()
	h.Set("x-amz-request-id", "S3CAPTURE")
	xml := func(body string) {
		h.Set("Content-Type", "application/xml")
		io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+body)
	}
	switch g.route.Op {
	case s3.PutObject, s3.UploadPart:
		data, _, err := decodePayload(g.req.Header, g.body)
		check(err)
		h.Set("ETag", `"`+md5hex(string(data))+`"`)
	case s3.CreateMultipartUpload:
		xml(`<InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>` + g.route.Bucket + `</Bucket><Key>` + g.route.Key + `</Key><UploadId>` + uploadID + `</UploadId></InitiateMultipartUploadResult>`)
	case s3.CompleteMultipartUpload:
		xml(`<CompleteMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>` + g.route.Bucket + `</Bucket><Key>` + g.route.Key + `</Key><ETag>"` + md5hex(payload) + `-1"</ETag></CompleteMultipartUploadResult>`)
	case s3.ListObjectsV2:
		xml(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>` + g.route.Bucket + `</Name><Prefix></Prefix><Delimiter>/</Delimiter><KeyCount>0</KeyCount><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated></ListBucketResult>`)
	case s3.HeadObject, s3.GetObject:
		h.Set("Content-Type", "text/plain")
		h.Set("Content-Length", strconv.Itoa(len(payload)))
		h.Set("ETag", `"`+md5hex(payload)+`"`)
		h.Set("Last-Modified", time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat))
		if g.req.Header.Get("X-Amz-Checksum-Mode") == "ENABLED" {
			h.Set("x-amz-checksum-crc64nvme", checksum("crc64nvme", []byte(payload)))
			h.Set("x-amz-checksum-type", "FULL_OBJECT")
		}
		io.WriteString(w, payload)
	}
}

// capture runs c and turns every request it sent into a checked test case.
func (s *server) capture(c capture, dir string, env []string) []*testCase {
	s.mu.Lock()
	s.got = nil
	s.mu.Unlock()

	endpoint := s.https
	if c.plain {
		endpoint = s.http
	}
	env = append(slices.Clone(env), "ENDPOINT="+endpoint)
	var name string
	var args []string
	var source string
	switch c.client {
	case "aws-cli":
		name, args = "aws", append(slices.Clone(c.args), "--endpoint-url", endpoint, "--debug")
		source = "aws " + shellJoin(c.args) + " --endpoint-url $ENDPOINT"
	case "boto3":
		name, args = "python", []string{"-c", boto3Prelude + c.args[0]}
		source = c.args[0]
	}
	stdout, logged, err := execute(dir, env, name, args...)
	if c.curl != nil { // the client printed a presigned URL for curl
		if err != nil {
			log.Fatalf("%s: %v\n%s", c.name, err, tail(logged))
		}
		tlsArgs := []string{"--cacert", "ca.pem", "--ssl-revoke-best-effort"}
		args := append(append(tlsArgs, "--silent", "--show-error", "--fail", "--output", os.DevNull), c.curl...)
		var curlErr string
		_, curlErr, err = execute(dir, env, "curl", append(args, strings.TrimSpace(stdout))...)
		logged += curlErr
		source = "curl " + shellJoin(append(tlsArgs, c.curl...)) + ` "$(` + source + `)"`
	}
	if (err != nil) != c.fail {
		log.Fatalf("%s: exit error %v, want failure %v\n%s", c.name, err, c.fail, tail(logged))
	}
	signed := canonicalRequests(logged)

	s.mu.Lock()
	got := s.got
	s.mu.Unlock()
	if len(got) == 0 {
		log.Fatalf("%s: no request captured", c.name)
	}
	rank := map[s3.Operation]int{s3.HeadObject: 1, s3.GetObject: 2, s3.CreateMultipartUpload: 3, s3.UploadPart: 4, s3.CompleteMultipartUpload: 5}
	slices.SortStableFunc(got, func(a, b *captured) int {
		if d := rank[a.route.Op] - rank[b.route.Op]; d != 0 {
			return d
		}
		pa, _ := strconv.Atoi(a.req.URL.Query().Get("partNumber"))
		pb, _ := strconv.Atoi(b.req.URL.Query().Get("partNumber"))
		return pa - pb
	})

	var cases []*testCase
	for _, g := range got {
		name := c.name
		if len(got) > 1 {
			name += "/" + string(g.route.Op)
			if g.route.Op == s3.UploadPart {
				name += "-" + g.req.URL.Query().Get("partNumber")
			}
		}
		cases = append(cases, toCase(c, g, name, source, signed))
	}
	return cases
}

func toCase(c capture, g *captured, name, source string, signed map[string]string) *testCase {
	fatal := func(format string, args ...any) { log.Fatalf(name+": "+format, args...) }
	head, body, _ := bytes.Cut(g.raw, []byte("\r\n\r\n"))
	tc := &testCase{Name: name, Client: c.client, Source: source, Note: c.note, Error: string(c.error), Request: strings.Split(string(head), "\r\n")}
	switch {
	case g.err != nil && g.err.Code != c.error:
		fatal("router error %v, want %q", g.err, c.error)
	case g.err == nil && c.fail:
		fatal("router accepted %s, want %s", g.route.Op, c.error)
	case g.err == nil:
		tc.Op = string(g.route.Op)
	}
	if !utf8.Valid(body) {
		fatal("body is not UTF-8; use a text payload")
	}
	if len(body) > maxBody {
		tc.BodyOmitted = len(body)
	} else {
		tc.Body = string(body)
	}

	q := g.req.URL.Query()
	tc.Now = g.req.Header.Get("X-Amz-Date") + q.Get("X-Amz-Date")
	sig := q.Get("X-Amz-Signature")
	if _, s, ok := strings.Cut(g.req.Header.Get("Authorization"), "Signature="); ok {
		sig = s
	}
	switch tc.CanonicalRequest = signed[sig]; {
	case sig == "" && c.error == "":
		fatal("request has no SigV4 signature")
	case sig != "" && (tc.CanonicalRequest == "" || signature(tc.Now, tc.CanonicalRequest) != sig):
		fatal("no logged canonical request gives signature %q", sig)
	}

	if g.err != nil || len(g.body) == 0 {
		return tc
	}
	data, trailers, err := decodePayload(g.req.Header, g.body)
	if err != nil {
		fatal("%v", err)
	}
	if c.error == "" {
		tc.PayloadSHA256 = sha256hex(string(data))
	}
	if g.route.Op != s3.PutObject && g.route.Op != s3.UploadPart {
		return tc // a Complete checksum covers the object, not the XML body
	}
	values := trailers
	for name, v := range g.req.Header {
		values = append(values, name+":"+v[0])
	}
	for _, line := range values {
		name, v, _ := strings.Cut(line, ":")
		alg, ok := strings.CutPrefix(strings.ToLower(name), "x-amz-checksum-")
		if _, known := checksums[alg]; ok && known && checksum(alg, data) != v {
			fatal("%s %s, payload gives %s", name, v, checksum(alg, data))
		}
	}
	if md := g.req.Header.Get("Content-Md5"); md != "" && c.error == "" && md != b64(md5.Sum(data)) {
		fatal("Content-MD5 %s does not match the payload", md)
	}
	return tc
}

var logRecord = regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d,\d+ - `)

// canonicalRequests maps each signature botocore logged to its canonical
// request. Records of one thread are paired; threads interleave.
func canonicalRequests(stderr string) map[string]string {
	pending := map[string]string{}
	out := map[string]string{}
	for _, rec := range logRecord.Split(strings.ReplaceAll(stderr, "\r\n", "\n"), -1) {
		thread, rest, _ := strings.Cut(rec, " - ")
		if _, msg, ok := strings.Cut(rest, " - DEBUG - CanonicalRequest:\n"); ok {
			pending[thread] = strings.TrimSuffix(msg, "\n")
		} else if _, msg, ok := strings.Cut(rest, " - DEBUG - Signature:\n"); ok {
			out[strings.TrimSpace(msg)] = pending[thread]
		}
	}
	return out
}

// decodePayload returns the logical payload of a body and, for aws-chunked,
// its trailer lines. Chunk signatures are not checked here.
func decodePayload(h http.Header, body []byte) ([]byte, []string, error) {
	if !strings.Contains(h.Get("Content-Encoding"), "aws-chunked") {
		return body, nil, nil
	}
	r := bufio.NewReader(bytes.NewReader(body))
	line := func() (string, error) {
		l, err := r.ReadString('\n')
		if err != nil || !strings.HasSuffix(l, "\r\n") {
			return "", fmt.Errorf("aws-chunked: line %q: %v", l, err)
		}
		return strings.TrimSuffix(l, "\r\n"), nil
	}
	var data []byte
	for {
		l, err := line()
		if err != nil {
			return nil, nil, err
		}
		size, _, _ := strings.Cut(l, ";")
		n, err := strconv.ParseUint(size, 16, 31)
		if err != nil {
			return nil, nil, fmt.Errorf("aws-chunked: chunk size %q", size)
		}
		if n == 0 {
			break
		}
		chunk := make([]byte, n+2)
		if _, err := io.ReadFull(r, chunk); err != nil || string(chunk[n:]) != "\r\n" {
			return nil, nil, fmt.Errorf("aws-chunked: chunk data: %v", err)
		}
		data = append(data, chunk[:n]...)
	}
	var trailers []string
	for {
		l, err := line()
		if err != nil {
			return nil, nil, err
		}
		if l == "" {
			break
		}
		trailers = append(trailers, l)
	}
	if r.Buffered() > 0 {
		return nil, nil, fmt.Errorf("aws-chunked: %d bytes after the trailer", r.Buffered())
	}
	if dl := h.Get("X-Amz-Decoded-Content-Length"); dl != strconv.Itoa(len(data)) {
		return nil, nil, fmt.Errorf("aws-chunked: %d bytes, x-amz-decoded-content-length %s", len(data), dl)
	}
	return data, trailers, nil
}

// checksums are the algorithms of API v2 2.7; values are the base64 of the
// big-endian digest. CRC64NVME is crc64 with the reflected NVMe polynomial.
var checksums = map[string]func() hash.Hash{
	"crc32":     func() hash.Hash { return crc32.NewIEEE() },
	"crc32c":    func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) },
	"crc64nvme": func() hash.Hash { return crc64.New(crc64.MakeTable(0x9a6c9329ac4bc9b5)) },
	"sha1":      sha1.New,
	"sha256":    sha256.New,
}

func checksum(alg string, data []byte) string {
	h := checksums[alg]()
	h.Write(data)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func b64(sum [md5.Size]byte) string { return base64.StdEncoding.EncodeToString(sum[:]) }

func md5hex(s string) string { sum := md5.Sum([]byte(s)); return hex.EncodeToString(sum[:]) }

func sha256hex(s string) string { sum := sha256.Sum256([]byte(s)); return hex.EncodeToString(sum[:]) }

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func scope(amzDate string) string { return amzDate[:8] + "/" + region + "/s3/aws4_request" }

// sign returns the hex HMAC of a string to sign with the signing key of the
// example secret on the day of amzDate.
func sign(amzDate, stringToSign string) string {
	key := []byte("AWS4" + secretAccessKey)
	for _, s := range []string{amzDate[:8], region, "s3", "aws4_request"} {
		key = hmacSHA256(key, s)
	}
	return hex.EncodeToString(hmacSHA256(key, stringToSign))
}

func signature(amzDate, canonicalRequest string) string {
	return sign(amzDate, "AWS4-HMAC-SHA256\n"+amzDate+"\n"+scope(amzDate)+"\n"+sha256hex(canonicalRequest))
}

func chunkSignature(amzDate, previous, data string) string {
	return sign(amzDate, "AWS4-HMAC-SHA256-PAYLOAD\n"+amzDate+"\n"+scope(amzDate)+"\n"+previous+"\n"+emptySHA256+"\n"+sha256hex(data))
}

// awsDocsExamples are the PUT examples of the AWS SigV4 streaming pages, the
// only source of STREAMING-AWS4-HMAC-SHA256-PAYLOAD(-TRAILER): none of the
// pinned clients sends them. Every signature is recomputed before use.
func awsDocsExamples() []*testCase {
	const now = "20130524T000000Z"
	data := strings.Repeat("a", 66560)
	parts := []string{data[:65536], data[65536:], ""}
	build := func(seed string, sigs []string) string {
		var b strings.Builder
		prev := seed
		for i, p := range parts {
			if s := chunkSignature(now, prev, p); s != sigs[i] {
				log.Fatalf("AWS docs chunk %d: signature %s, computed %s", i+1, sigs[i], s)
			}
			fmt.Fprintf(&b, "%x;chunk-signature=%s\r\n%s\r\n", len(p), sigs[i], p)
			prev = sigs[i]
		}
		return strings.TrimSuffix(b.String(), "\r\n")
	}
	header := func(c *testCase, signedHeaders, seed string) {
		if s := signature(now, c.CanonicalRequest); s != seed {
			log.Fatalf("%s: seed signature %s, computed %s", c.Name, seed, s)
		}
		c.Request[4] = "Authorization: AWS4-HMAC-SHA256 Credential=" + accessKeyID + "/" + scope(now) + ",SignedHeaders=" + signedHeaders + ",Signature=" + seed
		if want := "Content-Length: " + strconv.Itoa(len(c.Body)); !slices.Contains(c.Request, want) {
			log.Fatalf("%s: body is %d bytes", c.Name, len(c.Body))
		}
	}
	head := func(mode string, extra ...string) []string {
		return append([]string{"PUT /examplebucket/chunkObject.txt HTTP/1.1", "Host: s3.amazonaws.com", "x-amz-date: " + now,
			"x-amz-storage-class: REDUCED_REDUNDANCY", "Authorization: ", "x-amz-content-sha256: " + mode,
			"Content-Encoding: aws-chunked", "x-amz-decoded-content-length: 66560"}, extra...)
	}

	plain := &testCase{
		Name: "aws-docs-signed-chunked", Client: "aws-docs", Source: "https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html",
		Now: now, Op: string(s3.PutObject), PayloadSHA256: sha256hex(data),
		Request: head("STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "Content-Length: 66824"),
		Body: build("4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9", []string{
			"ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648",
			"0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497",
			"b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9",
		}) + "\r\n",
		CanonicalRequest: "PUT\n/examplebucket/chunkObject.txt\n\ncontent-encoding:aws-chunked\ncontent-length:66824\nhost:s3.amazonaws.com\n" +
			"x-amz-content-sha256:STREAMING-AWS4-HMAC-SHA256-PAYLOAD\nx-amz-date:" + now + "\nx-amz-decoded-content-length:66560\nx-amz-storage-class:REDUCED_REDUNDANCY\n\n" +
			"content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class\nSTREAMING-AWS4-HMAC-SHA256-PAYLOAD",
	}
	header(plain, "content-encoding;content-length;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class",
		"4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9")

	const crc, trailerSig = "sOO8/Q==", "d81f82fc3505edab99d459891051a732e8730629a2e4a59689829ca17fe2e435"
	if got := checksum("crc32c", []byte(data)); got != crc {
		log.Fatalf("AWS docs CRC32C %s, computed %s", crc, got)
	}
	trailerBody := build("106e2a8a18243abcf37539882f36619c00e2dfc72633413f02d3b74544bfeb8e", []string{
		"b474d8862b1487a5145d686f57f013e54db672cee1c953b3010fb58501ef5aa2",
		"1c1344b170168f8e65b41376b44b20fe354e373826ccbbe2c1d40a8cae51e5c7",
		"2ca2aba2005185cf7159c6277faf83795951dd77a3a99e6e65d5c9f85863f992",
	})
	if s := sign(now, "AWS4-HMAC-SHA256-TRAILER\n"+now+"\n"+scope(now)+"\n2ca2aba2005185cf7159c6277faf83795951dd77a3a99e6e65d5c9f85863f992\n"+
		sha256hex("x-amz-checksum-crc32c:"+crc+"\n")); s != trailerSig {
		log.Fatalf("AWS docs trailer signature %s, computed %s", trailerSig, s)
	}
	trailer := &testCase{
		Name: "aws-docs-signed-chunked-trailer", Client: "aws-docs", Source: "https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming-trailers.html",
		Note: "the Authorization header on that page lists content-length and omits x-amz-trailer; the seed signature is computed without content-length, so SignedHeaders follows the canonical request",
		Now:  now, Op: string(s3.PutObject), PayloadSHA256: sha256hex(data),
		Request: head("STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER", "x-amz-trailer: x-amz-checksum-crc32c", "Content-Length: 66946"),
		Body:    trailerBody + "x-amz-checksum-crc32c:" + crc + "\r\nx-amz-trailer-signature:" + trailerSig + "\r\n\r\n",
		CanonicalRequest: "PUT\n/examplebucket/chunkObject.txt\n\ncontent-encoding:aws-chunked\nhost:s3.amazonaws.com\n" +
			"x-amz-content-sha256:STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER\nx-amz-date:" + now + "\nx-amz-decoded-content-length:66560\nx-amz-storage-class:REDUCED_REDUNDANCY\nx-amz-trailer:x-amz-checksum-crc32c\n\n" +
			"content-encoding;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class;x-amz-trailer\nSTREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER",
	}
	header(trailer, "content-encoding;host;x-amz-content-sha256;x-amz-date;x-amz-decoded-content-length;x-amz-storage-class;x-amz-trailer",
		"106e2a8a18243abcf37539882f36619c00e2dfc72633413f02d3b74544bfeb8e")
	return []*testCase{plain, trailer}
}

// variants derives requests with exactly one fault from the captured cases.
func variants(cases []*testCase) []*testCase {
	base := func(name string) *testCase {
		for _, c := range cases {
			if c.Name == name {
				return c
			}
		}
		log.Fatalf("variants: no case %s", name)
		return nil
	}
	var out []*testCase
	derive := func(from *testCase, name string, code s3.ErrorCode, note string) *testCase {
		v := *from
		v.Name, v.Client, v.Source, v.From, v.Note, v.Error, v.CanonicalRequest = name, "variant", "", from.Name, note, string(code), ""
		v.Request = slices.Clone(from.Request)
		if code != "" {
			v.PayloadSHA256 = ""
		}
		out = append(out, &v)
		return &v
	}

	put := base("cli-put-default") // STREAMING-UNSIGNED-PAYLOAD-TRAILER, CRC64NVME
	derive(put, "sigv4-signature-changed", s3.ErrSignatureDoesNotMatch, "last hex digit of Signature changed").
		editHeader("Authorization", func(v string) string { return v[:len(v)-1] + flip(v[len(v)-1:]) })
	derive(put, "sigv4-unknown-access-key", s3.ErrInvalidAccessKeyID, "access key ID not in Credentials").
		editHeader("Authorization", func(v string) string { return mustReplace(v, accessKeyID, "AKIAIOSFODNN7UNKNOWN") })
	derive(put, "sigv4-host-rewritten", s3.ErrSignatureDoesNotMatch, "a proxy that rewrites Host breaks the signature (API v2 2.1)").
		editHeader("Host", func(string) string { return "gateway.internal" })
	derive(put, "sigv4-unsigned-amz-header", s3.ErrAccessDenied, "x-amz-* header outside SignedHeaders").
		addHeader("x-amz-meta-unsigned: 1")
	derive(put, "sigv4-unsigned-debug-header", "", "x-dsg-debug outside SignedHeaders: the request is valid, the debug opt-in is ignored (API v2 5)").
		addHeader("x-dsg-debug: 1")
	derive(put, "sigv4-skew-15m", "", "server clock 15 minutes after X-Amz-Date: still accepted").Now = shift(put.Now, 15*time.Minute)
	derive(put, "sigv4-skew-over-15m", s3.ErrRequestTimeTooSkewed, "server clock 15 minutes 1 second after X-Amz-Date").Now = shift(put.Now, 15*time.Minute+time.Second)
	derive(put, "sigv4-skew-over-15m-future", s3.ErrRequestTimeTooSkewed, "server clock 15 minutes 1 second before X-Amz-Date").Now = shift(put.Now, -15*time.Minute-time.Second)

	crc := checksum("crc64nvme", []byte(payload))
	chunk := fmt.Sprintf("%x\r\n%s\r\n", len(payload), payload)
	derive(put, "chunked-payload-changed", s3.ErrBadDigest, "one payload byte changed; the CRC64NVME trailer no longer matches").
		editChunked(func(b string) string { return mustReplace(b, "Miles", "Niles") })
	derive(put, "chunked-trailer-checksum-wrong", s3.ErrBadDigest, "trailer carries the CRC64NVME of other data").
		editChunked(func(b string) string { return mustReplace(b, crc, checksum("crc64nvme", []byte("x"))) })
	derive(put, "chunked-trailer-missing", s3.ErrInvalidRequest, "x-amz-trailer names a trailer the body does not carry").
		editChunked(func(b string) string { return mustReplace(b, "x-amz-checksum-crc64nvme:"+crc+"\r\n", "") })
	derive(put, "chunked-trailer-other-algorithm", s3.ErrInvalidRequest, "trailer is x-amz-checksum-crc32, x-amz-trailer names crc64nvme").
		editChunked(func(b string) string {
			return mustReplace(b, "x-amz-checksum-crc64nvme:"+crc, "x-amz-checksum-crc32:"+checksum("crc32", []byte(payload)))
		})
	derive(put, "chunked-decoded-length-short", s3.ErrIncompleteBody, "chunks carry one byte less than x-amz-decoded-content-length").
		editChunked(func(b string) string {
			return mustReplace(b, chunk, fmt.Sprintf("%x\r\n%s\r\n", len(payload)-1, payload[1:]))
		})
	derive(put, "chunked-size-not-hex", s3.ErrInvalidRequest, "chunk size line is not hexadecimal").
		editChunked(func(b string) string { return mustReplace(b, chunk, "2g\r\n"+payload+"\r\n") })
	inner := put.awsChunked()
	derive(put, "chunked-truncated", s3.ErrIncompleteBody, "connection closes inside the chunk data").
		Body = fmt.Sprintf("%x\r\n%s", len(inner), inner[:strings.Index(inner, "payload")])

	plain := base("cli-put-http") // hex payload SHA-256 + CRC64NVME header
	derive(plain, "payload-sha256-mismatch", s3.ErrXAmzContentSHA256Mismatch, "one body byte changed; x-amz-content-sha256 is checked before the checksum").
		Body = mustReplace(plain.Body, "Miles", "Niles")
	derive(plain, "payload-incomplete", s3.ErrIncompleteBody, "body 8 bytes shorter than Content-Length").
		Body = plain.Body[:len(plain.Body)-8]

	presign := base("cli-presign-get")
	expires := shift(presign.Now, 900*time.Second)
	derive(presign, "presign-at-expiry", "", "server clock at X-Amz-Date + X-Amz-Expires: still valid").Now = expires
	derive(presign, "presign-expired", s3.ErrAccessDenied, "server clock 1 second after X-Amz-Date + X-Amz-Expires").Now = shift(expires, time.Second)
	derive(presign, "presign-key-changed", s3.ErrSignatureDoesNotMatch, "URL reused for another key").
		Request[0] = mustReplace(presign.Request[0], "/demo/small.txt?", "/demo/other.txt?")

	docs := base("aws-docs-signed-chunked")
	derive(docs, "signed-chunk-signature-changed", s3.ErrSignatureDoesNotMatch, "last hex digit of the second chunk-signature changed").
		Body = mustReplace(docs.Body, "a503ef5497\r\n", "a503ef5490\r\n")
	derive(docs, "signed-chunk-data-changed", s3.ErrSignatureDoesNotMatch, "one byte of the first chunk changed").
		Body = mustReplace(docs.Body, "\r\naaaa", "\r\nbaaa")
	trailer := base("aws-docs-signed-chunked-trailer")
	derive(trailer, "signed-trailer-signature-changed", s3.ErrSignatureDoesNotMatch, "last hex digit of x-amz-trailer-signature changed").
		Body = mustReplace(trailer.Body, "7fe2e435\r\n", "7fe2e430\r\n")
	derive(trailer, "signed-trailer-checksum-changed", s3.ErrSignatureDoesNotMatch, "trailer CRC32C changed; the trailer signature fails before the checksum").
		Body = mustReplace(trailer.Body, "sOO8/Q==", checksum("crc32c", []byte("x")))
	return out
}

func (c *testCase) editHeader(name string, edit func(string) string) {
	for i, l := range c.Request {
		if n, v, ok := strings.Cut(l, ": "); ok && strings.EqualFold(n, name) {
			c.Request[i] = n + ": " + edit(v)
			return
		}
	}
	log.Fatalf("%s: no %s header", c.Name, name)
}

func (c *testCase) addHeader(line string) { c.Request = append(c.Request, line) }

// awsChunked returns the aws-chunked body under the HTTP chunked framing.
func (c *testCase) awsChunked() string {
	b, err := io.ReadAll(httputil.NewChunkedReader(strings.NewReader(c.Body)))
	check(err)
	return string(b)
}

// editChunked edits the aws-chunked body and sends it as one HTTP chunk.
func (c *testCase) editChunked(edit func(string) string) {
	e := edit(c.awsChunked())
	c.Body = fmt.Sprintf("%x\r\n%s\r\n0\r\n\r\n", len(e), e)
}

func mustReplace(s, old, new string) string {
	if !strings.Contains(s, old) {
		log.Fatalf("variants: %q not found", old)
	}
	return strings.Replace(s, old, new, 1)
}

func flip(hexDigit string) string {
	if hexDigit == "0" {
		return "1"
	}
	return "0"
}

func shift(amzDate string, d time.Duration) string {
	t, err := time.Parse(amzDateLayout, amzDate)
	check(err)
	return t.Add(d).Format(amzDateLayout)
}

var plainArg = regexp.MustCompile(`^[A-Za-z0-9_./:=@,-]+$`)

func shellJoin(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if !plainArg.MatchString(a) {
			a = "'" + a + "'"
		}
		quoted[i] = a
	}
	return strings.Join(quoted, " ")
}

func tail(s string) string {
	if len(s) > 4000 {
		return s[len(s)-4000:]
	}
	return s
}

// writeFixture writes one case per line, like the other fixtures.
func writeFixture(path string, f fixture) {
	enc := func(v any, indent string) string {
		var b bytes.Buffer
		e := json.NewEncoder(&b)
		e.SetEscapeHTML(false)
		e.SetIndent("", indent)
		check(e.Encode(v))
		return strings.TrimSuffix(b.String(), "\n")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "{\n\"clients\": %s,\n\"config\": %s,\n\"credentials\": %s,\n\"cases\": [\n", enc(f.Clients, "  "), enc(f.Config, "  "), enc(f.Credentials, "  "))
	for i, c := range f.Cases {
		sep := ","
		if i == len(f.Cases)-1 {
			sep = ""
		}
		fmt.Fprintf(&b, "  %s%s\n", enc(c, ""), sep)
	}
	b.WriteString("]\n}\n")
	check(os.WriteFile(path, []byte(b.String()), 0o644))
}
