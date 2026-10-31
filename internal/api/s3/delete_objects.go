package s3

import (
	"encoding/xml"
	"net/http"

	"github.com/KinasTomes/miles/internal/object"
)

const (
	// maxDeleteKeys is the S3 DeleteObjects batch limit (API v2 2.5).
	maxDeleteKeys = 1000
	// maxDeleteBodyBytes bounds the Delete document: 1000 keys of 1024 bytes
	// even when every byte is sent as a 6-byte XML entity.
	maxDeleteBodyBytes = 8 << 20
)

// deleteRequest is the DeleteObjects body of API v2 4.6. VersionId and the
// conditional elements are decoded only to reject them: ignoring one would
// delete what the client meant to keep.
type deleteRequest struct {
	XMLName xml.Name `xml:"Delete"`
	Objects []struct {
		Key              string
		VersionID        *string `xml:"VersionId"`
		ETag             *string
		LastModifiedTime *string
		Size             *string
	} `xml:"Object"`
	Quiet bool
}

type deleteResult struct {
	XMLName xml.Name        `xml:"DeleteResult"`
	Deleted []deletedObject `xml:"Deleted"`
	Errors  []deleteError   `xml:"Error"`
}
type deletedObject struct{ Key string }
type deleteError struct{ Key, Code, Message string }

// DeleteObjects runs DeleteObject for each key of the batch (API v2 4.6). The
// whole document, its keys and its Content-MD5 or checksum are validated
// before the first tombstone; then each key commits on its own, so a failure
// mid-batch is that key's Error, never a rollback. Quiet drops Deleted only.
func (o *Objects) DeleteObjects(w http.ResponseWriter, r *Request) error {
	if o.Writer == nil {
		return &Error{Code: ErrNotImplemented, Message: "DeleteObjects is not implemented"}
	}
	h := r.HTTP.Header
	if _, _, sent := requestChecksum(h); !sent && h.Get("X-Amz-Trailer") == "" && len(h.Values("Content-Md5")) == 0 {
		return &Error{Code: ErrInvalidRequest, Message: "Missing required header for this request: Content-MD5 or x-amz-checksum-*.", Bucket: r.Bucket}
	}
	var doc deleteRequest
	if err := r.Body.DecodeXML(&doc, maxDeleteBodyBytes); err != nil {
		return err
	}
	if len(doc.Objects) == 0 || len(doc.Objects) > maxDeleteKeys {
		return &Error{Code: ErrMalformedXML, Bucket: r.Bucket}
	}
	for _, obj := range doc.Objects {
		switch {
		case obj.Key == "":
			return &Error{Code: ErrMalformedXML, Bucket: r.Bucket}
		case len(obj.Key) > MaxKeyBytes:
			return &Error{Code: ErrKeyTooLong, Bucket: r.Bucket, Key: obj.Key}
		case obj.VersionID != nil:
			return &Error{Code: ErrNotImplemented, Message: "DeleteObjects with VersionId is not implemented.", Bucket: r.Bucket, Key: obj.Key}
		case obj.ETag != nil || obj.LastModifiedTime != nil || obj.Size != nil:
			return &Error{Code: ErrNotImplemented, Message: "Conditional DeleteObjects is not implemented.", Bucket: r.Bucket, Key: obj.Key}
		}
	}
	out := deleteResult{}
	for _, obj := range doc.Objects {
		err := o.Writer.DeleteObject(r.HTTP.Context(), object.DeleteRequest{
			Bucket: r.Access.Bucket, Key: obj.Key, UserID: r.Caller.UserID, CredentialID: r.Caller.AccessKeyID,
		})
		if err != nil {
			e := objectError(err, r.Bucket, obj.Key).(*Error)
			out.Errors = append(out.Errors, deleteError{Key: obj.Key, Code: string(e.Code), Message: e.message()})
		} else if !doc.Quiet {
			out.Deleted = append(out.Deleted, deletedObject{Key: obj.Key})
		}
	}
	return WriteXML(w, out)
}
