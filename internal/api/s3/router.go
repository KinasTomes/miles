package s3

import (
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Router is the S3 API http.Handler (API v2 3): it gives every request an ID,
// resolves its operation, checks its SigV4 signature and hands it with its
// payload to the handler of the operation. Bucket authorization (API v2
// 2.2.1) comes after it, before the handler reads the body.
type Router struct {
	Region      string // region of every credential scope
	Credentials Credentials
	Now         func() time.Time // server clock for X-Amz-Date and presigned expiry
	Handlers    map[Operation]Handler
}

// Request is an S3 request the router admitted: resolved, authenticated, with
// its body as a Payload.
type Request struct {
	Route
	HTTP   *http.Request
	ID     string
	Caller Caller
	Body   *Payload
}

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
	h, ok := rt.Handlers[route.Op]
	if !ok {
		return &Error{Code: ErrNotImplemented, Message: string(route.Op) + " is not implemented yet", Bucket: route.Bucket, Key: route.Key}
	}
	return h(w, &Request{Route: route, HTTP: r, ID: id, Caller: caller, Body: newPayload(r, route.Op, s)})
}
