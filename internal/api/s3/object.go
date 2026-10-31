package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

// MaxKeyBytes is the S3 object key limit (API v2 4.1).
const MaxKeyBytes = 1024

// ObjectWriter is the stateful write port. The service streams the body to
// packs and publishes the head or tombstone; the handler owns HTTP validation
// and responses.
type ObjectWriter interface {
	PutObject(ctx context.Context, r object.PutRequest) (object.PutResult, error)
	CopyObject(ctx context.Context, r object.CopyRequest) (object.CopyResult, error)
	DeleteObject(ctx context.Context, r object.DeleteRequest) error
}

// ObjectReader is the Warm read port. GetObject evaluates SelectRange against
// its bound head before Ready or Emit. Stat answers HEAD without payload.
type ObjectReader interface {
	GetObject(ctx context.Context, r object.GetRequest) (object.GetResult, error)
	Stat(ctx context.Context, r object.GetRequest) (object.GetResult, error)
}

// ObjectLister is the consistent LIST port: one logical page per call.
type ObjectLister interface {
	ListObjects(ctx context.Context, r storage.ListObjectsRequest) (object.ListPage, error)
}

// Objects serves the object operations of API v2 4.2, 4.5 and 4.6: PutObject,
// CopyObject, GetObject, HeadObject, DeleteObject, DeleteObjects and
// ListObjectsV2.
type Objects struct {
	Writer ObjectWriter
	Reader ObjectReader
	Lister ObjectLister
}

// NewObjectHandlers wires the object handlers into a Router's Handlers map.
func NewObjectHandlers(writer ObjectWriter, reader ObjectReader, lister ObjectLister) map[Operation]Handler {
	o := &Objects{Writer: writer, Reader: reader, Lister: lister}
	return map[Operation]Handler{
		PutObject: o.PutObject, CopyObject: o.CopyObject, GetObject: o.GetObject, HeadObject: o.HeadObject,
		DeleteObject: o.DeleteObject, DeleteObjects: o.DeleteObjects, ListObjectsV2: o.ListObjectsV2,
	}
}

func objectKey(r *Request) error {
	if len(r.Key) > MaxKeyBytes {
		return &Error{Code: ErrKeyTooLong, Bucket: r.Bucket, Key: r.Key}
	}
	return nil
}

// DecodedLength is the logical body size: aws-chunked declares it separately
// from the encoded Content-Length (API v2 2.2.2). It is -1 when unknown.
func DecodedLength(r *http.Request) int64 {
	if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		n, err := strconv.ParseInt(r.Header.Get("X-Amz-Decoded-Content-Length"), 10, 64)
		if err != nil || n < 0 {
			return -1
		}
		return n
	}
	return r.ContentLength
}

// RequestMetadata keeps the HTTP metadata of API v2 4.2 in the head's lower
// case map. A missing Content-Type stores the S3 default explicitly.
func RequestMetadata(r *http.Request) map[string]string {
	metadata := map[string]string{"content-type": "binary/octet-stream"}
	for _, name := range []string{"Content-Type", "Cache-Control", "Content-Disposition", "Content-Language", "Expires"} {
		if v := r.Header.Get(name); v != "" {
			metadata[strings.ToLower(name)] = v
		}
	}
	encodings := []string{}
	for _, value := range r.Header.Values("Content-Encoding") {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token != "" && !strings.EqualFold(token, "aws-chunked") {
				encodings = append(encodings, token)
			}
		}
	}
	if len(encodings) > 0 {
		metadata["content-encoding"] = strings.Join(encodings, ", ")
	}
	for name, values := range r.Header {
		lower := strings.ToLower(name)
		if meta, ok := strings.CutPrefix(lower, "x-amz-meta-"); ok && meta != "" {
			metadata[lower] = strings.Join(values, ",")
		}
	}
	return metadata
}

func (o *Objects) PutObject(w http.ResponseWriter, r *Request) error {
	if o.Writer == nil {
		return &Error{Code: ErrNotImplemented, Message: "PutObject is not implemented"}
	}
	if err := objectKey(r); err != nil {
		return err
	}
	length := DecodedLength(r.HTTP)
	if length < 0 {
		return &Error{Code: ErrInvalidRequest, Message: "Content-Length or x-amz-decoded-content-length is required.", Bucket: r.Bucket, Key: r.Key}
	}
	metadata := RequestMetadata(r.HTTP)
	if err := object.ValidatePut(length, metadata); err != nil {
		return objectError(err, r.Bucket, r.Key)
	}
	result, err := o.Writer.PutObject(r.HTTP.Context(), object.PutRequest{
		Bucket: r.Access.Bucket, Key: r.Key, Body: r.Body, ContentLength: length,
		Metadata: metadata, UserID: r.Caller.UserID, CredentialID: r.Caller.AccessKeyID,
	})
	if err != nil {
		return objectError(err, r.Bucket, r.Key)
	}
	w.Header().Set("ETag", result.ETag)
	if name, ok := checksumHeaderName(result.ChecksumAlgorithm); ok && result.ChecksumValue != "" {
		w.Header().Set(name, result.ChecksumValue)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (o *Objects) GetObject(w http.ResponseWriter, r *Request) error {
	if o.Reader == nil {
		return &Error{Code: ErrNotImplemented, Message: "GetObject is not implemented"}
	}
	if err := objectKey(r); err != nil {
		return err
	}
	ctx := r.HTTP.Context()
	// Range and If-Match are selected after the reader binds its head. A prior
	// Stat could observe a different object size during a concurrent overwrite.
	hasRange := false
	status := http.StatusOK
	wrote := false
	proof := func(result object.GetResult) error {
		length := result.Info.Size
		if hasRange {
			if result.Range.Length <= 0 || result.Range.Validate(result.Info.Size) != nil {
				return &Error{Code: ErrInvalidRange, Bucket: r.Bucket, Key: r.Key}
			}
			length = result.Range.Length
		}
		h := w.Header()
		h.Set("ETag", result.Info.ETag)
		h.Set("Last-Modified", result.Info.LastModified.UTC().Format(http.TimeFormat))
		h.Set("Accept-Ranges", "bytes")
		h.Set("Content-Length", strconv.FormatInt(length, 10))
		writeObjectMetadata(h, result.Info.Metadata)
		if !hasRange {
			writeResponseChecksum(h, r.HTTP.Header.Get("X-Amz-Checksum-Mode"), result.Info.Checksum)
		}
		if hasRange {
			h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", result.Range.Offset, result.Range.Offset+result.Range.Length-1, result.Info.Size))
		}
		w.WriteHeader(status)
		wrote = true
		return nil
	}
	_, err := o.Reader.GetObject(ctx, object.GetRequest{
		Bucket: r.Bucket, BucketID: r.Access.Bucket.ID, Key: r.Key, CredentialID: r.Caller.AccessKeyID,
		SelectRange: func(info object.Info) (*storage.ByteRange, error) {
			start, length, selected, ok := parseObjectRange(r.HTTP.Header.Get("Range"), info.Size)
			if selected && !ok {
				return nil, &Error{Code: ErrInvalidRange, Bucket: r.Bucket, Key: r.Key}
			}
			if err := checkIfMatch(r, info.ETag); err != nil {
				return nil, err
			}
			hasRange = selected
			if selected {
				status = http.StatusPartialContent
				return &storage.ByteRange{Offset: start, Length: length}, nil
			}
			return nil, nil
		}, Ready: proof,
		Emit: func(_ context.Context, b []byte) error {
			_, err := w.Write(b)
			return err
		},
	})
	if err != nil {
		if wrote {
			// Headers are committed; the only legal move is to abort the stream
			// (API v2 4.2), never to append an XML error to object bytes.
			panic(http.ErrAbortHandler)
		}
		return objectError(err, r.Bucket, r.Key)
	}
	return nil
}

func (o *Objects) HeadObject(w http.ResponseWriter, r *Request) error {
	if o.Reader == nil {
		return &Error{Code: ErrNotImplemented, Message: "HeadObject is not implemented"}
	}
	if err := objectKey(r); err != nil {
		return err
	}
	ctx := r.HTTP.Context()
	stat, err := o.Reader.Stat(ctx, object.GetRequest{Bucket: r.Bucket, BucketID: r.Access.Bucket.ID, Key: r.Key, CredentialID: r.Caller.AccessKeyID})
	if err != nil {
		return objectError(err, r.Bucket, r.Key)
	}
	_, length, hasRange, ok := parseObjectRange(r.HTTP.Header.Get("Range"), stat.Info.Size)
	if hasRange && !ok {
		return &Error{Code: ErrInvalidRange, Bucket: r.Bucket, Key: r.Key}
	}
	if !hasRange {
		length = stat.Info.Size
	}
	h := w.Header()
	h.Set("ETag", stat.Info.ETag)
	h.Set("Last-Modified", stat.Info.LastModified.UTC().Format(http.TimeFormat))
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	writeObjectMetadata(h, stat.Info.Metadata)
	if !hasRange {
		writeResponseChecksum(h, r.HTTP.Header.Get("X-Amz-Checksum-Mode"), stat.Info.Checksum)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// DeleteObject answers 204 for a caller the router authorized with write,
// whether or not the key exists (API v2 4.2). The payload is read to EOF
// first, so a bad x-amz-content-sha256 or checksum header fails the request
// before the tombstone transaction.
func (o *Objects) DeleteObject(w http.ResponseWriter, r *Request) error {
	if o.Writer == nil {
		return &Error{Code: ErrNotImplemented, Message: "DeleteObject is not implemented"}
	}
	if err := objectKey(r); err != nil {
		return err
	}
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return err
	}
	err := o.Writer.DeleteObject(r.HTTP.Context(), object.DeleteRequest{
		Bucket: r.Access.Bucket, Key: r.Key, UserID: r.Caller.UserID, CredentialID: r.Caller.AccessKeyID,
	})
	if err != nil {
		return objectError(err, r.Bucket, r.Key)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func checkIfMatch(r *Request, etag string) error {
	value := r.HTTP.Header.Get("If-Match")
	if value == "" {
		return nil
	}
	for _, candidate := range strings.Split(value, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return nil
		}
	}
	return &Error{Code: ErrPreconditionFailed, Bucket: r.Bucket, Key: r.Key}
}

func writeObjectMetadata(h http.Header, metadata map[string]string) {
	contentType := metadata["content-type"]
	if contentType == "" {
		contentType = "binary/octet-stream"
	}
	h.Set("Content-Type", contentType)
	for name, header := range map[string]string{
		"cache-control": "Cache-Control", "content-disposition": "Content-Disposition",
		"content-encoding": "Content-Encoding", "content-language": "Content-Language", "expires": "Expires",
	} {
		if v := metadata[name]; v != "" {
			h.Set(header, v)
		}
	}
	for name, value := range metadata {
		if strings.HasPrefix(name, "x-amz-meta-") {
			h.Set(name, value)
		}
	}
}

var checksumHeaders = map[string]string{
	"CRC64NVME": "x-amz-checksum-crc64nvme", "CRC32": "x-amz-checksum-crc32",
	"CRC32C": "x-amz-checksum-crc32c", "SHA1": "x-amz-checksum-sha1", "SHA256": "x-amz-checksum-sha256",
}

func checksumHeaderName(algorithm string) (string, bool) {
	name, ok := checksumHeaders[strings.ToUpper(algorithm)]
	return name, ok
}

// writeResponseChecksum echoes the stored full-object checksum only when the
// client asked for it with x-amz-checksum-mode: ENABLED (API v2 2.7).
func writeResponseChecksum(h http.Header, mode string, checksum storage.Checksum) {
	if mode != "ENABLED" || checksum.Value == "" {
		return
	}
	name, ok := checksumHeaderName(checksum.Algorithm)
	if !ok {
		return
	}
	h.Set(name, checksum.Value)
	kind := checksum.Type
	if kind == "" {
		kind = "FULL_OBJECT"
	}
	h.Set("x-amz-checksum-type", kind)
}

func objectError(err error, bucket, key string) error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	code := ErrInternalError
	switch {
	case errors.Is(err, object.ErrObjectTooLarge), errors.Is(err, object.ErrMetadataTooLarge):
		code = ErrEntityTooLarge
	case errors.Is(err, object.ErrLengthMismatch):
		code = ErrIncompleteBody
	case errors.Is(err, storage.ErrBucketNotFound):
		code = ErrNoSuchBucket
	case errors.Is(err, storage.ErrNotFound):
		code = ErrNoSuchKey
	case errors.Is(err, storage.ErrAccessDenied):
		code = ErrAccessDenied
	case errors.Is(err, storage.ErrQuotaExceeded):
		code = ErrQuotaExceeded
	case errors.Is(err, storage.ErrConflict):
		code = ErrOperationAborted
	case errors.Is(err, storage.ErrUnavailable), errors.Is(err, storage.ErrFenced):
		code = ErrServiceUnavailable
	case errors.Is(err, storage.ErrCold):
		code = ErrInvalidObjectState
	case errors.Is(err, storage.ErrInvalid):
		code = ErrInvalidRequest
	}
	return &Error{Code: code, Bucket: bucket, Key: key}
}

// parseObjectRange is the S3 GetObject/HeadObject Range grammar of API v2 4.1:
// exactly one bytes=a-b, bytes=a- or bytes=-n range. A unit other than bytes is
// ignored (the whole object is served); anything else that is not satisfiable
// is InvalidRange. A closed end past the object is clamped to its last byte.
func parseObjectRange(spec string, size int64) (start, length int64, isRange, ok bool) {
	spec = strings.TrimSpace(spec)
	unit, rest, found := strings.Cut(spec, "=")
	if !found || !strings.EqualFold(unit, "bytes") {
		return 0, size, false, false
	}
	if strings.Contains(rest, ",") {
		return 0, 0, true, false
	}
	first, last, hasDash := strings.Cut(rest, "-")
	if !hasDash || first == "" && last == "" {
		return 0, 0, true, false
	}
	if first == "" {
		n, err := strconv.ParseUint(last, 10, 63)
		if err != nil || n == 0 || size == 0 {
			return 0, 0, true, false
		}
		l := min(int64(n), size)
		return size - l, l, true, true
	}
	begin, err := strconv.ParseUint(first, 10, 63)
	if err != nil || int64(begin) >= size {
		return 0, 0, true, false
	}
	if last == "" {
		return int64(begin), size - int64(begin), true, true
	}
	end, err := strconv.ParseUint(last, 10, 63)
	if err != nil || end < begin {
		return 0, 0, true, false
	}
	if end >= uint64(size) {
		end = uint64(size) - 1
	}
	return int64(begin), int64(end) - int64(begin) + 1, true, true
}
