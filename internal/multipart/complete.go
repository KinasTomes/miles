// Package multipart holds the CompleteMultipartUpload contract of API v2
// 4.3.1 that the multipart handlers and the metadata backend share: the
// upload binding, the canonical input and its digest (validation rows 1-5),
// the checks of the frozen part heads with the response the receipt keeps
// (rows 6-11), and the state table of Complete, UploadPart, Abort and
// ListParts. Freeze, takeover, quota and the commit transaction are the
// handlers' and the backend's (W04).
package multipart

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/KinasTomes/miles/internal/api/s3"
)

const (
	maxParts      = 10000
	minPartSize   = 5 << 20
	maxObjectSize = 100 << 30
)

// Upload is what CreateMultipartUpload fixed for an upload_id: its binding
// (API v2 2.2.3), which handlers check with s3.Request.CheckUpload, and the
// checksum algorithm and type of 2.7, upper case (default CRC64NVME and
// FULL_OBJECT).
type Upload struct {
	s3.UploadBinding
	ChecksumAlgorithm, ChecksumType string
}

// Input is the canonical Complete input v1 of API v2 4.3.1. Fields are in
// serialization order.
type Input struct {
	V                 int     `json:"v"`
	BucketID          string  `json:"bucket_id"`
	Key               string  `json:"key"`
	ChecksumAlgorithm string  `json:"checksum_algorithm"`
	ChecksumType      string  `json:"checksum_type"`
	ObjectChecksum    *string `json:"object_checksum"`
	ObjectSize        *int64  `json:"object_size"`
	Parts             []Part  `json:"parts"`
}

// Part is a Part of the request: its ETag without the surrounding quotes in
// lower case, and its Checksum<ALG> in standard Base64, nil when not sent.
type Part struct {
	PartNumber int     `json:"part_number"`
	ETag       string  `json:"etag"`
	Checksum   *string `json:"checksum"`
}

// Canonical is the JSON of in without whitespace, escaped as encoding/json
// with SetEscapeHTML(false): "<", ">" and "&" stay, U+2028 and U+2029
// become \u2028 and \u2029.
func (in Input) Canonical() []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(in); err != nil {
		panic(err) // strings, ints and pointers to them always encode
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

// Digest is input_digest: lower-case hex SHA-256 of Canonical.
func (in Input) Digest() string {
	sum := sha256.Sum256(in.Canonical())
	return hex.EncodeToString(sum[:])
}

// CompleteXML is the body of CompleteMultipartUpload; handlers read it with
// s3.Payload.DecodeXML, which rejects anything that is not one XML document
// rooted at CompleteMultipartUpload, then pass it to Normalize. Children of
// Part are collected as sent so Normalize can see missing, repeated and
// unknown ones.
type CompleteXML struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber, ETag []string
		Other            []struct {
			XMLName xml.Name
			Value   string `xml:",chardata"`
		} `xml:",any"`
	} `xml:"Part"`
}

func fail(code s3.ErrorCode, msg string) *s3.Error { return &s3.Error{Code: code, Message: msg} }

// Normalize is rows 1 to 5 of the API v2 4.3.1 validation: the canonical
// input of a Complete on upload u with headers h and body doc, or its first
// error. Nothing changes the upload's state before it succeeds. The input
// takes bucket_id and key from u, so the binding must be checked first.
func Normalize(u Upload, h http.Header, doc *CompleteXML) (Input, *s3.Error) {
	in := Input{V: 1, BucketID: u.BucketID, Key: u.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType}
	if len(doc.Parts) == 0 {
		return in, fail(s3.ErrMalformedXML, "CompleteMultipartUpload needs at least one Part")
	}
	nums := make([]int, len(doc.Parts))
	for i, p := range doc.Parts {
		if len(p.PartNumber) != 1 || len(p.ETag) != 1 {
			return in, fail(s3.ErrMalformedXML, "every Part needs exactly one PartNumber and one ETag")
		}
		var err error
		if nums[i], err = strconv.Atoi(p.PartNumber[0]); err != nil {
			return in, fail(s3.ErrMalformedXML, "PartNumber must be an integer")
		}
		for _, c := range p.Other {
			if !strings.HasPrefix(c.XMLName.Local, "Checksum") {
				return in, fail(s3.ErrMalformedXML, "unexpected element "+c.XMLName.Local+" in Part")
			}
		}
	}

	if t, ok := h["X-Amz-Checksum-Type"]; ok && (len(t) != 1 || t[0] != u.ChecksumType) {
		return in, fail(s3.ErrInvalidRequest, "x-amz-checksum-type must be "+u.ChecksumType+", the type of CreateMultipartUpload")
	}
	for name, values := range h {
		alg, ok := strings.CutPrefix(strings.ToLower(name), "x-amz-checksum-")
		if !ok || alg == "type" || alg == "algorithm" || alg == "mode" {
			continue
		}
		switch {
		case u.ChecksumType == "COMPOSITE":
			return in, fail(s3.ErrInvalidRequest, "a COMPOSITE upload takes no object checksum")
		case !strings.EqualFold(alg, u.ChecksumAlgorithm):
			return in, fail(s3.ErrInvalidRequest, "the object checksum must use "+u.ChecksumAlgorithm+", the algorithm of CreateMultipartUpload")
		}
		if in.ObjectChecksum = digest(u.ChecksumAlgorithm, values); in.ObjectChecksum == nil {
			return in, fail(s3.ErrInvalidRequest, "x-amz-checksum-"+alg+" is not the Base64 of a "+u.ChecksumAlgorithm+" digest")
		}
	}
	sums := make([]*string, len(doc.Parts))
	for i, p := range doc.Parts {
		switch {
		case len(p.Other) == 0:
			continue
		case len(p.Other) > 1:
			return in, fail(s3.ErrInvalidRequest, "a Part takes at most one checksum")
		case p.Other[0].XMLName.Local != "Checksum"+u.ChecksumAlgorithm:
			return in, fail(s3.ErrInvalidRequest, "Part checksums must use "+u.ChecksumAlgorithm+", the algorithm of CreateMultipartUpload")
		}
		if sums[i] = digest(u.ChecksumAlgorithm, []string{p.Other[0].Value}); sums[i] == nil {
			return in, fail(s3.ErrInvalidRequest, p.Other[0].XMLName.Local+" is not the Base64 of a "+u.ChecksumAlgorithm+" digest")
		}
	}

	for _, n := range nums {
		if n < 1 || n > maxParts {
			return in, fail(s3.ErrInvalidArgument, "Part number must be an integer between 1 and 10000, inclusive")
		}
	}
	if v, ok := h["X-Amz-Mp-Object-Size"]; ok {
		n, err := strconv.ParseUint(strings.Join(v, ","), 10, 63)
		if err != nil {
			return in, fail(s3.ErrInvalidArgument, "x-amz-mp-object-size must be a non-negative integer")
		}
		in.ObjectSize = new(int64(n))
	}
	for i, n := range nums {
		if i > 0 && n <= nums[i-1] {
			return in, fail(s3.ErrInvalidPartOrder, "part numbers must be strictly ascending")
		}
		if u.ChecksumType == "COMPOSITE" && n != i+1 {
			return in, fail(s3.ErrInvalidPartOrder, "a COMPOSITE upload completes with parts 1 to N without gaps")
		}
	}
	for i, p := range doc.Parts {
		if u.ChecksumType == "COMPOSITE" && sums[i] == nil {
			return in, fail(s3.ErrInvalidRequest, "every Part of a COMPOSITE upload needs its Checksum"+u.ChecksumAlgorithm)
		}
		etag := p.ETag[0]
		if len(etag) >= 2 && etag[0] == '"' && etag[len(etag)-1] == '"' {
			etag = etag[1 : len(etag)-1]
		}
		in.Parts = append(in.Parts, Part{nums[i], strings.ToLower(etag), sums[i]})
	}
	return in, nil
}

// digest is the value of one checksum header or element when it is the
// standard Base64 of a digest of alg, else nil.
func digest(alg string, values []string) *string {
	h, ok := s3.ChecksumHash(alg)
	if !ok || len(values) != 1 {
		return nil
	}
	b, err := base64.StdEncoding.Strict().DecodeString(values[0])
	if err != nil || len(b) != h.Size() {
		return nil
	}
	return new(values[0])
}

// Head is a part head as the freeze snapshot holds it: what UploadPart
// published and verified, the ETag in lower-case hex without quotes and the
// checksum as standard Base64 of the upload's algorithm.
type Head struct {
	PartNumber int
	ETag       string
	Size       int64
	Checksum   string
}

// Result is the part of CompleteMultipartUploadResult the receipt keeps:
// the quoted ETag MD5(part MD5s)-N, and the checksum, suffixed -N for
// COMPOSITE, with its type (API v2 2.7, 4.3.1).
type Result struct {
	ETag, Checksum, ChecksumType string
}

// Assemble is rows 6 to 11 of the API v2 4.3.1 validation, run on the part
// heads frozen for in (by part number): the selected heads in input order
// and the response, or the first error. An *s3.Error reopens the upload
// Active; any other error is a bad stored head.
func Assemble(in Input, heads map[int]Head) ([]Head, Result, error) {
	selected := make([]Head, len(in.Parts))
	for i, p := range in.Parts {
		h, ok := heads[p.PartNumber]
		if !ok || h.ETag != p.ETag {
			return nil, Result{}, fail(s3.ErrInvalidPart, fmt.Sprintf("part %d was not uploaded or its ETag differs", p.PartNumber))
		}
		selected[i] = h
	}
	for i, p := range in.Parts {
		if p.Checksum != nil && *p.Checksum != selected[i].Checksum {
			return nil, Result{}, fail(s3.ErrBadDigest, fmt.Sprintf("Checksum%s of part %d differs from the one verified at UploadPart", in.ChecksumAlgorithm, p.PartNumber))
		}
	}
	var total int64
	for i, h := range selected {
		if i < len(selected)-1 && h.Size < minPartSize {
			return nil, Result{}, fail(s3.ErrEntityTooSmall, fmt.Sprintf("part %d is smaller than 5 MiB and not the last", h.PartNumber))
		}
		total += h.Size
	}
	if total > maxObjectSize {
		return nil, Result{}, fail(s3.ErrEntityTooLarge, "the object would be larger than 100 GiB")
	}
	if in.ObjectSize != nil && *in.ObjectSize != total {
		return nil, Result{}, fail(s3.ErrInvalidRequest, fmt.Sprintf("x-amz-mp-object-size is %d, the parts total %d bytes", *in.ObjectSize, total))
	}

	res, err := response(in, selected)
	if err != nil {
		return nil, Result{}, err
	}
	if in.ObjectChecksum != nil && *in.ObjectChecksum != res.Checksum {
		return nil, Result{}, fail(s3.ErrBadDigest, "the object checksum differs from the combined part checksums")
	}
	return selected, res, nil
}

// response computes the Result of the selected heads: FULL_OBJECT combines
// the part CRCs, COMPOSITE hashes the binary part digests.
func response(in Input, heads []Head) (Result, error) {
	composite, ok := s3.ChecksumHash(in.ChecksumAlgorithm)
	poly, full := crcs[in.ChecksumAlgorithm]
	switch {
	case !ok:
		return Result{}, fmt.Errorf("multipart: unknown checksum algorithm %q", in.ChecksumAlgorithm)
	case in.ChecksumType == "COMPOSITE":
		full = false
	case !full:
		return Result{}, fmt.Errorf("multipart: %s has no FULL_OBJECT checksum", in.ChecksumAlgorithm)
	}
	etags := md5.New()
	var crc uint64
	for i, h := range heads {
		m, err := hex.DecodeString(h.ETag)
		d, err2 := base64.StdEncoding.Strict().DecodeString(h.Checksum)
		if err != nil || len(m) != md5.Size || err2 != nil || len(d) != composite.Size() {
			return Result{}, fmt.Errorf("multipart: part %d head has ETag %q and checksum %q", h.PartNumber, h.ETag, h.Checksum)
		}
		etags.Write(m)
		composite.Write(d)
		switch {
		case !full:
		case i == 0:
			crc = poly.value(d)
		default:
			crc = poly.combine(crc, poly.value(d), h.Size)
		}
	}
	n := "-" + strconv.Itoa(len(heads))
	res := Result{ETag: `"` + hex.EncodeToString(etags.Sum(nil)) + n + `"`, ChecksumType: in.ChecksumType}
	if full {
		res.Checksum = base64.StdEncoding.EncodeToString(poly.bytes(crc))
	} else {
		res.Checksum = base64.StdEncoding.EncodeToString(composite.Sum(nil)) + n
	}
	return res, nil
}
