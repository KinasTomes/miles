package s3

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/KinasTomes/miles/internal/object"
)

// copyObjectResult is the CopyObject response of API v2 4.5.
type copyObjectResult struct {
	XMLName      xml.Name `xml:"CopyObjectResult"`
	ETag         string
	LastModified string
}

// CopyObject serves API v2 4.5 for one source in the destination's dedup
// domain: the router checked write on the destination; the handler parses
// x-amz-copy-source and x-amz-metadata-directive, then checks read on the
// source (AuthorizeCopySource) before the service looks the source key up.
// ETag and checksum are the source's, kept opaque (R4).
func (o *Objects) CopyObject(w http.ResponseWriter, r *Request) error {
	if o.Writer == nil {
		return &Error{Code: ErrNotImplemented, Message: "CopyObject is not implemented"}
	}
	if err := objectKey(r); err != nil {
		return err
	}
	srcBucket, srcKey, e := parseCopySource(r.HTTP.Header.Get("X-Amz-Copy-Source"))
	if e != nil {
		e.Bucket, e.Key = r.Bucket, r.Key
		return e
	}
	var metadata map[string]string
	switch r.HTTP.Header.Get("X-Amz-Metadata-Directive") {
	case "", "COPY":
	case "REPLACE":
		metadata = RequestMetadata(r.HTTP)
		if r.HTTP.Header.Get("Content-Type") == "" {
			metadata["content-type"] = "application/octet-stream" // API v2 4.5 REPLACE default
		}
	default:
		return &Error{Code: ErrInvalidArgument, Message: "Unknown metadata directive.", Bucket: r.Bucket, Key: r.Key}
	}
	// A copy has no payload; reading it to EOF still verifies
	// x-amz-content-sha256 and any checksum header before the commit.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		return err
	}
	source, err := r.AuthorizeCopySource(srcBucket)
	if err != nil {
		return err
	}
	result, err := o.Writer.CopyObject(r.HTTP.Context(), object.CopyRequest{
		Source: source.Bucket, SourceKey: srcKey, Bucket: r.Access.Bucket, Key: r.Key, Metadata: metadata,
		ChecksumAlgorithm: strings.ToUpper(r.HTTP.Header.Get("X-Amz-Checksum-Algorithm")),
		UserID:            r.Caller.UserID, CredentialID: r.Caller.AccessKeyID,
	})
	if err != nil {
		return copyError(err, r, srcBucket, srcKey)
	}
	return WriteXML(w, copyObjectResult{ETag: result.ETag, LastModified: result.LastModified.UTC().Format(isoTimeLayout)})
}

// parseCopySource reads x-amz-copy-source: "bucket/key" with an optional
// leading "/", URL-encoded once (API v2 4.5). Versions are out of scope.
func parseCopySource(value string) (bucket, key string, e *Error) {
	path, query, _ := strings.Cut(value, "?")
	if query != "" {
		return "", "", &Error{Code: ErrNotImplemented, Message: "x-amz-copy-source with a version is not implemented."}
	}
	decoded, err := url.PathUnescape(path)
	if err != nil || !utf8.ValidString(decoded) {
		return "", "", &Error{Code: ErrInvalidArgument, Message: "Invalid copy source encoding."}
	}
	bucket, key, _ = strings.Cut(strings.TrimPrefix(decoded, "/"), "/")
	switch {
	case bucket == "" || key == "":
		return "", "", &Error{Code: ErrInvalidArgument, Message: "Copy Source must mention the source bucket and key: sourcebucket/sourcekey."}
	case len(key) > MaxKeyBytes:
		return "", "", &Error{Code: ErrKeyTooLong, Message: "The copy source key is too long."}
	}
	return bucket, key, nil
}

func copyError(err error, r *Request, srcBucket, srcKey string) error {
	invalid := func(msg string) error {
		return &Error{Code: ErrInvalidRequest, Message: msg, Bucket: r.Bucket, Key: r.Key}
	}
	switch {
	case errors.Is(err, object.ErrCopySourceTooLarge):
		return invalid(fmt.Sprintf("The specified copy source is larger than the maximum allowable size for a copy source: %d", object.MaxCopyBytes))
	case errors.Is(err, object.ErrCopyToItself):
		return invalid("This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.")
	case errors.Is(err, object.ErrCopyChecksumChange):
		return invalid("CopyObject keeps the source checksum; changing the checksum algorithm is not implemented.")
	case errors.Is(err, object.ErrCrossDomainCopy):
		return &Error{Code: ErrNotImplemented, Message: "CopyObject across dedup domains is not implemented yet.", Bucket: r.Bucket, Key: r.Key}
	case errors.Is(err, object.ErrNoSuchCopySource):
		return &Error{Code: ErrNoSuchKey, Bucket: srcBucket, Key: srcKey}
	case errors.Is(err, object.ErrNoSuchCopySourceBucket):
		return &Error{Code: ErrNoSuchBucket, Bucket: srcBucket}
	}
	return objectError(err, r.Bucket, r.Key)
}
