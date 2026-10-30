package multipart

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
	"github.com/KinasTomes/miles/internal/multipart/core"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

// Service is the stateful port of the multipart handlers; object.Service
// implements it over the shared multipart store.
type Service interface {
	CreateUpload(context.Context, object.CreateUploadRequest) (storage.MultipartUpload, error)
	GetUpload(context.Context, string) (object.Upload, error)
	UploadPart(context.Context, object.PartRequest) (object.PartResult, error)
	ListParts(context.Context, object.ListPartsRequest) (storage.PartListPage, error)
	AbortUpload(context.Context, object.AbortRequest) error
}

// Handlers serve CreateMultipartUpload, UploadPart, ListParts and
// AbortMultipartUpload (API v2 4.3). Complete is not served here, so a full
// set of parts never makes an object. Now is the server clock of the state
// table.
type Handlers struct {
	Service Service
	Now     func() time.Time
}

// NewHandlers wires the multipart handlers into a Router's Handlers map.
func NewHandlers(svc Service, now func() time.Time) map[s3.Operation]s3.Handler {
	h := &Handlers{Service: svc, Now: now}
	return map[s3.Operation]s3.Handler{
		s3.CreateMultipartUpload: h.Create, s3.UploadPart: h.UploadPart,
		s3.ListParts: h.ListParts, s3.AbortMultipartUpload: h.Abort,
	}
}

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	Bucket   string
	Key      string
	UploadID string `xml:"UploadId"`
}

// Create starts an upload with the checksum mode of API v2 2.7: CRC64NVME
// FULL_OBJECT when the request names no algorithm, else the named algorithm
// with its default type unless x-amz-checksum-type is sent. The router has
// already rejected unsupported algorithms and pairs.
func (h *Handlers) Create(w http.ResponseWriter, r *s3.Request) error {
	if len(r.Key) > s3.MaxKeyBytes {
		return &s3.Error{Code: s3.ErrKeyTooLong, Bucket: r.Bucket, Key: r.Key}
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return err
	}
	algorithm := strings.ToUpper(r.HTTP.Header.Get("X-Amz-Checksum-Algorithm"))
	kind := r.HTTP.Header.Get("X-Amz-Checksum-Type")
	requested := algorithm != ""
	switch {
	case !requested:
		algorithm, kind = "CRC64NVME", "FULL_OBJECT"
	case kind == "" && algorithm == "CRC64NVME":
		kind = "FULL_OBJECT"
	case kind == "":
		kind = "COMPOSITE"
	}
	if !core.ValidChecksumMode(algorithm, kind) {
		return &s3.Error{Code: s3.ErrInvalidRequest, Message: algorithm + " does not support checksum type " + kind}
	}
	u, err := h.Service.CreateUpload(r.HTTP.Context(), object.CreateUploadRequest{
		Bucket: r.Access.Bucket, Key: r.Key, Metadata: s3.RequestMetadata(r.HTTP),
		ChecksumAlgorithm: algorithm, ChecksumType: kind, CredentialID: r.Caller.AccessKeyID,
	})
	if err != nil {
		return failure(r, err)
	}
	if requested {
		w.Header().Set("x-amz-checksum-algorithm", algorithm)
		w.Header().Set("x-amz-checksum-type", kind)
	}
	return s3.WriteXML(w, initiateResult{Bucket: r.Bucket, Key: r.Key, UploadID: u.ID})
}

// UploadPart stores one part with the checksum of the upload's algorithm,
// computed by the server when the request sends another one or none. A
// checksum the request sends is verified on the body either way. Only a
// CRC64NVME upload (the default when Create names no algorithm) accepts a part
// naming another algorithm, so clients without CRC64NVME (boto3 without
// awscrt) can upload to it; any other mismatch is InvalidRequest before the
// body is read.
func (h *Handlers) UploadPart(w http.ResponseWriter, r *s3.Request) error {
	u, err := h.upload(r)
	if err != nil {
		return err
	}
	length := s3.DecodedLength(r.HTTP)
	if length < 0 {
		return &s3.Error{Code: s3.ErrInvalidRequest, Message: "Content-Length or x-amz-decoded-content-length is required.", Bucket: r.Bucket, Key: r.Key}
	}
	if length > object.MaxPartBytes {
		return &s3.Error{Code: s3.ErrEntityTooLarge, Message: "A part may not exceed 5 GiB.", Bucket: r.Bucket, Key: r.Key}
	}
	requested := r.Body.RequestedChecksum()
	if requested != "" && requested != u.ChecksumAlgorithm && u.ChecksumAlgorithm != "CRC64NVME" {
		return &s3.Error{Code: s3.ErrInvalidRequest, Message: "The upload was created with the " + u.ChecksumAlgorithm + " checksum algorithm, not " + requested + ".", Bucket: r.Bucket, Key: r.Key}
	}
	body := &partBody{Payload: r.Body, algorithm: u.ChecksumAlgorithm}
	if cmp.Or(requested, "CRC64NVME") != u.ChecksumAlgorithm { // the payload's own checksum is of another algorithm
		body.sum, _ = s3.ChecksumHash(u.ChecksumAlgorithm)
	}
	number, _ := strconv.ParseUint(r.Query.Get("partNumber"), 10, 32) // the router checked 1..10000
	res, err := h.Service.UploadPart(r.HTTP.Context(), object.PartRequest{
		Upload: u.MultipartUpload, Number: uint32(number), Body: body, ContentLength: length, CredentialID: r.Caller.AccessKeyID,
	})
	if err != nil {
		return h.stateFailure(r, err)
	}
	algorithm, value := res.ChecksumAlgorithm, res.ChecksumValue
	if requested != "" && requested != algorithm { // echo the checksum the client sent
		name, digest := r.Body.Checksum()
		algorithm, value = name, base64.StdEncoding.EncodeToString(digest)
	}
	w.Header().Set("ETag", `"`+res.ETag+`"`)
	w.Header().Set("x-amz-checksum-"+strings.ToLower(algorithm), value)
	w.WriteHeader(http.StatusOK)
	return nil
}

type listPartsResult struct {
	XMLName              xml.Name `xml:"ListPartsResult"`
	Bucket               string
	Key                  string
	UploadID             string `xml:"UploadId"`
	PartNumberMarker     uint32
	NextPartNumberMarker uint32
	MaxParts             int
	IsTruncated          bool
	Parts                []listedPart `xml:"Part"`
	Initiator            principal
	Owner                principal
	StorageClass         string
	ChecksumAlgorithm    string
	ChecksumType         string
}

// listedPart has no LastModified: part heads keep no publication time, and
// pinned SDKs accept the element missing (not empty).
type listedPart struct {
	PartNumber        uint32
	ETag              string
	Size              int64
	ChecksumCRC32     string `xml:",omitempty"`
	ChecksumCRC32C    string `xml:",omitempty"`
	ChecksumCRC64NVME string `xml:",omitempty"`
	ChecksumSHA1      string `xml:",omitempty"`
	ChecksumSHA256    string `xml:",omitempty"`
}

// principal carries DSG user IDs, not AWS canonical IDs.
type principal struct{ ID string }

// ListParts returns one page of published parts after part-number-marker,
// at most max-parts (default and cap 1000) of them.
func (h *Handlers) ListParts(w http.ResponseWriter, r *s3.Request) error {
	u, err := h.upload(r)
	if err != nil {
		return err
	}
	invalid := func(msg string) error {
		return &s3.Error{Code: s3.ErrInvalidArgument, Message: msg, Bucket: r.Bucket, Key: r.Key}
	}
	maxParts := 1000
	if v, ok := r.Query["max-parts"]; ok {
		n, err := strconv.Atoi(v[0])
		if err != nil || n < 0 {
			return invalid("Argument max-parts must be an integer between 0 and 2147483647")
		}
		maxParts = min(n, 1000)
	}
	var marker uint32
	if v, ok := r.Query["part-number-marker"]; ok {
		n, err := strconv.ParseUint(v[0], 10, 32)
		if err != nil {
			return invalid("Argument part-number-marker must be an integer between 0 and 2147483647")
		}
		marker = uint32(n)
	}
	page, err := h.Service.ListParts(r.HTTP.Context(), object.ListPartsRequest{UploadID: u.ID, CredentialID: r.Caller.AccessKeyID, After: marker, Limit: max(maxParts, 1)})
	if err != nil {
		return h.stateFailure(r, err)
	}
	parts, truncated := page.Parts, page.Next != 0
	if maxParts == 0 {
		parts, truncated = nil, len(page.Parts) > 0
	}
	out := listPartsResult{
		Bucket: r.Bucket, Key: r.Key, UploadID: u.ID, PartNumberMarker: marker, NextPartNumberMarker: marker,
		MaxParts: maxParts, IsTruncated: truncated, Initiator: principal{u.Initiator}, Owner: principal{u.Owner},
		StorageClass: "STANDARD", ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType,
	}
	for _, p := range parts {
		lp := listedPart{PartNumber: p.Number, ETag: `"` + p.ETag + `"`, Size: p.Size}
		switch p.Checksum.Algorithm {
		case "CRC32":
			lp.ChecksumCRC32 = p.Checksum.Value
		case "CRC32C":
			lp.ChecksumCRC32C = p.Checksum.Value
		case "CRC64NVME":
			lp.ChecksumCRC64NVME = p.Checksum.Value
		case "SHA1":
			lp.ChecksumSHA1 = p.Checksum.Value
		case "SHA256":
			lp.ChecksumSHA256 = p.Checksum.Value
		}
		out.Parts = append(out.Parts, lp)
		out.NextPartNumberMarker = p.Number
	}
	return s3.WriteXML(w, out)
}

// Abort answers 204 only once the upload reached Aborted.
func (h *Handlers) Abort(w http.ResponseWriter, r *s3.Request) error {
	u, err := h.upload(r)
	if err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return err
	}
	if err := h.Service.AbortUpload(r.HTTP.Context(), object.AbortRequest{Upload: u.MultipartUpload, CredentialID: r.Caller.AccessKeyID}); err != nil {
		return h.stateFailure(r, err)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// upload is steps 6-7 of the API v2 2.2.3 order for the uploadId of r: the
// binding (NoSuchUpload, then AccessDenied without the initiator right),
// then the state table of 4.3.1, before anything else reads the upload.
func (h *Handlers) upload(r *s3.Request) (object.Upload, error) {
	id := r.Query.Get("uploadId")
	var binding *s3.UploadBinding
	var u object.Upload
	if uploadID(id) {
		var err error
		u, err = h.Service.GetUpload(r.HTTP.Context(), id)
		switch {
		case err == nil:
			binding = &s3.UploadBinding{BucketID: u.Object.BucketID, Key: u.Object.Key, Initiator: u.Initiator}
		case !errors.Is(err, storage.ErrNotFound):
			return u, failure(r, err)
		}
	}
	if e := r.CheckUpload(binding); e != nil {
		return u, e
	}
	if _, err := Dispatch(r.Op, status(u), "", h.Now()); err != nil {
		return u, err
	}
	return u, nil
}

// stateFailure answers a failed write from the state that won it: a freeze,
// an abort or expiry fences publication (OperationAborted or NoSuchUpload),
// a Complete conflicts with Abort. Otherwise err maps as is.
func (h *Handlers) stateFailure(r *s3.Request, err error) error {
	if errors.Is(err, storage.ErrFenced) || errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrNotFound) {
		u, readErr := h.Service.GetUpload(r.HTTP.Context(), r.Query.Get("uploadId"))
		if errors.Is(readErr, storage.ErrNotFound) {
			return &s3.Error{Code: s3.ErrNoSuchUpload, Bucket: r.Bucket, Key: r.Key}
		}
		if readErr == nil {
			if _, e := Dispatch(r.Op, status(u), "", h.Now()); e != nil {
				return e
			}
		}
	}
	return failure(r, err)
}

func failure(r *s3.Request, err error) error {
	var e *s3.Error
	if errors.As(err, &e) {
		return e
	}
	switch {
	case errors.Is(err, object.ErrLengthMismatch):
		e = &s3.Error{Code: s3.ErrIncompleteBody}
	case errors.Is(err, object.ErrPartTooLarge), errors.Is(err, object.ErrMetadataTooLarge):
		e = &s3.Error{Code: s3.ErrEntityTooLarge}
	case errors.Is(err, storage.ErrBucketNotFound):
		e = &s3.Error{Code: s3.ErrNoSuchBucket}
	case errors.Is(err, storage.ErrNotFound):
		e = &s3.Error{Code: s3.ErrNoSuchUpload}
	default:
		if e = s3.MetadataWriteError(err); e.Code == s3.ErrInternalError {
			return err
		}
	}
	e.Bucket, e.Key = r.Bucket, r.Key
	return e
}

func status(u object.Upload) Status {
	return Status{State: State(strings.ToLower(u.State)), ExpiresAt: u.ExpiresAt, LeaseExpiresAt: u.LeaseUntil,
		ReceiptExpiresAt: u.ReceiptExpiresAt, InputDigest: u.Input.SHA256}
}

// uploadID reports whether id has the upload_id format of API v2 2.2.3.
func uploadID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// partBody hashes the part with the upload's algorithm when the payload's
// own checksum is of another one (the CRC64NVME default).
type partBody struct {
	*s3.Payload
	algorithm string
	sum       hash.Hash
}

func (b *partBody) Read(p []byte) (int, error) {
	n, err := b.Payload.Read(p)
	if b.sum != nil {
		b.sum.Write(p[:n])
	}
	return n, err
}

func (b *partBody) Checksum() (string, []byte) {
	if b.sum == nil {
		return b.Payload.Checksum()
	}
	return b.algorithm, b.sum.Sum(nil)
}
