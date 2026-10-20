package s3

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

// transport.json: requests of the pinned clients and their single-fault
// variants (scripts/s3capture), with the server clock to verify them at.
type transportFixture struct {
	Credentials struct {
		AccessKeyID     string `json:"access_key_id"`
		Region          string
		SecretAccessKey string `json:"secret_access_key"`
	}
	Cases []transportCase
}

type transportCase struct {
	Name          string
	Now           string
	Op            Operation
	Error         ErrorCode
	Request       []string
	Body          string
	BodyOmitted   int    `json:"body_omitted"`
	PayloadSHA256 string `json:"payload_sha256"`
}

func loadTransport(t *testing.T) transportFixture {
	var f transportFixture
	readFixture(t, "transport.json", &f)
	return f
}

func (f transportFixture) find(t *testing.T, name string) transportCase {
	for _, c := range f.Cases {
		if c.Name == name {
			c.Request = slices.Clone(c.Request)
			return c
		}
	}
	t.Fatalf("transport.json has no case %s", name)
	return transportCase{}
}

// router authenticates the fixture access key as cred.
func (f transportFixture) router(cred Credential) Router {
	cred.Secret = f.Credentials.SecretAccessKey
	return Router{Region: f.Credentials.Region, Credentials: credentialMap{f.Credentials.AccessKeyID: cred}}
}

var activeUser = Credential{UserID: "demo", Active: true}

type credentialMap map[string]Credential

func (m credentialMap) Credential(_ context.Context, accessKeyID string) (Credential, error) {
	c, ok := m[accessKeyID]
	if !ok {
		return Credential{}, storage.ErrNotFound
	}
	return c, nil
}

type unavailableCredentials struct{}

func (unavailableCredentials) Credential(context.Context, string) (Credential, error) {
	return Credential{}, storage.ErrUnavailable
}

// served is what an operation handler received from the router.
type served struct {
	op        Operation
	caller    Caller
	payload   []byte
	algorithm string
	checksum  []byte
}

// request parses the raw request of c.
func (c transportCase) request(t *testing.T) *http.Request {
	t.Helper()
	raw := strings.Join(c.Request, "\r\n") + "\r\n\r\n" + c.Body
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw)))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// serve sends the raw request of c through rt at the clock c.Now. Unless rt
// has handlers, every operation gets one that reads the whole payload. It
// returns the response and what the handler received, nil when the router
// answered alone.
func serve(t *testing.T, rt Router, c transportCase) (*httptest.ResponseRecorder, *served) {
	t.Helper()
	now, _ := time.Parse(amzDateLayout, c.Now) // empty for requests the router rejects first
	rt.Now = func() time.Time { return now }
	var got *served
	if rt.Handlers == nil {
		rt.Handlers = map[Operation]Handler{}
		for _, row := range routes {
			rt.Handlers[row.op] = func(w http.ResponseWriter, r *Request) error {
				data, err := io.ReadAll(r.Body)
				got = &served{op: r.Op, caller: r.Caller, payload: data}
				got.algorithm, got.checksum = r.Body.Checksum()
				return err
			}
		}
	}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, c.request(t))
	return rec, got
}

func errorCode(rec *httptest.ResponseRecorder) ErrorCode {
	var body struct{ Code ErrorCode }
	xml.Unmarshal(rec.Body.Bytes(), &body)
	return body.Code
}

// Each fixture request resolves to op, then the router accepts it, the
// handler reading payload_sha256 as logical payload, or answers error. Errors
// that need the signature or body come after Resolve, which still gives op.
// A case whose body is omitted (5 MiB) is checked up to the handler.
func TestTransportFixtures(t *testing.T) {
	f := loadTransport(t)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if _, ok := errorCodes[c.Error]; c.Error != "" && !ok {
				t.Fatalf("error %s is not in API v2 2.4", c.Error)
			}
			if route, e := Resolve(c.request(t)); c.Op != "" && (e != nil || route.Op != c.Op) {
				t.Errorf("Resolve: %s %v, want op %s", route.Op, e, c.Op)
			}
			rec, got := serve(t, f.router(activeUser), c)
			code := errorCode(rec)
			switch {
			case c.BodyOmitted > 0:
				if got == nil || got.op != c.Op {
					t.Errorf("handler got %+v, want op %s (response %d %s)", got, c.Op, rec.Code, code)
				}
			case c.Error != "":
				if code != c.Error {
					t.Errorf("response %d %s, want %s", rec.Code, code, c.Error)
				}
				if got != nil && got.op != c.Op {
					t.Errorf("handler op %s, want %s", got.op, c.Op)
				}
			case rec.Code != http.StatusOK || got == nil:
				t.Fatalf("response %d %s %q, want %s accepted", rec.Code, code, rec.Body, c.Op)
			default:
				if got.op != c.Op {
					t.Errorf("handler op %s, want %s", got.op, c.Op)
				}
				if sum := sha256.Sum256(got.payload); c.PayloadSHA256 != "" && hex.EncodeToString(sum[:]) != c.PayloadSHA256 {
					t.Errorf("payload SHA-256 %x, want %s", sum, c.PayloadSHA256)
				}
				if len(rec.Header().Get("X-Amz-Request-Id")) != 16 {
					t.Errorf("X-Amz-Request-Id %q", rec.Header().Get("X-Amz-Request-Id"))
				}
			}
		})
	}
}

// The payload checksum is computed on the logical bytes: the algorithm the
// client names, or CRC64NVME when it names none (API v2 2.7).
func TestPayloadChecksum(t *testing.T) {
	f := loadTransport(t)
	sha := sha1.Sum([]byte("Miles transport fixture payload\n"))
	for _, tc := range []struct {
		name, algorithm string
		digest          []byte
	}{
		{"cli-put-when-required", "CRC64NVME", must(base64.StdEncoding.DecodeString("Y53hf1fT3M4="))},
		{"cli-put-sha1", "SHA1", sha[:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := serve(t, f.router(activeUser), f.find(t, tc.name))
			if got == nil || got.algorithm != tc.algorithm || string(got.checksum) != string(tc.digest) {
				t.Errorf("handler got %+v, want %s %x", got, tc.algorithm, tc.digest)
			}
		})
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Requests broken before the body: auth format, missing or unsupported
// signing algorithm, credential state and the rank of router errors.
func TestAuthenticationErrors(t *testing.T) {
	f := loadTransport(t)
	authorization := func(edit func(string) string) func(*transportCase) {
		return func(c *transportCase) {
			for i, l := range c.Request {
				if v, ok := strings.CutPrefix(l, "Authorization: "); ok {
					c.Request[i] = "Authorization: " + edit(v)
				}
			}
		}
	}
	target := func(old, new string) func(*transportCase) {
		return func(c *transportCase) { c.Request[0] = strings.Replace(c.Request[0], old, new, 1) }
	}
	header := func(name, value string) func(*transportCase) {
		return func(c *transportCase) { c.Request = withHeader(c.Request, name, value) }
	}
	for _, tc := range []struct {
		name, from string
		edit       func(*transportCase)
		creds      Credentials
		want       ErrorCode
	}{
		{"no signature", "cli-put-default", header("Authorization", ""), nil, ErrAccessDenied},
		{"SigV2 header", "cli-get/GetObject", header("Authorization", "AWS AKIAIOSFODNN7EXAMPLE:frJIUN8DYpKDtOLCwo//yllqDzg="), nil, ErrNotImplemented},
		{"unsupported signing algorithm", "cli-put-default",
			authorization(func(v string) string { return strings.Replace(v, "AWS4-HMAC-SHA256", "AWS4-ECDSA-P256-SHA256", 1) }), nil, ErrInvalidArgument},
		{"Authorization without SignedHeaders", "cli-get/GetObject",
			authorization(func(v string) string { return strings.Replace(v, "SignedHeaders=", "Headers=", 1) }), nil, ErrInvalidArgument},
		{"scope of another region", "cli-put-default",
			authorization(func(v string) string { return strings.Replace(v, "/us-east-1/", "/eu-west-1/", 1) }), nil, ErrInvalidArgument},
		{"scope date is not X-Amz-Date", "cli-put-default",
			authorization(func(v string) string { return strings.Replace(v, "/20261018/", "/20261017/", 1) }), nil, ErrInvalidArgument},
		{"X-Amz-Date missing", "cli-get/GetObject", header("X-Amz-Date", ""), nil, ErrInvalidArgument},
		{"x-amz-content-sha256 missing", "cli-get/GetObject", header("X-Amz-Content-SHA256", ""), nil, ErrInvalidArgument},
		{"x-amz-content-sha256 unsupported", "cli-get/GetObject", header("X-Amz-Content-SHA256", "STREAMING-AWS4-ECDSA-P256-SHA256-PAYLOAD"), nil, ErrInvalidArgument},
		{"revoked credential", "cli-put-default", nil, credentialMap{"AKIAIOSFODNN7EXAMPLE": {Secret: f.Credentials.SecretAccessKey}}, ErrInvalidAccessKeyID},
		{"credential backend down", "cli-put-default", nil, unavailableCredentials{}, ErrInternalError},
		{"presigned without X-Amz-Algorithm", "cli-presign-get", target("X-Amz-Algorithm=AWS4-HMAC-SHA256&", ""), nil, ErrInvalidArgument},
		{"presigned with SigV4a", "cli-presign-get", target("X-Amz-Algorithm=AWS4-HMAC-SHA256", "X-Amz-Algorithm=AWS4-ECDSA-P256-SHA256"), nil, ErrInvalidArgument},
		{"presigned X-Amz-Expires 0", "cli-presign-get", target("X-Amz-Expires=900", "X-Amz-Expires=0"), nil, ErrInvalidArgument},
		{"presigned and Authorization", "cli-presign-get", header("Authorization", "AWS4-HMAC-SHA256 Credential=x"), nil, ErrInvalidArgument},
		{"presigned 15m1s in the future", "cli-presign-get", func(c *transportCase) { c.Now = "20261018T154046Z" }, nil, ErrRequestTimeTooSkewed},
		{"malformed query before auth", "cli-list", func(c *transportCase) {
			c.Request = withHeader(c.Request, "Authorization", "")
			c.Request[0] = strings.Replace(c.Request[0], "prefix=", "prefix=%zz", 1)
		}, nil, ErrInvalidArgument},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := f.find(t, tc.from)
			if tc.edit != nil {
				tc.edit(&c)
			}
			rt := f.router(activeUser)
			if tc.creds != nil {
				rt.Credentials = tc.creds
			}
			rec, got := serve(t, rt, c)
			if code := errorCode(rec); code != tc.want || got != nil {
				t.Errorf("response %d %s, handler %+v; want %s", rec.Code, code, got, tc.want)
			}
		})
	}
}

// withHeader returns request lines with header name set to value, or
// removed when value is empty.
func withHeader(lines []string, name, value string) []string {
	out := slices.DeleteFunc(slices.Clone(lines), func(l string) bool {
		n, _, ok := strings.Cut(l, ": ")
		return ok && strings.EqualFold(n, name)
	})
	if value != "" {
		out = append(out, name+": "+value)
	}
	return out
}

// x-dsg-* responses need x-dsg-debug: 1 signed and a credential with
// system_admin and debug_allowed (API v2 5); otherwise the request still
// succeeds without debug.
func TestDebugGate(t *testing.T) {
	f := loadTransport(t)
	for _, tc := range []struct {
		name, from         string
		admin, allowed, on bool
	}{
		{"signed, admin with debug", "boto3-put-debug-signed", true, true, true},
		{"signed, admin without debug_allowed", "boto3-put-debug-signed", true, false, false},
		{"signed, debug_allowed without admin", "boto3-put-debug-signed", false, true, false},
		{"unsigned, admin with debug", "sigv4-unsigned-debug-header", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := serve(t, f.router(Credential{UserID: "root", Active: true, SystemAdmin: tc.admin, DebugAllowed: tc.allowed}), f.find(t, tc.from))
			if got == nil || got.caller.Debug != tc.on {
				t.Errorf("handler got %+v, want Debug %v", got, tc.on)
			}
		})
	}
}

func TestOperationWithoutHandler(t *testing.T) {
	f := loadTransport(t)
	rt := f.router(activeUser)
	rt.Handlers = map[Operation]Handler{}
	rec, _ := serve(t, rt, f.find(t, "cli-get/GetObject"))
	if code := errorCode(rec); rec.Code != http.StatusNotImplemented || code != ErrNotImplemented {
		t.Errorf("response %d %s, want 501 NotImplemented", rec.Code, code)
	}
}

// chunkedPayload is an unsigned aws-chunked PutObject body of decoded length
// 4 with a CRC32 trailer; headers override or, when empty, remove its headers.
func chunkedPayload(body string, headers map[string]string) *Payload {
	r := httptest.NewRequest(http.MethodPut, "/demo/k", strings.NewReader(body))
	for k, v := range map[string]string{
		"X-Amz-Content-Sha256":         streamingUnsignedTrailer,
		"Content-Encoding":             "aws-chunked",
		"X-Amz-Decoded-Content-Length": "4",
		"X-Amz-Trailer":                "x-amz-checksum-crc32",
	} {
		r.Header.Set(k, v)
	}
	for k, v := range headers {
		if r.Header.Del(k); v != "" {
			r.Header.Set(k, v)
		}
	}
	return newPayload(r, PutObject, nil)
}

func codeOf(err error) ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Framing faults beyond the fixture variants; the CRC32 of "demo" is 1kLfoA==.
func TestAWSChunkedFraming(t *testing.T) {
	const trailer = "0\r\nx-amz-checksum-crc32:1kLfoA==\r\n\r\n"
	for _, tc := range []struct {
		name, body string
		headers    map[string]string
		want       ErrorCode
	}{
		{"valid", "4\r\ndemo\r\n" + trailer, nil, ""},
		{"two chunks", "1\r\nd\r\n3\r\nemo\r\n" + trailer, nil, ""},
		{"unsigned chunk with signature", "4;chunk-signature=" + emptySHA256 + "\r\ndemo\r\n" + trailer, nil, ErrInvalidRequest},
		{"data without CRLF", "4\r\ndemoX\r\n" + trailer, nil, ErrInvalidRequest},
		{"line without CR", "4\ndemo\r\n" + trailer, nil, ErrInvalidRequest},
		{"data over decoded length", "5\r\ndemo!\r\n" + trailer, nil, ErrInvalidRequest},
		{"two checksum trailers", "4\r\ndemo\r\n0\r\nx-amz-checksum-crc32:1kLfoA==\r\nx-amz-checksum-crc32:1kLfoA==\r\n\r\n", nil, ErrInvalidRequest},
		{"bytes after trailer", "4\r\ndemo\r\n" + trailer + "x", nil, ErrInvalidRequest},
		{"EOF in trailer", "4\r\ndemo\r\n0\r\nx-amz-checksum-crc32:1kLfoA==\r\n", nil, ErrIncompleteBody},
		{"Content-Encoding without aws-chunked", "4\r\ndemo\r\n" + trailer, map[string]string{"Content-Encoding": "gzip"}, ErrInvalidRequest},
		{"aws-chunked among encodings", "4\r\ndemo\r\n" + trailer, map[string]string{"Content-Encoding": "gzip, aws-chunked"}, ""},
		{"no decoded length", "4\r\ndemo\r\n" + trailer, map[string]string{"X-Amz-Decoded-Content-Length": ""}, ErrInvalidRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := chunkedPayload(tc.body, tc.headers)
			data, err := io.ReadAll(p)
			if code := codeOf(err); code != tc.want || err != nil && code == "" {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if alg, sum := p.Checksum(); tc.want == "" && (string(data) != "demo" || alg != "CRC32" || base64.StdEncoding.EncodeToString(sum) != "1kLfoA==") {
				t.Errorf("payload %q, checksum %s %x", data, alg, sum)
			}
		})
	}
}

// DecodeXML reads the whole body first, so payload errors rank before
// MalformedXML.
func TestDecodeXML(t *testing.T) {
	const valid = `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber></Part></CompleteMultipartUpload>`
	for _, tc := range []struct {
		name, body, contentMD5 string
		want                   ErrorCode
	}{
		{"valid", valid, "", ""},
		{"not XML", `{"Parts":[]}`, "", ErrMalformedXML},
		{"empty", "", "", ErrMalformedXML},
		{"not XML with wrong Content-MD5", `{"Parts":[]}`, "1B2M2Y8AsgTpgAmY7PhCfg==", ErrBadDigest},
		{"over the limit", valid + strings.Repeat(" ", 200), "", ErrEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/demo/k?uploadId=U", strings.NewReader(tc.body))
			r.Header.Set("X-Amz-Content-Sha256", unsignedPayload)
			if tc.contentMD5 != "" {
				r.Header.Set("Content-MD5", tc.contentMD5)
			}
			var doc struct {
				Parts []struct{ PartNumber int } `xml:"Part"`
			}
			err := newPayload(r, CompleteMultipartUpload, nil).DecodeXML(&doc, 200)
			if code := codeOf(err); code != tc.want || err != nil && code == "" {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
			if tc.want == "" && (len(doc.Parts) != 1 || doc.Parts[0].PartNumber != 1) {
				t.Errorf("decoded %+v", doc)
			}
		})
	}
}
