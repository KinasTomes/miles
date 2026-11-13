package s3

import (
	"context"
	"errors"
	"log/slog"

	"github.com/KinasTomes/miles/internal/storage"
)

// failureDiagnostic only records fixed, non-sensitive classifications. It
// deliberately does not retain raw backend errors, credentials or object keys.
type failureDiagnostic struct {
	stage     string
	class     string
	operation Operation
	logged    bool
	debug     bool
}

// RecordFailure records a constant stage name and a safe error class from the
// server wiring layer. Never pass a user-controlled string as the stage.
func (r *Request) RecordFailure(stage string, err error) { r.recordFailure(stage, err) }

func (r *Request) recordFailure(stage string, err error) {
	if err == nil || r.failure == nil {
		return
	}
	r.failure.stage = stage
	r.failure.class = safeFailureClass(err)
}

// recordStreamAbort records a structured server failure for an active streaming
// response whose headers were already committed, prior to aborting the handler via
// panic(http.ErrAbortHandler).
func (r *Request) recordStreamAbort(stage string, err error) {
	if r == nil || err == nil {
		return
	}
	r.recordFailure(stage, err)
	class := "unknown"
	if r.failure != nil && r.failure.class != "" {
		class = r.failure.class
	} else {
		class = safeFailureClass(err)
	}
	op := r.Route.Op
	if op == "" {
		op = GetObject
	}
	if r.failure != nil {
		r.failure.logged = true
	}
	ctx := r.HTTP.Context()
	slog.ErrorContext(ctx, "s3 server failure",
		"request_id", r.ID,
		"node_id", r.nodeID,
		"method", r.HTTP.Method,
		"operation", string(op),
		"stage", stage,
		"failure_class", class,
	)
}

func safeFailureClass(err error) string {
	switch {
	case errors.Is(err, storage.ErrR1Busy):
		return "r1_gate_busy"
	case errors.Is(err, storage.ErrFenced):
		return "fenced"
	case errors.Is(err, storage.ErrClockUncertain):
		return "clock_uncertainty_exceeded"
	case errors.Is(err, storage.ErrUnavailable):
		return "backend_unavailable"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, storage.ErrConflict):
		return "metadata_conflict"
	case errors.Is(err, storage.ErrQuotaExceeded):
		return "quota_exceeded"
	case errors.Is(err, storage.ErrInvalid):
		return "invalid_state"
	default:
		return "unknown"
	}
}
