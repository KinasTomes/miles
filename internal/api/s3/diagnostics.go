package s3

import (
	"context"
	"errors"

	"github.com/KinasTomes/miles/internal/storage"
)

// failureDiagnostic only records fixed, non-sensitive classifications. It
// deliberately does not retain raw backend errors, credentials or object keys.
type failureDiagnostic struct {
	stage     string
	class     string
	operation Operation
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

func safeFailureClass(err error) string {
	switch {
	case errors.Is(err, storage.ErrR1Busy):
		return "r1_gate_busy"
	case errors.Is(err, storage.ErrFenced):
		return "fenced"
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
