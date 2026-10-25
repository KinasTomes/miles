package s3

import (
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// Fuzz targets over the parser surface the gateway trusts before any
// handler runs (API v2 2.2.2 and 3): the route table, the SigV4 header and
// presigned query, the aws-chunked decoder with its chunk and trailer
// signatures, and the shared XML body parser. Every seed is a request of the
// pinned clients recorded in transport.json or a row of routes.json,
// compat.json and multipart.json, so the corpus stays close to what
// AWS CLI 2.36.41, boto3 1.43.114 and curl 8.21.0 actually send; the fuzzer
// then corrupts bytes, framing and headers around them.
//
// The seeds run as part of every `go test`; `make fuzz` runs each target on
// random inputs for a bounded time. A crasher found that way is minimized,
// added to compat.json or transport.json as a regression row, and its real
// error recorded — that is how parser findings make it back into the
// fixtures before the API freeze.

// addRequestSeeds adds the raw request lines of a fixture row, with its
// body, as FuzzResolve seeds.
func addRequestSeeds(f *testing.F, lines []string, body string) {
	f.Add(strings.Join(lines, "\r\n") + "\r\n\r\n" + body)
}

func FuzzResolve(f *testing.F) {
	var routesFixture []struct {
		Request []string
	}
	readFixture(f, "routes.json", &routesFixture)
	for _, row := range routesFixture {
		addRequestSeeds(f, row.Request, "")
	}
	compat := loadCompat(f)
	for _, row := range compat.Keys {
		addRequestSeeds(f, row.Request, "")
	}
	for _, row := range compat.Head {
		addRequestSeeds(f, row.Request, "")
	}
	tf := loadTransport(f)
	for _, c := range tf.Cases {
		addRequestSeeds(f, c.Request, c.Body)
	}
	f.Add("PUT /demo/k HTTP/1.1\r\nHost: h\r\nRange: bytes=0-\r\n\r\n")
	f.Add("GET /demo/caf%C3%A9/%2F%25%2B.txt?list-type=2&prefix=%FF HTTP/1.1\r\nHost: h\r\n\r\n")

	f.Fuzz(func(t *testing.T, raw string) {
		req, err := http.ReadRequest(bufio.NewReader(strings.NewReader(raw + "\r\n\r\n")))
		if err != nil {
			return // never an HTTP request; a real server answers 400
		}
		route, e := Resolve(req)
		if e != nil && route.Op != "" {
			t.Errorf("Resolve returned both a route and an error: %s %v", route.Op, e)
		}
		if e != nil {
			var code ErrorCode = e.Code
			if _, ok := errorCodes[code]; !ok {
				t.Errorf("Resolve error %q is not an API v2 2.4 code", code)
			}
		}
	})
}

func FuzzSigV4(f *testing.F) {
	tf := loadTransport(f)
	for _, c := range tf.Cases {
		auth := ""
		raw := strings.Join(c.Request, "\r\n")
		if _, s, ok := strings.Cut(raw, "Authorization: "); ok {
			auth, _, _ = strings.Cut(s, "\r\n")
		}
		query := ""
		if i := strings.IndexByte(raw, '?'); i >= 0 {
			rest := raw[i+1:]
			query, _, _ = strings.Cut(rest, " ")
		}
		f.Add(auth, query, "STREAMING-UNSIGNED-PAYLOAD-TRAILER")
	}
	f.Add("AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20261018/us-east-1/s3/aws4_request, SignedHeaders=host, Signature="+strings.Repeat("ab", 32),
		"X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIAIOSFODNN7EXAMPLE%2F20261018%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20261018T155527Z&X-Amz-Expires=900&X-Amz-SignedHeaders=host&X-Amz-Signature="+strings.Repeat("cd", 32),
		"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")

	f.Fuzz(func(t *testing.T, auth, rawQuery, mode string) {
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			return // Resolve answers InvalidArgument before the auth layer
		}
		r := httptest.NewRequest(http.MethodPut, "/demo/k", nil)
		r.Header.Set("Authorization", auth)
		if mode != "" {
			r.Header.Set("X-Amz-Content-Sha256", mode)
		}
		s, e := parseSigV4(r, query, "us-east-1")
		if e != nil {
			return
		}
		// Accepted: the signature is 64 hex digits and the scope splits, and
		// the canonical request of the same request is computable.
		if _, ok := hexDigest(s.signature); !ok {
			t.Errorf("accepted signature %q is not 64 hex digits", s.signature)
		}
		if strings.Count(s.scope, "/") != 3 {
			t.Errorf("accepted scope %q", s.scope)
		}
		canonicalRequest(r, query, &s)
	})
}

// fuzzSigner signs chunks with a fixed key and a fixed seed signature, so
// signed aws-chunked fuzz inputs exercise the chunk and trailer signature
// verification (every signature of a corrupted body fails closed) while
// unsigned-trailer inputs reach the trailer parser itself.
func fuzzSigner() *signer {
	return &signer{
		key:     []byte("fuzz-key"),
		amzDate: "20261018T000000Z",
		scope:   "20261018/us-east-1/s3/aws4_request",
		seed:    strings.Repeat("00", 32),
	}
}

// decodeHTTPChunked strips the HTTP chunked framing a captured transport
// body still carries; the aws-chunked decoder sits above it. A seed body
// that ends mid-chunk (the truncated variant) decodes up to the cut.
func decodeHTTPChunked(f *testing.F, body string) []byte {
	data, err := io.ReadAll(httputil.NewChunkedReader(strings.NewReader(body)))
	if err != nil && len(data) == 0 {
		f.Helper()
		f.Fatalf("seed body is not HTTP chunked: %v", err)
	}
	return data
}

func FuzzAWSChunked(f *testing.F) {
	tf := loadTransport(f)
	for _, c := range tf.Cases {
		if c.BodyOmitted > 0 || c.Body == "" {
			continue
		}
		head := strings.ToLower(strings.Join(c.Request, "\n"))
		if !strings.Contains(head, "content-encoding: aws-chunked") {
			continue // a plain body; only its framing differs
		}
		body := []byte(c.Body)
		if strings.Contains(head, "transfer-encoding: chunked") {
			body = decodeHTTPChunked(f, c.Body)
		}
		f.Add(body, uint8(0), int64(len(body)), "x-amz-checksum-crc64nvme")
		f.Add(body, uint8(1), int64(len(body)), "x-amz-checksum-crc64nvme")
		f.Add(body, uint8(2), int64(len(body)), "")
	}
	f.Add([]byte("0\r\n\r\n"), uint8(0), int64(0), "x-amz-checksum-crc64nvme")
	f.Add([]byte("400\r\n"+strings.Repeat("a", 0x400)+"\r\n0\r\nx-amz-checksum-crc64nvme:AAAAAAAAAAA=\r\n\r\n"), uint8(0), int64(0), "x-amz-checksum-crc64nvme")

	f.Fuzz(func(t *testing.T, body []byte, mode uint8, decodedLen int64, trailer string) {
		modes := [3]string{streamingUnsignedTrailer, streamingSigned, streamingSignedTrailer}
		kind := modes[int(mode)%len(modes)]
		r := httptest.NewRequest(http.MethodPut, "/demo/k", bytes.NewReader(body))
		r.Header.Set("Content-Encoding", "aws-chunked")
		r.Header.Set("X-Amz-Content-Sha256", kind)
		r.Header.Set("X-Amz-Decoded-Content-Length", strconv.FormatInt(max(0, decodedLen), 10))
		if trailer != "" {
			r.Header.Set("X-Amz-Trailer", trailer)
		} else if kind != streamingSigned {
			r.Header.Set("X-Amz-Trailer", "x-amz-checksum-crc64nvme")
		}
		p := newPayload(r, PutObject, fuzzSigner())

		// The read must terminate with io.EOF (every check passed) or an
		// *Error of API v2 2.4 — anything else is a parser finding.
		total := 0
		buf := make([]byte, 64<<10)
		for {
			n, err := p.Read(buf)
			total += n
			if err != nil {
				if err != io.EOF {
					var e *Error
					if !errors.As(err, &e) {
						t.Fatalf("aws-chunked decoder returned %v, want *Error or io.EOF", err)
					}
					if _, ok := errorCodes[e.Code]; !ok {
						t.Fatalf("decoder error %q is not an API v2 2.4 code", e.Code)
					}
				}
				return
			}
			if n == 0 {
				t.Fatal("aws-chunked Read made no progress")
			}
			if total > 1<<20 {
				t.Fatalf("aws-chunked Read produced %d bytes, want bounded by x-amz-decoded-content-length", total)
			}
		}
	})
}

func FuzzDecodeXML(f *testing.F) {
	compat := loadCompat(f)
	for _, row := range compat.XML {
		f.Add([]byte(row.Body), int64(len(row.Body)))
	}
	var mp struct {
		Canonical []struct{ Body string } `json:"canonical"`
	}
	readFixture(f, "multipart.json", &mp)
	for _, row := range mp.Canonical {
		if row.Body != "" {
			f.Add([]byte(row.Body), int64(len(row.Body)))
		}
	}
	f.Add([]byte("<?xml version=\"1.0\"?><CompleteMultipartUpload></CompleteMultipartUpload>"), int64(1<<20))

	f.Fuzz(func(t *testing.T, data []byte, limit int64) {
		r := httptest.NewRequest(http.MethodPost, "/demo/k?uploadId=UP1", bytes.NewReader(data))
		p := newPayload(r, CompleteMultipartUpload, nil)
		var doc struct {
			XMLName xml.Name `xml:"CompleteMultipartUpload"`
			Parts   []struct {
				PartNumber, ETag []string
			} `xml:"Part"`
		}
		err := p.DecodeXML(&doc, max(1, limit))
		if err == nil && int64(len(data)) > max(1, limit) {
			t.Fatalf("DecodeXML accepted %d bytes under a limit of %d", len(data), max(1, limit))
		}
		if err != nil {
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("DecodeXML returned %v, want *Error or nil", err)
			}
		}
	})
}
