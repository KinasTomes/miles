package s3

import (
	"bufio"
	"bytes"
	"cmp"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// checksumHashes are the algorithms of API v2 2.7 by lower-case header name.
// Values on the wire are the base64 of the big-endian digest.
var checksumHashes = map[string]func() hash.Hash{
	"crc32":     func() hash.Hash { return crc32.NewIEEE() },
	"crc32c":    func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) },
	"crc64nvme": func() hash.Hash { return crc64.New(crc64NVME) },
	"sha1":      sha1.New,
	"sha256":    sha256.New,
}

// crc64NVME is CRC-64/NVME: reflected polynomial 0xad93d23594c93659, init and
// xorout all ones as in hash/crc64.
var crc64NVME = crc64.MakeTable(0x9a6c9329ac4bc9b5)

// Payload is the logical body of a request: its bytes after HTTP chunked and
// aws-chunked decoding, the bytes checksums, the ETag and the engine work on
// (API v2 2.7). It checks the body while it is read, in the order of API v2
// 2.2.2 step 4: framing and length, chunk and trailer signatures,
// x-amz-content-sha256, Content-MD5, then the checksum header or trailer.
// Read returns an *Error at the first failure, and io.EOF only once every
// check passed: a handler that commits on io.EOF never commits a bad body.
type Payload struct {
	src     io.Reader
	chunked *awsChunked // nil unless the body is aws-chunked
	err     error       // sticky; io.EOF once the body passed every check
	hashes  io.Writer

	sha256    hash.Hash // x-amz-content-sha256 is a hex digest
	wantSHA   []byte
	md5       hash.Hash // Content-MD5 was sent, or the operation has an MD5 ETag
	wantMD5   string
	sum       hash.Hash
	algorithm string // upper case, as in API v2 2.7
	wantSum   string // x-amz-checksum-<alg> header; a trailer value replaces it
}

// newPayload wraps the body of a request resolved to op. s is nil only when
// the body cannot carry signed chunks. A header error of the body is returned
// by the first Read, so it ranks after the checks that run before the body.
func newPayload(r *http.Request, op Operation, s *signer) *Payload {
	p := &Payload{src: r.Body}
	var hashes []io.Writer
	alg, want := requestChecksum(r.Header)
	switch {
	case op == CompleteMultipartUpload:
		alg, want = "", "" // its checksum headers describe the object, not the XML body
	case alg == "" && (op == PutObject || op == UploadPart):
		alg = "crc64nvme" // stored when the client names no algorithm (API v2 2.7)
	}
	if alg != "" {
		p.sum, p.algorithm, p.wantSum = checksumHashes[alg](), strings.ToUpper(alg), want
		hashes = append(hashes, p.sum)
	}
	if p.wantMD5 = r.Header.Get("Content-Md5"); p.wantMD5 != "" || op == PutObject || op == UploadPart {
		p.md5 = md5.New()
		hashes = append(hashes, p.md5)
	}
	mode := r.Header.Get("X-Amz-Content-Sha256")
	if sum, ok := hexDigest(mode); ok {
		p.sha256, p.wantSHA = sha256.New(), sum
		hashes = append(hashes, p.sha256)
	}
	p.hashes = io.MultiWriter(hashes...)
	if strings.HasPrefix(mode, "STREAMING-") {
		p.chunked, p.err = newAWSChunked(r, mode, s)
		p.src = p.chunked
	}
	return p
}

// requestChecksum returns the checksum algorithm a request names, in lower
// case, and the value of its x-amz-checksum-<alg> header; a trailer value
// only arrives after the body. Resolve has already rejected unsupported
// algorithms and requests naming more than one.
func requestChecksum(h http.Header) (alg, value string) {
	if t := h.Get("X-Amz-Trailer"); t != "" {
		return strings.TrimPrefix(strings.ToLower(strings.TrimSpace(t)), "x-amz-checksum-"), ""
	}
	for name, values := range h {
		a, ok := strings.CutPrefix(strings.ToLower(name), "x-amz-checksum-")
		if ok && a != "algorithm" && a != "mode" && a != "type" {
			return a, values[0]
		}
	}
	return strings.ToLower(cmp.Or(h.Get("X-Amz-Sdk-Checksum-Algorithm"), h.Get("X-Amz-Checksum-Algorithm"))), ""
}

func (p *Payload) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	n, err := p.src.Read(b)
	p.hashes.Write(b[:n])
	switch {
	case err == io.EOF:
		if e := p.verify(); e != nil {
			err = e
		}
	case errors.Is(err, io.ErrUnexpectedEOF):
		err = &Error{Code: ErrIncompleteBody}
	}
	p.err = err
	return n, err
}

func (p *Payload) verify() *Error {
	if p.sha256 != nil && !bytes.Equal(p.sha256.Sum(nil), p.wantSHA) {
		return &Error{Code: ErrXAmzContentSHA256Mismatch}
	}
	if p.wantMD5 != "" && base64.StdEncoding.EncodeToString(p.md5.Sum(nil)) != p.wantMD5 {
		return &Error{Code: ErrBadDigest}
	}
	want := p.wantSum
	if p.chunked != nil && p.chunked.value != "" {
		want = p.chunked.value
	}
	if p.sum != nil && want != "" && base64.StdEncoding.EncodeToString(p.sum.Sum(nil)) != want {
		return &Error{Code: ErrBadDigest, Message: "The " + p.algorithm + " checksum you specified did not match the calculated checksum."}
	}
	return nil
}

// MD5 returns the MD5 digest of the payload, the ETag of PutObject and
// UploadPart, once Read returned io.EOF. It is nil for other operations that
// sent no Content-MD5.
func (p *Payload) MD5() []byte {
	if p.md5 == nil {
		return nil
	}
	return p.md5.Sum(nil)
}

// Checksum returns the checksum of the payload once Read returned io.EOF: the
// algorithm the request names, already checked against the value it sent, or
// CRC64NVME for PutObject and UploadPart (API v2 2.7). Algorithm is empty
// when there is none.
func (p *Payload) Checksum() (algorithm string, digest []byte) {
	if p.sum == nil {
		return "", nil
	}
	return p.algorithm, p.sum.Sum(nil)
}

// DecodeXML reads the whole payload, then unmarshals it into v, so a payload
// error ranks before MalformedXML. A payload longer than limit bytes is
// EntityTooLarge and is not read further.
func (p *Payload) DecodeXML(v any, limit int64) error {
	data, err := io.ReadAll(io.LimitReader(p, limit+1))
	switch {
	case err != nil:
		return err
	case int64(len(data)) > limit:
		return &Error{Code: ErrEntityTooLarge, Message: fmt.Sprintf("The XML body exceeds %d bytes.", limit)}
	case xml.Unmarshal(data, v) != nil:
		return &Error{Code: ErrMalformedXML}
	}
	return nil
}

// awsChunked decodes an aws-chunked body (API v2 2.2.2): chunks
// "<hex size>[;chunk-signature=<sig>]\r\n<data>\r\n", a last chunk of size 0,
// trailer lines, then an empty line. It checks the framing, the data total
// against x-amz-decoded-content-length, each chunk and trailer signature when
// signed, and that the trailer is exactly the checksum x-amz-trailer names.
type awsChunked struct {
	r         *bufio.Reader
	signer    *signer   // nil: chunks are not signed
	trailer   string    // trailer x-amz-trailer names; empty when there is none
	remaining int64     // data x-amz-decoded-content-length still allows
	chunk     int64     // data left in the current chunk
	started   bool      // a chunk header was read, so its data ends with CRLF
	chunkSig  string    // signature the current chunk claims
	chunkSum  hash.Hash // SHA-256 of the current chunk data, when signed
	prev      string    // previous signature in the chain
	value     string    // trailer checksum value, set at the end
	err       error     // sticky; io.EOF after the trailer
}

func invalidChunked(msg string) error { return &Error{Code: ErrInvalidRequest, Message: msg} }

func newAWSChunked(r *http.Request, mode string, s *signer) (*awsChunked, error) {
	encoded := false
	for _, v := range r.Header.Values("Content-Encoding") {
		for token := range strings.SplitSeq(v, ",") {
			encoded = encoded || strings.EqualFold(strings.TrimSpace(token), "aws-chunked")
		}
	}
	if !encoded {
		return nil, invalidChunked("x-amz-content-sha256 " + mode + " requires Content-Encoding aws-chunked")
	}
	n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
	if err != nil || n < 0 {
		return nil, invalidChunked("aws-chunked requires a valid x-amz-decoded-content-length")
	}
	c := &awsChunked{r: bufio.NewReader(r.Body), remaining: n}
	if mode != streamingUnsignedTrailer {
		c.signer, c.prev = s, s.seed
	}
	if mode != streamingSigned {
		c.trailer = strings.ToLower(strings.TrimSpace(r.Header.Get("X-Amz-Trailer")))
	}
	return c, nil
}

func (c *awsChunked) Read(b []byte) (int, error) {
	for c.chunk == 0 {
		if c.err != nil {
			return 0, c.err
		}
		c.err = c.next()
	}
	n, err := c.r.Read(b[:min(int64(len(b)), c.chunk)])
	c.chunk -= int64(n)
	if c.signer != nil {
		c.chunkSum.Write(b[:n])
	}
	return n, unexpected(err)
}

// next ends the current chunk and reads the header of the next one. After the
// last chunk it reads and checks the trailer and returns io.EOF.
func (c *awsChunked) next() error {
	if c.started {
		var crlf [2]byte
		if _, err := io.ReadFull(c.r, crlf[:]); err != nil {
			return unexpected(err)
		}
		if string(crlf[:]) != "\r\n" {
			return invalidChunked("aws-chunked data does not end with CRLF")
		}
		if err := c.checkChunk(); err != nil {
			return err
		}
	}
	c.started = true
	line, err := c.line()
	if err != nil {
		return err
	}
	size, ext, hasExt := strings.Cut(line, ";")
	n, err := strconv.ParseUint(size, 16, 63)
	if err != nil {
		return invalidChunked("aws-chunked size " + strconv.Quote(size) + " is not hexadecimal")
	}
	if c.signer == nil && hasExt {
		return invalidChunked("unsigned aws-chunked chunk has extension " + strconv.Quote(ext))
	}
	if c.signer != nil {
		sig, ok := strings.CutPrefix(ext, "chunk-signature=")
		if _, isHex := hexDigest(sig); !ok || !isHex {
			return invalidChunked("signed aws-chunked chunk requires chunk-signature")
		}
		c.chunkSig, c.chunkSum = sig, sha256.New()
	}
	if int64(n) > c.remaining {
		return invalidChunked("aws-chunked data exceeds x-amz-decoded-content-length")
	}
	c.remaining -= int64(n)
	if c.chunk = int64(n); n > 0 {
		return nil
	}
	if err := c.checkChunk(); err != nil {
		return err
	}
	if c.remaining > 0 {
		return &Error{Code: ErrIncompleteBody, Message: "The aws-chunked data is shorter than x-amz-decoded-content-length."}
	}
	return c.readTrailer()
}

// checkChunk checks the signature of the chunk just read, chained from the
// previous one.
func (c *awsChunked) checkChunk() error {
	if c.signer == nil {
		return nil
	}
	s := c.signer
	want := s.sign("AWS4-HMAC-SHA256-PAYLOAD\n" + s.amzDate + "\n" + s.scope + "\n" + c.prev + "\n" + emptySHA256 + "\n" + hex.EncodeToString(c.chunkSum.Sum(nil)))
	if !hmac.Equal([]byte(want), []byte(c.chunkSig)) {
		return &Error{Code: ErrSignatureDoesNotMatch, Message: "The aws-chunked chunk signature does not match."}
	}
	c.prev = c.chunkSig
	return nil
}

func (c *awsChunked) readTrailer() error {
	var name, value, sig string
	for lines := 0; ; {
		line, err := c.line()
		if err != nil {
			return err
		}
		if line == "" {
			break
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			return invalidChunked("malformed aws-chunked trailer line")
		}
		k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
		if c.signer != nil && c.trailer != "" && k == "x-amz-trailer-signature" {
			sig = v
			continue
		}
		if lines++; lines > 1 {
			return invalidChunked("the aws-chunked trailer must carry a single checksum")
		}
		name, value = k, v
	}
	if c.signer != nil && c.trailer != "" {
		var canonical string
		if name != "" {
			canonical = name + ":" + value + "\n"
		}
		sum := sha256.Sum256([]byte(canonical))
		s := c.signer
		want := s.sign("AWS4-HMAC-SHA256-TRAILER\n" + s.amzDate + "\n" + s.scope + "\n" + c.prev + "\n" + hex.EncodeToString(sum[:]))
		if !hmac.Equal([]byte(want), []byte(sig)) {
			return &Error{Code: ErrSignatureDoesNotMatch, Message: "The aws-chunked trailer signature does not match."}
		}
	}
	if name != c.trailer {
		return invalidChunked("the aws-chunked trailer must be " + cmp.Or(c.trailer, "empty") + ", got " + cmp.Or(name, "none"))
	}
	c.value = value
	if _, err := c.r.ReadByte(); err != io.EOF {
		if err == nil {
			return invalidChunked("bytes follow the aws-chunked trailer")
		}
		return err
	}
	return io.EOF
}

// line reads one CRLF-terminated line, at most the bufio buffer size.
func (c *awsChunked) line() (string, error) {
	l, err := c.r.ReadSlice('\n')
	switch {
	case err == bufio.ErrBufferFull:
		return "", invalidChunked("aws-chunked line is too long")
	case err != nil:
		return "", unexpected(err)
	case len(l) < 2 || l[len(l)-2] != '\r':
		return "", invalidChunked("aws-chunked line does not end with CRLF")
	}
	return string(l[:len(l)-2]), nil
}

// unexpected turns an EOF inside the aws-chunked framing into
// io.ErrUnexpectedEOF, which Payload reports as IncompleteBody.
func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
