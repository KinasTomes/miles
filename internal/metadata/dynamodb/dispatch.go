package dynamodb

import (
	"context"
	"errors"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// dispatchDeadline checks remaining authority AFTER potentially slow manifest
// verification and before every explicit final send. Native transaction predicates
// still own atomic permission/revision checks; this is not a server-clock LP.
func (m *Store) dispatchDeadline(ctx context.Context, r s.CommitRequest) (time.Time, error) {
	t := m.transaction(ctx)
	if r.Producer != nil {
		if err := (multipartTx{t}).Admit(*r.Producer); err != nil {
			return time.Time{}, err
		}
	}
	if err := m.fence(t, r.Writer); err != nil {
		return time.Time{}, err
	}
	auth, err := m.authorize(t, r.CredentialID, r.Head.Object.Bucket)
	if err != nil {
		return time.Time{}, err
	}
	if auth.UserID != r.UserID || !auth.Write {
		return time.Time{}, s.ErrAccessDenied
	}
	if auth.Bucket.ID != r.Head.Object.BucketID {
		return time.Time{}, s.ErrBucketNotFound
	}
	if auth.Bucket.Domain != r.Head.Domain {
		return time.Time{}, s.ErrInvalid
	}
	reservation, err := get[s.Reservation](t, key("reservations", r.ReservationID))
	if err != nil {
		return time.Time{}, err
	}
	if reservation.OpID != r.Head.OpID || reservation.Object != r.Head.Object || reservation.OwnerID != auth.Bucket.OwnerID {
		return time.Time{}, s.ErrConflict
	}
	if reservation.State != s.ReservationActive || reservation.Revision != r.ReservationRevision {
		return time.Time{}, s.ErrConflict
	}
	if reservation.Writer != r.Writer {
		return time.Time{}, s.ErrFenced
	}
	deadline := reservation.Deadline
	if r.Receipt != nil && r.Receipt.ExpiresAt.Before(deadline) {
		deadline = r.Receipt.ExpiresAt
	}
	if !r.Head.Deleted {
		if err := m.protect(t, r.Writer, r.Head.ManifestID); err != nil {
			return time.Time{}, err
		}
	}
	// This is deliberately last: authority/metadata reads can also cross expiry.
	if !m.now().Before(deadline) {
		return deadline, s.ErrFenced
	}
	return deadline, nil
}

func (m *Store) publishAttempt(ctx context.Context, r s.CommitRequest, input *ddb.TransactWriteItemsInput, k Key, pending Item) error {
	deadline, err := m.dispatchDeadline(ctx, r)
	if err == nil && !m.now().Before(deadline) {
		err = s.ErrFenced
	}
	if err != nil {
		if errors.Is(err, s.ErrAccessDenied) || errors.Is(err, s.ErrFenced) || errors.Is(err, s.ErrBucketNotFound) || errors.Is(err, s.ErrConflict) {
			_, _, closedErr := m.rejectPending(ctx, r, err)
			return closedErr
		}
		return err
	}
	// SDK retries would create hidden sends after the dispatch guard/deadline.
	// Unknown sends are reconciled explicitly using the persisted exact request.
	if a, ok := m.authority.(*SharedAuthority); ok {
		// Fresh conservative dispatch check after all slow metadata work. Native
		// input still condition-checks the observed gate/lease closure revisions.
		t := m.transaction(ctx)
		if err := a.Check(ctx, r.Writer, r1Observe(t)); err != nil {
			return err
		}
	}
	_, err = m.api.TransactWriteItems(ctx, input, func(o *ddb.Options) { o.Retryer = aws.NopRetryer{} })
	if isKnownCancellation(err) {
		if clearErr := m.retireAttempt(ctx, k, pending); clearErr != nil {
			return clearErr
		}
		return s.ErrConflict
	}
	return backend(err)
}
