package dynamodb

import (
	"context"
	"errors"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ReconcileR1 settles a closed writer's original commit under the same CAS as
// attempt closure. It neither grants publication rights nor releases pins/I/O.
func (m *Store) ReconcileR1(ctx context.Context, f s.WriterFence) error {
	return m.update(ctx, func(t *transaction) error {
		l, err := r1Read[s.R1Lease](t, r1LeaseKey(f.LeaseID))
		if err != nil {
			return err
		}
		if l.Fence != f || l.State != "Closed" {
			return s.ErrFenced
		}
		if len(l.Pending) == 0 {
			return nil
		}
		if l.Kind != "writer" || l.OpID == "" || len(l.Pending) != 1 || l.Pending[0] != "commit/"+l.OpID {
			return s.ErrR1Busy
		}
		row, err := t.load(key("outcomes", l.OpID))
		if err != nil {
			return err
		}
		binding, ok := row.after["reservation_id"].(*types.AttributeValueMemberS)
		if !ok || binding == nil || binding.Value == "" {
			return s.ErrUnavailable
		}
		r, err := get[s.Reservation](t, key("reservations", binding.Value))
		if err != nil {
			return err
		}
		if r.Writer != f || r.OpID != l.OpID || r.Object != l.Object {
			return s.ErrFenced
		}
		done, err := get[outcome](t, key("outcomes", l.OpID))
		if err == nil {
			if row.after["pending_request"] != nil || operationClosed(row.after) || r.State != s.ReservationCommitted || done.Head.OpID != l.OpID || done.Head.Object != l.Object {
				return s.ErrUnavailable
			}
		} else if errors.Is(err, s.ErrNotFound) {
			if r.State != s.ReservationActive && r.State != s.ReservationReleased || r.Revision == ^uint64(0) {
				return s.ErrUnavailable
			}
			// Absence alone is not proof. This CAS fences every saved final send
			// before quota or the producer's terminal receipt may change.
			if err := closeOperation(row, r); err != nil {
				return err
			}
			// Multipart recovery owns its upload/staging reservation lifecycle.
			if r.State == s.ReservationActive && r.UploadID == "" {
				if err := releaseReservedQuota(t, r); err != nil {
					return err
				}
			}
		} else {
			return err
		}
		l.Pending = nil
		l.Revision++
		if err := m.recordRecoveryClosure(t, l); err != nil {
			return err
		}
		return r1Put(t, r1LeaseKey(f.LeaseID), l)
	})
}
