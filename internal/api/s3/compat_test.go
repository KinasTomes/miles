package s3

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// compat.json is the compatibility matrix the API freezes on (API v2 11,
// HTTP row): Unicode/path encoding keys, empty PUT, Range open/suffix/416,
// HEAD without body and the XML body parser. The rows the parser owns run
// through the real Resolve, Payload and WriteError; the rows the W03/W04
// handlers own (Range resolution, HEAD response headers, GET of an empty
// object) run through the reference functions at the end of this file,
// written from API v2 4.1, 4.2 and 2.7. Handler tests must pass the same
// fixture rows and then replace those functions, as authorization.json and
// quota.json did in contract_test.go.
//
// Every row that expects an S3 error also pins its HTTP status; the test
// checks it against the API v2 2.4 table (errorCodes), so fixture, code and
// document cannot drift apart before the freeze.

type compatKeyRow struct {
	Name            string
	Request         []string
	Op              Operation
	Error           ErrorCode
	Status          int
	Bucket          string
	Key             string
	KeyHex          string `json:"key_hex"`
	Query           map[string]string
	ErrorBodyKey    string `json:"error_body_key"`
	ErrorBodyKeyHex string `json:"error_body_key_hex"`
	Note            string
}

type compatEmptyRow struct {
	Name    string
	From    string // a case of transport.json
	Request *struct {
		Method, Path, Body, ContentSHA256 string
	}
	PayloadSHA256 string `json:"payload_sha256"`
	EtagMD5       string `json:"etag_md5"`
	// Checksum pins algorithm and Base64 digest together: "CRC64NVME AAAAAAAAAAA=".
	Checksum      string
	Status        int
	ContentLength int64 `json:"content_length"`
	Range         string
	Error         ErrorCode
	Note          string
}

type compatRangeRow struct {
	Name         string
	Size         int64
	Range        string
	Status       int
	Start        int64
	Length       int64
	ContentRange string `json:"content_range"`
	Error        ErrorCode
	Note         string
}

type compatHeadRow struct {
	Name           string
	Request        []string
	Size           int64
	RequestHeaders map[string]string `json:"request_headers"`
	Status         int
	Headers        map[string]string
	CommonHeaders  []string `json:"common_headers"`
	NoChecksum     bool     `json:"no_checksum"`
	NoBody         bool     `json:"no_body"`
	Error          ErrorCode
	Note           string
}

type compatXMLRow struct {
	Name    string
	Op      Operation
	Body    string
	Limit   int64
	Parts   int
	Objects int
	Error   ErrorCode
	Status  int
	Note    string
}

type compatFixture struct {
	Sections map[string]string `json:"sections"`
	Keys     []compatKeyRow
	Empty    []compatEmptyRow
	Range    []compatRangeRow
	Head     []compatHeadRow
	XML      []compatXMLRow
}

func loadCompat(t testing.TB) compatFixture {
	t.Helper()
	var f compatFixture
	readFixture(t, "compat.json", &f)
	if len(f.Sections) != 5 || len(f.Keys) == 0 || len(f.Empty) == 0 || len(f.Range) == 0 || len(f.Head) == 0 || len(f.XML) == 0 {
		t.Fatal("compat.json must cite an API v2 section and hold rows for keys, empty, range, head and xml")
	}
	return f
}

// readFixtureRequest parses the raw request lines of a row.
func readFixtureRequest(t *testing.T, lines []string) *http.Request {
	t.Helper()
	req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(strings.Join(lines, "\r\n") + "\r\n\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestKeyFixtures runs the key rows through Resolve, and the error echo rows
// through WriteError: the key is decoded exactly once and XML-escaped, never
// re-encoded, and a byte that is not valid UTF-8 becomes U+FFFD in the error
// body (API v2 2.4).
func TestKeyFixtures(t *testing.T) {
	for _, row := range loadCompat(t).Keys {
		t.Run(row.Name, func(t *testing.T) {
			checkStatusPin(t, row.Error, row.Status)
			req := readFixtureRequest(t, row.Request)
			route, e := Resolve(req)
			wantKey := row.Key
			if row.KeyHex != "" {
				wantKey = string(must(hex.DecodeString(row.KeyHex)))
			}
			gotKey := route.Key
			gotBucket := route.Bucket
			if e != nil {
				gotKey, gotBucket = e.Key, e.Bucket
			}
			if route.Op != row.Op || e != nil != (row.Error != "") || (e != nil && e.Code != row.Error) {
				t.Errorf("Resolve: %s %v, want op %s error %q", route.Op, e, row.Op, row.Error)
			}
			if gotBucket != row.Bucket || gotKey != wantKey {
				t.Errorf("bucket/key: %q %q, want %q %q", route.Bucket, gotKey, row.Bucket, wantKey)
			}
			for name, want := range row.Query {
				if got := route.Query.Get(name); got != want {
					t.Errorf("query %s = %q, want %q", name, got, want)
				}
			}
			if row.ErrorBodyKey == "" && row.ErrorBodyKeyHex == "" {
				return
			}
			rec := httptest.NewRecorder()
			if err := WriteError(rec, req, e, "COMPATFIXTURE00"); err != nil {
				t.Fatal(err)
			}
			body := rec.Body.String()
			open, end := "<Key>", "</Key>"
			i, j := strings.Index(body, open), strings.Index(body, end)
			if i < 0 || j < i {
				t.Fatalf("error body has no Key element: %q", body)
			}
			want := row.ErrorBodyKey
			if row.ErrorBodyKeyHex != "" {
				want = string(must(hex.DecodeString(row.ErrorBodyKeyHex)))
			}
			if got := body[i+len(open) : j]; got != want {
				t.Errorf("error body Key %q, want %q", got, want)
			}
		})
	}
}

// servedPut is what the handler of an executed empty-PUT row received.
type servedPut struct {
	op        Operation
	payload   []byte
	algorithm string
	checksum  []byte
	md5       []byte
}

// TestEmptyFixtures pins the empty-object contract: the pinned CLI's empty
// PUT (aws-chunked) and a plain Content-Length 0 PUT both decode to zero
// bytes, store CRC64NVME of the empty string and end with the empty MD5
// ETag (API v2 2.7); GET of an empty object is 200 length 0 and every Range
// on it is 416 (4.1). The GET rows are the handler contract, checked against
// parseRange below.
func TestEmptyFixtures(t *testing.T) {
	f := loadCompat(t)
	for _, row := range f.Empty {
		t.Run(row.Name, func(t *testing.T) {
			checkStatusPin(t, row.Error, row.Status)
			alg, digest, _ := strings.Cut(row.Checksum, " ")
			switch {
			case row.From != "":
				tf := loadTransport(t)
				rec, got := serve(t, tf.router(activeUser), tf.find(t, row.From))
				if got == nil || rec.Code != http.StatusOK {
					t.Fatalf("response %d, handler got %+v", rec.Code, got)
				}
				checkEmptyPut(t, got.op, got.payload, got.algorithm, got.checksum, alg, digest, row.PayloadSHA256)
			case row.Request != nil:
				tf := loadTransport(t)
				req := httptest.NewRequest(row.Request.Method, row.Request.Path, strings.NewReader(row.Request.Body))
				req.Header.Set("X-Amz-Content-Sha256", row.Request.ContentSHA256)
				sign(req, tf.Credentials.AccessKeyID, tf.Credentials.SecretAccessKey)
				got := serveSignedPut(t, tf, req)
				checkEmptyPut(t, got.op, got.payload, got.algorithm, got.checksum, alg, digest, row.PayloadSHA256)
				if base64.StdEncoding.EncodeToString(got.md5) != row.EtagMD5 {
					t.Errorf("payload MD5 %s, want %s", base64.StdEncoding.EncodeToString(got.md5), row.EtagMD5)
				}
			case row.Range != "":
				if _, _, ok := parseRange(row.Range, 0); ok {
					t.Errorf("parseRange(%q, 0) satisfiable, want 416", row.Range)
				}
			case row.Status == http.StatusOK:
				if row.ContentLength != 0 {
					t.Errorf("GET of an empty object pins content length 0, fixture says %d", row.ContentLength)
				}
			default:
				t.Errorf("empty row %s pins nothing", row.Name)
			}
		})
	}
}

func checkEmptyPut(t *testing.T, op Operation, payload []byte, algorithm string, checksum []byte, wantAlg, wantDigest, wantSHA string) {
	t.Helper()
	if op != PutObject {
		t.Errorf("op %s, want PutObject", op)
	}
	if len(payload) != 0 {
		t.Errorf("payload %q, want empty", payload)
	}
	if sum := sha256.Sum256(nil); hex.EncodeToString(sum[:]) != wantSHA {
		t.Errorf("payload SHA-256 %x, want %s", sum, wantSHA)
	}
	if algorithm != wantAlg || base64.StdEncoding.EncodeToString(checksum) != wantDigest {
		t.Errorf("checksum %s %s, want %s %s", algorithm, base64.StdEncoding.EncodeToString(checksum), wantAlg, wantDigest)
	}
}

// serveSignedPut sends a hand-built, signed request through the fixture
// router at the auth clock, with a handler that reads the payload like the
// W03 one will.
func serveSignedPut(t *testing.T, f transportFixture, r *http.Request) *servedPut {
	t.Helper()
	rt := f.router(activeUser)
	rt.Now = func() time.Time { return authNow }
	var got *servedPut
	rt.Handlers = map[Operation]Handler{PutObject: func(w http.ResponseWriter, req *Request) error {
		data, err := io.ReadAll(req.Body)
		got = &servedPut{op: req.Op, payload: data, md5: req.Body.MD5()}
		got.algorithm, got.checksum = req.Body.Checksum()
		return err
	}}
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("response %d %s", rec.Code, errorCode(rec))
	}
	return got
}

// TestRangeFixtures checks every Range row against parseRange, the reference
// of API v2 4.1 written below: one byte range, open, closed or suffix, with
// the end clamped to the object; InvalidRange for everything else. The rows
// also pin the 206/200/416 statuses and Content-Range the handler must send.
func TestRangeFixtures(t *testing.T) {
	for _, row := range loadCompat(t).Range {
		t.Run(row.Name, func(t *testing.T) {
			checkStatusPin(t, row.Error, row.Status)
			r, isByteRange, ok := parseRange(row.Range, row.Size)
			switch {
			case row.Error == ErrInvalidRange:
				if isByteRange && ok {
					t.Errorf("parseRange(%q, %d) = %d+%d, want unsatisfiable", row.Range, row.Size, r.start, r.length)
				}
			case !isByteRange:
				if row.Status != http.StatusOK {
					t.Errorf("row pins %d for a request whose Range is ignored", row.Status)
				}
				if r.length != row.Length {
					t.Errorf("ignored range serves %d bytes, want %d", r.length, row.Length)
				}
			case !ok:
				t.Errorf("parseRange(%q, %d) rejected the range", row.Range, row.Size)
			default:
				if row.Status != http.StatusPartialContent {
					t.Errorf("a satisfiable byte range pins 206, row says %d", row.Status)
				}
				if r.start != row.Start || r.length != row.Length {
					t.Errorf("parseRange(%q, %d) = %d+%d, want %d+%d", row.Range, row.Size, r.start, r.length, row.Start, row.Length)
				}
				if cr := fmt.Sprintf("bytes %d-%d/%d", r.start, r.start+r.length-1, row.Size); cr != row.ContentRange {
					t.Errorf("Content-Range %s, want %s", cr, row.ContentRange)
				}
			}
		})
	}
}

// TestHeadFixtures runs the Resolve-level HEAD rows (partNumber is P2,
// If-Match belongs to GetObject) through the real parser, checks every error
// row is a bodyless response with the common headers (API v2 4.2), and pins
// the checksum headers of x-amz-checksum-mode against the reference from
// 2.7.
func TestHeadFixtures(t *testing.T) {
	for _, row := range loadCompat(t).Head {
		t.Run(row.Name, func(t *testing.T) {
			checkStatusPin(t, row.Error, row.Status)
			if len(row.Request) > 0 {
				route, e := Resolve(readFixtureRequest(t, row.Request))
				if e == nil || e.Code != row.Error {
					t.Errorf("Resolve: %s %v, want %s", route.Op, e, row.Error)
				}
			}
			if len(row.RequestHeaders) > 0 {
				req := httptest.NewRequest(http.MethodHead, "/demo/k", nil)
				for k, v := range row.RequestHeaders {
					req.Header.Set(k, v)
				}
				route, e := Resolve(req)
				if e != nil || route.Op != HeadObject {
					t.Errorf("Resolve: %s %v, want HeadObject", route.Op, e)
				}
			}
			if row.Error != "" {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodHead, "/demo/k", nil)
				if err := WriteError(rec, req, &Error{Code: row.Error, Bucket: "demo", Key: "k"}, "COMPATFIXTURE00"); err != nil {
					t.Fatal(err)
				}
				if rec.Code != row.Status {
					t.Errorf("HEAD error status %d, want %d", rec.Code, row.Status)
				}
				if rec.Body.Len() != 0 {
					t.Errorf("HEAD error carries a body: %q", rec.Body.String())
				}
				for _, name := range row.CommonHeaders {
					if rec.Header().Get(name) == "" {
						t.Errorf("HEAD error lacks %s", name)
					}
				}
			}
			// The checksum rows are the contract of x-amz-checksum-mode: the
			// reference from 2.7 must return exactly the headers the row pins.
			for name, value := range row.Headers {
				if !strings.HasPrefix(name, "x-amz-checksum-") || name == "x-amz-checksum-type" {
					continue
				}
				got := headChecksumHeaders(row.RequestHeaders["x-amz-checksum-mode"], "CRC64NVME", value)
				if got == nil || got[name] != value {
					t.Fatalf("reference for mode %q does not echo %s", row.RequestHeaders["x-amz-checksum-mode"], name)
				}
				if typ := got["x-amz-checksum-type"]; typ != "FULL_OBJECT" {
					t.Errorf("reference checksum type %q, want FULL_OBJECT", typ)
				}
			}
			if row.NoChecksum && len(row.RequestHeaders) == 0 && headChecksumHeaders("", "CRC64NVME", "AAAAAAAAAAA=") != nil {
				t.Error("absent checksum mode must not send checksum headers")
			}
		})
	}
}

// TestXMLFixtures runs every XML body row through Payload.DecodeXML, the
// parser CompleteMultipartUpload, DeleteObjects and CreateBucket share
// (API v2 2.2.2 step 4): exactly one XML document, no directive, and a
// payload error ranking before MalformedXML.
func TestXMLFixtures(t *testing.T) {
	for _, row := range loadCompat(t).XML {
		t.Run(row.Name, func(t *testing.T) {
			checkStatusPin(t, row.Error, row.Status)
			method := http.MethodPost
			if row.Op == PutObject {
				method = http.MethodPut
			}
			req := httptest.NewRequest(method, "/demo", strings.NewReader(row.Body))
			p := newPayload(req, row.Op, nil)
			limit := row.Limit
			if limit == 0 {
				limit = 1 << 20
			}
			type completeDoc struct {
				XMLName xml.Name `xml:"CompleteMultipartUpload"`
				Parts   []struct {
					PartNumber, ETag []string
				} `xml:"Part"`
			}
			type deleteDoc struct {
				XMLName xml.Name `xml:"Delete"`
				Objects []struct {
					Key string
				} `xml:"Object"`
			}
			var err error
			switch row.Op {
			case DeleteObjects:
				var doc deleteDoc
				err = p.DecodeXML(&doc, limit)
				if err == nil && len(doc.Objects) != row.Objects {
					t.Errorf("%d objects, want %d", len(doc.Objects), row.Objects)
				}
			default:
				var doc completeDoc
				err = p.DecodeXML(&doc, limit)
				if err == nil && len(doc.Parts) != row.Parts {
					t.Errorf("%d parts, want %d", len(doc.Parts), row.Parts)
				}
			}
			if code := codeOf(err); code != row.Error || err != nil && code == "" {
				t.Errorf("DecodeXML: %v, want %q", err, row.Error)
			}
		})
	}
}

// checkStatusPin keeps fixture, code and document together: a row that
// expects an S3 error pins the HTTP status of API v2 2.4, and that is what
// errorCodes — the table errors.json also pins — must answer.
func checkStatusPin(t *testing.T, code ErrorCode, status int) {
	t.Helper()
	if code == "" {
		return
	}
	if _, ok := errorCodes[code]; !ok {
		t.Errorf("error %s is not in API v2 2.4", code)
		return
	}
	if got := (&Error{Code: code}).Status(); got != status {
		t.Errorf("%s: status %d, fixture pins %d", code, got, status)
	}
}

// byteRange is one resolved byte range of a GetObject.
type byteRange struct {
	start, length int64
}

// parseRange is the reference of API v2 4.1: spec against an object of size
// bytes. isByteRange is false when the header carries no byte range at all
// (absent, or a unit other than bytes, which RFC 9110 says to ignore: the
// object is served whole); ok is false when it is a byte range DSG must
// answer InvalidRange: not exactly one range of the form bytes=a-b, bytes=a-
// or bytes=-n, a start at or beyond the object, a zero suffix, or an object
// of zero bytes. A closed end past the object is clamped to its last byte.
func parseRange(spec string, size int64) (r byteRange, isByteRange, ok bool) {
	spec = strings.TrimSpace(spec)
	unit, rest, found := strings.Cut(spec, "=")
	if !found || unit != "bytes" {
		return byteRange{0, size}, false, false
	}
	if strings.Contains(rest, ",") {
		return byteRange{}, true, false
	}
	first, last, hasDash := strings.Cut(rest, "-")
	if !hasDash || first == "" && last == "" {
		return byteRange{}, true, false
	}
	if first == "" { // suffix -n
		n, err := strconv.ParseUint(last, 10, 63)
		if err != nil || n == 0 || size == 0 {
			return byteRange{}, true, false
		}
		l := min(int64(n), size)
		return byteRange{size - l, l}, true, true
	}
	start, err := strconv.ParseUint(first, 10, 63)
	if err != nil || int64(start) >= size {
		return byteRange{}, true, false
	}
	if last == "" { // open a-
		return byteRange{int64(start), size - int64(start)}, true, true
	}
	end, err := strconv.ParseUint(last, 10, 63)
	if err != nil || end < start {
		return byteRange{}, true, false
	}
	if end >= uint64(size) {
		end = uint64(size) - 1
	}
	return byteRange{int64(start), int64(end) - int64(start) + 1}, true, true
}

// headChecksumHeaders is the reference of API v2 2.7 for HeadObject: with
// x-amz-checksum-mode ENABLED the response carries the stored checksum and
// its type; with the mode absent it carries neither. algorithm is the one
// stored at PUT, upper case; value is the stored digest as sent.
func headChecksumHeaders(mode, algorithm, value string) map[string]string {
	if mode != "ENABLED" {
		return nil
	}
	name := map[string]string{"CRC64NVME": "crc64nvme", "CRC32": "crc32", "CRC32C": "crc32c", "SHA1": "sha1", "SHA256": "sha256"}[algorithm]
	if name == "" {
		return nil
	}
	return map[string]string{"x-amz-checksum-" + name: value, "x-amz-checksum-type": "FULL_OBJECT"}
}
