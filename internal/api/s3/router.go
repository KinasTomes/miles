package s3

import (
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

// Router is the S3 API http.Handler (API v2 3): it gives every request an ID,
// resolves its operation, checks its SigV4 signature, authorizes the caller
// on the bucket (API v2 2.2.3) and hands the request with its payload to the
// handler of the operation, before anything reads the body, an object or the
// cache.
type Router struct {
	Region      string // region of every credential scope
	Credentials Credentials
	Access      Access
	Now         func() time.Time // server clock for X-Amz-Date and presigned expiry
	Handlers    map[Operation]Handler
}

// Request is an S3 request the router admitted: resolved, authenticated,
// authorized on its bucket, with its body as a Payload.
type Request struct {
	Route
	HTTP   *http.Request
	ID     string
	Caller Caller
	// Access is the authorization snapshot the router admitted the request
	// on: the bucket incarnation, owner and domain, and the caller's rights.
	// It is zero for ListBuckets and for CreateBucket of a free name. Writes
	// carry its bucket ID and credential into their metadata transaction,
	// which checks them again (API v2 2.2.3).
	Access storage.Authorization
	Body   *Payload
	access Access
	region string
}

// Region is the gateway region every credential scope uses; bucket handlers
// check LocationConstraint against it and echo it back (API v2 4.2).
func (r *Request) Region() string { return r.region }

// Handler serves one operation. An *Error it returns becomes the S3 error
// response; any other error is logged and answered InternalError. It returns
// an error only before writing the response; once the status is sent it
// aborts the stream with panic(http.ErrAbortHandler) (API v2 3).
type Handler func(w http.ResponseWriter, r *Request) error

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id := NewRequestID()
	setCommonHeaders(w.Header(), id)
	err := rt.serve(w, r, id)
	if err == nil {
		return
	}
	var e *Error
	if !errors.As(err, &e) {
		slog.ErrorContext(r.Context(), "s3 request failed", "request_id", id, "err", err)
		e = &Error{Code: ErrInternalError}
	}
	if err := WriteError(w, r, e, id); err != nil {
		slog.WarnContext(r.Context(), "s3 error response not delivered", "request_id", id, "code", e.Code, "err", err)
	}
}

func (rt *Router) serve(w http.ResponseWriter, r *http.Request, id string) error {
	route, e := Resolve(r)
	if e != nil {
		return e
	}
	caller, s, err := rt.authenticate(r, route.Query)
	if err != nil {
		return err
	}
	access, err := rt.authorize(r.Context(), route, caller)
	if err != nil {
		return err
	}
	h, ok := rt.Handlers[route.Op]
	if !ok {
		return &Error{Code: ErrNotImplemented, Message: string(route.Op) + " is not implemented yet", Bucket: route.Bucket, Key: route.Key}
	}
	return h(w, &Request{Route: route, HTTP: r, ID: id, Caller: caller, Access: access, Body: newPayload(r, route.Op, s), access: rt.Access, region: rt.Region})
}
