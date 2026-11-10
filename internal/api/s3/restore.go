package s3

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

// ObjectRestorer is the RestoreObject port (API v2 4.4). The object service
// implements it beside ObjectReader; a reader without it leaves the operation
// NotImplemented.
type ObjectRestorer interface {
	RestoreObject(ctx context.Context, r object.RestoreRequest) (object.RestoreResult, error)
}

// maxRestoreBodyBytes bounds the RestoreRequest document, which carries a few
// short elements.
const maxRestoreBodyBytes = 64 << 10

// restoreRequest is the RestoreRequest body of API v2 4.4. The SELECT and
// output elements are decoded only to reject them: ignoring one would answer a
// different request than the client sent.
type restoreRequest struct {
	XMLName              xml.Name `xml:"RestoreRequest"`
	Days                 *int
	GlacierJobParameters *struct{ Tier string }
	Type                 *string
	SelectParameters     *struct{}
	OutputLocation       *struct{}
}

// RestoreObject starts restoring the object's GLACIER packs with the caller's
// Days and Tier and returns without waiting for the archive: 202 when it
// started at least one, 409 RestoreAlreadyInProgress when none was started but
// one is running, 200 when every archive pack already has a copy lasting Days
// (API v2 4.4). A Warm/GLACIER_IR-only object is 400 InvalidRequest.
func (o *Objects) RestoreObject(w http.ResponseWriter, r *Request) error {
	restorer, ok := o.Reader.(ObjectRestorer)
	if !ok {
		return &Error{Code: ErrNotImplemented, Message: "RestoreObject is not implemented"}
	}
	if err := objectKey(r); err != nil {
		return err
	}
	var doc restoreRequest
	if err := r.Body.DecodeXML(&doc, maxRestoreBodyBytes); err != nil {
		return err
	}
	if doc.Type != nil || doc.SelectParameters != nil || doc.OutputLocation != nil {
		return &Error{Code: ErrNotImplemented, Message: "SELECT restore and OutputLocation are not implemented.", Bucket: r.Bucket, Key: r.Key}
	}
	request := storage.RestoreRequest{Tier: "Standard"}
	if doc.Days != nil {
		request.Days = *doc.Days
	}
	if doc.GlacierJobParameters != nil {
		request.Tier = doc.GlacierJobParameters.Tier
	}
	if doc.Days == nil || request.Validate() != nil {
		return &Error{Code: ErrInvalidRequest, Message: fmt.Sprintf("RestoreRequest needs Days from 1 to %d and a GlacierJobParameters Tier of Expedited, Standard or Bulk.", storage.MaxRestoreDays), Bucket: r.Bucket, Key: r.Key}
	}
	out, err := restorer.RestoreObject(r.HTTP.Context(), object.RestoreRequest{
		Bucket: r.Bucket, BucketID: r.Access.Bucket.ID, Key: r.Key, CredentialID: r.Caller.AccessKeyID, Days: request.Days, Tier: request.Tier,
	})
	switch {
	case errors.Is(err, object.ErrNotArchived):
		return &Error{Code: ErrInvalidRequest, Message: "The object has no GLACIER data to restore; STANDARD and GLACIER_IR data is readable now.", Bucket: r.Bucket, Key: r.Key}
	case err != nil:
		r.recordFailure("object_restore", err)
		return objectError(err, r.Bucket, r.Key)
	case out.Initiated > 0:
		w.WriteHeader(http.StatusAccepted)
	case out.InProgress > 0:
		return &Error{Code: ErrRestoreAlreadyInProgress, Bucket: r.Bucket, Key: r.Key}
	default:
		w.WriteHeader(http.StatusOK)
	}
	return nil
}
