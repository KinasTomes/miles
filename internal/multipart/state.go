package multipart

import (
	"fmt"
	"time"

	"github.com/KinasTomes/miles/internal/api/s3"
)

// LeaseGrace extends a completion lease before another worker or a retry
// may take the upload over (API v2 4.3).
const LeaseGrace = 5 * time.Second

// State is the lifecycle state of an upload in Uploads (API v2 4.3).
type State string

const (
	Active     State = "active"
	Completing State = "completing"
	Completed  State = "completed"
	Aborting   State = "aborting"
	Aborted    State = "aborted"
)

// Status is what Uploads holds about the lifecycle of an upload.
type Status struct {
	State            State
	ExpiresAt        time.Time // business expiry: Create + 24 h
	LeaseExpiresAt   time.Time // Completing: completion worker lease
	ReceiptExpiresAt time.Time // Completed: committed_at + 7 days
	InputDigest      string    // Completing, Completed: digest of the frozen input
}

// Action is what an operation does once Dispatch let it through.
type Action uint8

const (
	// Proceed runs the operation; for Complete on Active that is the freeze
	// and validation rows 6-13.
	Proceed Action = iota
	// Takeover takes the completion fence of a Completing upload past its
	// lease and grace and resumes its frozen input with the same
	// completion_op_id, even after expires_at.
	Takeover
	// Replay answers the receipt of a Completed upload, even after
	// expires_at.
	Replay
)

// Dispatch is the state table of API v2 4.3.1 for op on an upload in st at
// now; digest is the input digest of a Complete and is ignored for the
// other operations. It runs after the binding check (s3.Request.CheckUpload)
// and Normalize. A publish that loses its CAS to a freeze or an abort is
// answered by Dispatch on the state that won: Completing gives
// OperationAborted, Aborting NoSuchUpload.
func Dispatch(op s3.Operation, st Status, digest string, now time.Time) (Action, error) {
	noSuchUpload := &s3.Error{Code: s3.ErrNoSuchUpload}
	busy := &s3.Error{Code: s3.ErrOperationAborted, Message: "the upload is completing; retry later"}
	switch op {
	case s3.CompleteMultipartUpload, s3.UploadPart, s3.AbortMultipartUpload, s3.ListParts:
	default:
		return 0, fmt.Errorf("multipart: %s has no upload state", op)
	}
	switch st.State {
	case Active:
		if !now.Before(st.ExpiresAt) {
			return 0, noSuchUpload
		}
		return Proceed, nil
	case Completing:
		switch {
		case op == s3.ListParts:
			return Proceed, nil
		case op != s3.CompleteMultipartUpload:
			return 0, busy
		case digest != st.InputDigest:
			return 0, &s3.Error{Code: s3.ErrInvalidRequest, Message: "the upload is completing with another part list"}
		case now.Before(st.LeaseExpiresAt.Add(LeaseGrace)):
			return 0, busy
		}
		return Takeover, nil
	case Completed:
		switch {
		case !now.Before(st.ReceiptExpiresAt):
			return 0, noSuchUpload
		case op == s3.AbortMultipartUpload:
			return 0, &s3.Error{Code: s3.ErrOperationAborted, Message: "the upload is completed"}
		case op != s3.CompleteMultipartUpload:
			return 0, noSuchUpload
		case digest != st.InputDigest:
			return 0, &s3.Error{Code: s3.ErrInvalidRequest, Message: "the upload was completed with another part list"}
		}
		return Replay, nil
	case Aborting, Aborted:
		return 0, noSuchUpload
	}
	return 0, fmt.Errorf("multipart: unknown upload state %q", st.State)
}
