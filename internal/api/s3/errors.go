package s3

import (
	"crypto/rand"
	"encoding/xml"
	"fmt"
	"net/http"
	"strconv"
)

// ErrorCode is an S3 error code of API v2 2.4.
type ErrorCode string

const (
	ErrAccessDenied              ErrorCode = "AccessDenied"
	ErrInvalidAccessKeyID        ErrorCode = "InvalidAccessKeyId"
	ErrSignatureDoesNotMatch     ErrorCode = "SignatureDoesNotMatch"
	ErrRequestTimeTooSkewed      ErrorCode = "RequestTimeTooSkewed"
	ErrInvalidObjectState        ErrorCode = "InvalidObjectState"
	ErrNoSuchBucket              ErrorCode = "NoSuchBucket"
	ErrNoSuchKey                 ErrorCode = "NoSuchKey"
	ErrNoSuchUpload              ErrorCode = "NoSuchUpload"
	ErrMethodNotAllowed          ErrorCode = "MethodNotAllowed"
	ErrBucketAlreadyOwnedByYou   ErrorCode = "BucketAlreadyOwnedByYou"
	ErrBucketAlreadyExists       ErrorCode = "BucketAlreadyExists"
	ErrInvalidRequest            ErrorCode = "InvalidRequest"
	ErrRestoreAlreadyInProgress  ErrorCode = "RestoreAlreadyInProgress"
	ErrOperationAborted          ErrorCode = "OperationAborted"
	ErrQuotaExceeded             ErrorCode = "QuotaExceeded"
	ErrBucketNotEmpty            ErrorCode = "BucketNotEmpty"
	ErrInvalidBucketName         ErrorCode = "InvalidBucketName"
	ErrKeyTooLong                ErrorCode = "KeyTooLongError"
	ErrInvalidArgument           ErrorCode = "InvalidArgument"
	ErrMalformedXML              ErrorCode = "MalformedXML"
	ErrBadDigest                 ErrorCode = "BadDigest"
	ErrXAmzContentSHA256Mismatch ErrorCode = "XAmzContentSHA256Mismatch"
	ErrIncompleteBody            ErrorCode = "IncompleteBody"
	ErrInvalidPart               ErrorCode = "InvalidPart"
	ErrInvalidPartOrder          ErrorCode = "InvalidPartOrder"
	ErrEntityTooSmall            ErrorCode = "EntityTooSmall"
	ErrEntityTooLarge            ErrorCode = "EntityTooLarge"
	ErrInvalidRange              ErrorCode = "InvalidRange"
	ErrPreconditionFailed        ErrorCode = "PreconditionFailed"
	ErrNotImplemented            ErrorCode = "NotImplemented"
	ErrInternalError             ErrorCode = "InternalError"
	ErrSlowDown                  ErrorCode = "SlowDown"
	ErrServiceUnavailable        ErrorCode = "ServiceUnavailable"
)

var errorCodes = map[ErrorCode]struct {
	status  int
	message string
}{
	ErrAccessDenied:              {http.StatusForbidden, "Access Denied"},
	ErrInvalidAccessKeyID:        {http.StatusForbidden, "The access key ID you provided does not exist in our records."},
	ErrSignatureDoesNotMatch:     {http.StatusForbidden, "The request signature we calculated does not match the signature you provided."},
	ErrRequestTimeTooSkewed:      {http.StatusForbidden, "The difference between the request time and the server's time is too large."},
	ErrInvalidObjectState:        {http.StatusForbidden, "The operation is not valid for the object's storage class."},
	ErrNoSuchBucket:              {http.StatusNotFound, "The specified bucket does not exist."},
	ErrNoSuchKey:                 {http.StatusNotFound, "The specified key does not exist."},
	ErrNoSuchUpload:              {http.StatusNotFound, "The specified upload does not exist."},
	ErrMethodNotAllowed:          {http.StatusMethodNotAllowed, "The specified method is not allowed against this resource."},
	ErrBucketAlreadyOwnedByYou:   {http.StatusConflict, "Your previous request to create the named bucket succeeded and you already own it."},
	ErrBucketAlreadyExists:       {http.StatusConflict, "The requested bucket name is not available."},
	ErrInvalidRequest:            {http.StatusBadRequest, "Invalid request."},
	ErrRestoreAlreadyInProgress:  {http.StatusConflict, "Object restore is already in progress."},
	ErrOperationAborted:          {http.StatusConflict, "A conflicting operation is currently in progress against this resource."},
	ErrQuotaExceeded:             {http.StatusForbidden, "The logical storage quota of the bucket owner is exceeded."},
	ErrBucketNotEmpty:            {http.StatusConflict, "The bucket you tried to delete is not empty."},
	ErrInvalidBucketName:         {http.StatusBadRequest, "The specified bucket is not valid."},
	ErrKeyTooLong:                {http.StatusBadRequest, "Your key is too long."},
	ErrInvalidArgument:           {http.StatusBadRequest, "Invalid argument."},
	ErrMalformedXML:              {http.StatusBadRequest, "The XML you provided was not well-formed or did not validate against our published schema."},
	ErrBadDigest:                 {http.StatusBadRequest, "The Content-MD5 you specified did not match what we received."},
	ErrXAmzContentSHA256Mismatch: {http.StatusBadRequest, "The provided 'x-amz-content-sha256' header does not match what was computed."},
	ErrIncompleteBody:            {http.StatusBadRequest, "You did not provide the number of bytes specified by the Content-Length HTTP header."},
	ErrInvalidPart:               {http.StatusBadRequest, "One or more of the specified parts could not be found."},
	ErrInvalidPartOrder:          {http.StatusBadRequest, "The list of parts was not in ascending order."},
	ErrEntityTooSmall:            {http.StatusBadRequest, "Your proposed upload is smaller than the minimum allowed object size."},
	ErrEntityTooLarge:            {http.StatusBadRequest, "Your proposed upload exceeds the maximum allowed object size."},
	ErrInvalidRange:              {http.StatusRequestedRangeNotSatisfiable, "The requested range is not satisfiable."},
	ErrPreconditionFailed:        {http.StatusPreconditionFailed, "At least one of the preconditions you specified did not hold."},
	ErrNotImplemented:            {http.StatusNotImplemented, "A header or query you provided implies functionality that is not implemented."},
	ErrInternalError:             {http.StatusInternalServerError, "We encountered an internal error. Please try again."},
	ErrSlowDown:                  {http.StatusServiceUnavailable, "Please reduce your request rate."},
	ErrServiceUnavailable:        {http.StatusServiceUnavailable, "Service is unable to handle request."},
}

// Error is an S3 error response. An empty Message means the default message
// of Code. Bucket and Key are echoed as BucketName and Key when set.
type Error struct {
	Code    ErrorCode
	Message string
	Bucket  string
	Key     string
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.message() }

// Status is the HTTP status of Code; an unknown code is 500.
func (e *Error) Status() int {
	if c, ok := errorCodes[e.Code]; ok {
		return c.status
	}
	return http.StatusInternalServerError
}

func (e *Error) message() string {
	if e.Message != "" {
		return e.Message
	}
	return errorCodes[e.Code].message
}

type errorBody struct {
	XMLName    xml.Name `xml:"Error"`
	Code       ErrorCode
	Message    string
	Key        string `xml:",omitempty"`
	BucketName string `xml:",omitempty"`
	RequestID  string `xml:"RequestId"`
}

// WriteError writes e as the S3 XML error with the common response headers
// of API v2 2.3. A HEAD response carries status and headers only, never a
// body (API v2 4.2 HeadObject). Invalid XML characters in a key are written
// as U+FFFD.
//
// The returned error comes from encoding or writing the body, usually a
// client that went away. The status line is already sent by then, so the
// caller can only log it with requestID; it must not write another response.
func WriteError(w http.ResponseWriter, r *http.Request, e *Error, requestID string) error {
	h := w.Header()
	h.Set("Server", "DedupStorageGateway")
	h.Set("X-Amz-Request-Id", requestID)
	h.Set("X-Amz-Id-2", requestID)
	if r.Method == http.MethodHead {
		w.WriteHeader(e.Status())
		return nil
	}
	body, err := xml.Marshal(errorBody{
		Code:       e.Code,
		Message:    e.message(),
		Key:        e.Key,
		BucketName: e.Bucket,
		RequestID:  requestID,
	})
	if err != nil {
		w.WriteHeader(e.Status())
		return fmt.Errorf("s3: encode %s error: %w", e.Code, err)
	}
	body = append([]byte(xml.Header), body...)
	h.Set("Content-Type", "application/xml")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(e.Status())
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("s3: write %s error: %w", e.Code, err)
	}
	return nil
}

// NewRequestID returns a request ID in the S3 form: 16 upper-case hex digits.
// It has no error path: since Go 1.24 crypto/rand.Read never returns an
// error and crashes the process if the system entropy source fails.
func NewRequestID() string {
	var b [8]byte
	rand.Read(b[:])
	return fmt.Sprintf("%X", b[:])
}
