package badger

import (
	"context"
	"errors"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

// resolve runs in the claim/release transaction. Missing or inconsistent proof
// never authorizes releasing quota after an ambiguous acknowledgement.
func (m *Store) resolve(t *db.Txn, r s.Reservation) (*s.CommitResult, error) {
	if m.reconcile != nil {
		if err := m.reconcile(); err != nil {
			return nil, err
		}
	}
	done, err := get[outcome](t, key("outcomes", r.OpID))
	if err == nil {
		if r.State != s.ReservationCommitted {
			return nil, s.ErrUnavailable
		}
		return &s.CommitResult{Head: done.Head, AlreadyCommitted: true}, nil
	}
	if !errors.Is(err, s.ErrNotFound) {
		return nil, err
	}
	if r.State == s.ReservationCommitted {
		return nil, s.ErrUnavailable
	}
	found, err := exists(t, key("event-ops", r.OpID))
	if err != nil {
		return nil, err
	}
	if found {
		return nil, s.ErrUnavailable
	}
	return nil, nil
}
func (m *Store) ClaimReservation(ctx context.Context, r s.ReservationClaim) (s.ReservationResolution, error) {
	var out s.ReservationResolution
	err := m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, r.Writer); err != nil {
			return err
		}
		if r.ID == "" || r.OpID == "" || r.ExpectedRevision == 0 {
			return s.ErrInvalid
		}
		v, err := get[s.Reservation](t, key("reservations", r.ID))
		if err != nil {
			return err
		}
		if v.OpID != r.OpID {
			return s.ErrConflict
		}
		done, err := m.resolve(t, v)
		if err != nil {
			return err
		}
		if done != nil {
			out = s.ReservationResolution{Reservation: v, Committed: done}
			return nil
		}
		if v.State != s.ReservationActive || v.Revision != r.ExpectedRevision || v.Revision == ^uint64(0) {
			return s.ErrConflict
		}
		if r.Writer.ServiceIncarnation != v.Writer.ServiceIncarnation {
			return s.ErrFenced
		}
		if v.Writer == r.Writer {
			out.Reservation = v
			return nil
		}
		if err := m.fence(t, v.Writer); err == nil {
			return s.ErrConflict
		} else if !errors.Is(err, s.ErrFenced) {
			return err
		}
		v.Writer = r.Writer
		v.Revision++
		out.Reservation = v
		return put(t, key("reservations", v.ID), v)
	})
	return out, err
}
func (m *Store) ReleaseLogicalQuota(ctx context.Context, r s.ReservationRelease) error {
	return m.update(ctx, func(t *db.Txn) error {
		if err := m.fence(t, r.Writer); err != nil {
			return err
		}
		if r.ID == "" || r.OpID == "" || r.ExpectedRevision == 0 {
			return s.ErrInvalid
		}
		v, err := get[s.Reservation](t, key("reservations", r.ID))
		if err != nil {
			return err
		}
		if v.OpID != r.OpID {
			return s.ErrConflict
		}
		if v.Writer != r.Writer {
			return s.ErrFenced
		}
		done, err := m.resolve(t, v)
		if err != nil {
			return err
		}
		if done != nil {
			return s.ErrConflict
		}
		if v.State == s.ReservationReleased {
			return nil
		}
		if v.State != s.ReservationActive || v.Revision != r.ExpectedRevision || v.Revision == ^uint64(0) {
			return s.ErrConflict
		}
		q, err := get[s.LogicalQuota](t, key("quotas", v.OwnerID))
		if err != nil {
			return err
		}
		if q.Reserved < v.Bytes || q.Staged < v.StagedBytes {
			return s.ErrInvalid
		}
		q.Reserved -= v.Bytes
		q.Staged -= v.StagedBytes
		v.State = s.ReservationReleased
		v.Revision++
		if err := put(t, key("quotas", q.OwnerID), q); err != nil {
			return err
		}
		return put(t, key("reservations", v.ID), v)
	})
}
