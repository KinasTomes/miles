package dynamodb

import (
	"context"
	"errors"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// CloseCommitAttempt is an explicit trusted reconciliation operation, not a
// signing-credential permission check or a missing-outcome inference. It fences
// registration/final publication through the SAME operation row before cleanup.
// A replacement writer must prove the original authority is closed. Unknown CAS
// acknowledgements remain unavailable; callers must not release roots on error.
// This does not clear coordination.Writer's unknown state or release R1 pins.
func (m *Store) CloseCommitAttempt(ctx context.Context, request s.ReservationClaim) (s.ReservationResolution, error) {
	var result s.ReservationResolution
	if request.ID == "" || request.OpID == "" || request.ExpectedRevision == 0 {
		return result, s.ErrInvalid
	}
	err := m.update(ctx, func(t *transaction) error {
		result = s.ReservationResolution{}
		r, err := get[s.Reservation](t, key("reservations", request.ID))
		if err != nil {
			return err
		}
		if r.OpID != request.OpID {
			return s.ErrConflict
		}
		row, err := t.load(key("outcomes", r.OpID))
		if err != nil {
			return err
		}
		done, err := get[outcome](t, key("outcomes", r.OpID))
		if err == nil {
			if r.State != s.ReservationCommitted || done.Head.Object != r.Object {
				return s.ErrUnavailable
			}
			result = s.ReservationResolution{Reservation: r, Committed: &s.CommitResult{Head: done.Head, AlreadyCommitted: true}}
			return nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		if err := m.fence(t, request.Writer); err != nil {
			return err
		}
		if request.Writer.ServiceIncarnation != r.Writer.ServiceIncarnation {
			return s.ErrFenced
		}
		if request.Writer != r.Writer {
			if err := m.fence(t, r.Writer); err == nil {
				return s.ErrConflict
			} else if !errors.Is(err, s.ErrFenced) {
				return err
			}
		}
		if operationClosed(row.after) {
			result.Reservation = r
			return nil
		}
		if r.State != s.ReservationActive || r.Revision != request.ExpectedRevision {
			return s.ErrConflict
		}
		if err := closeOperation(row, r); err != nil {
			return err
		}
		result.Reservation = r
		return nil
	})
	if err != nil {
		return s.ReservationResolution{}, err
	}
	return result, nil
}

func operationClosed(item Item) bool {
	state, ok := item["state"].(*types.AttributeValueMemberS)
	return ok && state != nil && state.Value == "Closed"
}

func closeOperation(row *observedRow, r s.Reservation) error {
	if row.after == nil || row.after["result"] != nil || row.after["event_id"] != nil {
		return s.ErrUnavailable
	}
	binding, ok := row.after["reservation_id"].(*types.AttributeValueMemberS)
	if !ok || binding == nil || binding.Value != r.ID {
		return s.ErrUnavailable
	}
	revision, ok := row.before["record_revision"].(*types.AttributeValueMemberS)
	if !ok || revision == nil || revision.Value == "" {
		return s.ErrUnavailable
	}
	// Final dispatch and attempt registration both compare this row's revision.
	// CAS closure wins before either, or loses to the retained committed result.
	row.after = copyItem(row.after)
	delete(row.after, "pending_request")
	delete(row.after, "pending_sha256")
	row.after["state"] = text("Closed")
	row.after["closed_attempt"] = text(revision.Value)
	row.dirty = true
	return nil
}

func (m *Store) rejectPending(ctx context.Context, r s.CommitRequest, cause error) (s.CommitResult, bool, error) {
	resolution, err := m.CloseCommitAttempt(ctx, s.ReservationClaim{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: r.ReservationRevision, Writer: r.Writer})
	if err != nil {
		return s.CommitResult{}, true, err
	}
	if resolution.Committed != nil {
		return *resolution.Committed, true, nil
	}
	return s.CommitResult{}, true, cause
}
