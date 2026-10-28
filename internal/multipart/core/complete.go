// Package core contains HTTP-independent normalized Complete semantics.
package core

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"strconv"
)

const minPartSize = 5 << 20
const maxObjectSize = 100 << 30

func ValidChecksumMode(algorithm, mode string) bool {
	_, ok := checksumHash(algorithm)
	_, full := crcs[algorithm]
	return ok && (mode == "COMPOSITE" || mode == "FULL_OBJECT" && full)
}

type ValidationError struct{ Code, Message string }

func (e *ValidationError) Error() string { return e.Code + ": " + e.Message }
func fail(code, message string) error    { return &ValidationError{code, message} }
func checksumHash(alg string) (hash.Hash, bool) {
	switch alg {
	case "SHA1":
		return sha1.New(), true
	case "SHA256":
		return sha256.New(), true
	case "CRC32":
		return crc32.NewIEEE(), true
	case "CRC32C":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli)), true
	case "CRC64NVME":
		return crc64.New(crc64.MakeTable(0x9a6c9329ac4bc9b5)), true
	}
	return nil, false
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
			return nil, Result{}, fail("InvalidPart", fmt.Sprintf("part %d was not uploaded or its ETag differs", p.PartNumber))
		}
		selected[i] = h
	}
	for i, p := range in.Parts {
		if p.Checksum != nil && *p.Checksum != selected[i].Checksum {
			return nil, Result{}, fail("BadDigest", fmt.Sprintf("Checksum%s of part %d differs from the one verified at UploadPart", in.ChecksumAlgorithm, p.PartNumber))
		}
	}
	var total int64
	for i, h := range selected {
		if i < len(selected)-1 && h.Size < minPartSize {
			return nil, Result{}, fail("EntityTooSmall", fmt.Sprintf("part %d is smaller than 5 MiB and not the last", h.PartNumber))
		}
		total += h.Size
	}
	if total > maxObjectSize {
		return nil, Result{}, fail("EntityTooLarge", "the object would be larger than 100 GiB")
	}
	if in.ObjectSize != nil && *in.ObjectSize != total {
		return nil, Result{}, fail("InvalidRequest", fmt.Sprintf("x-amz-mp-object-size is %d, the parts total %d bytes", *in.ObjectSize, total))
	}

	res, err := response(in, selected)
	if err != nil {
		return nil, Result{}, err
	}
	if in.ObjectChecksum != nil && *in.ObjectChecksum != res.Checksum {
		return nil, Result{}, fail("BadDigest", "the object checksum differs from the combined part checksums")
	}
	return selected, res, nil
}

// response computes the Result of the selected heads: FULL_OBJECT combines
// the part CRCs, COMPOSITE hashes the binary part digests.
func response(in Input, heads []Head) (Result, error) {
	composite, ok := checksumHash(in.ChecksumAlgorithm)
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
