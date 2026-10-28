package dynamodb

import (
	"context"
	"errors"
	s "github.com/KinasTomes/miles/internal/storage"
)

func (m *Store) ReserveLogicalQuota(ctx context.Context, r s.Reservation) error {
	return m.update(ctx, func(t *transaction) error {
		if err := m.fence(t, r.Writer); err != nil {
			return err
		}
		if err := r.Object.Validate(); err != nil {
			return err
		}
		if r.Version != s.ModelVersion || r.ID == "" || r.OpID == "" || r.Revision != 1 || r.State != s.ReservationActive || r.BaseSize < 0 || r.BaseSize > s.MaxObjectBytes || !m.now().Before(r.Deadline) || r.Bytes < 0 || r.Bytes > s.MaxObjectBytes || r.StagedBytes < 0 || r.StagedBytes > s.MaxObjectBytes {
			return s.ErrInvalid
		}
		if r.StagedBytes > r.BaseSize && r.Bytes < r.StagedBytes-r.BaseSize {
			return s.ErrInvalid
		}
		old, err := get[s.Reservation](t, key("reservations", r.ID))
		if err == nil {
			if old != r {
				return s.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		found, err := exists(t, key("reservation-ops", r.OpID))
		if err != nil {
			return err
		}
		if found {
			return s.ErrConflict
		}
		b, err := get[s.Bucket](t, key("buckets", r.Object.Bucket))
		if err != nil {
			if errors.Is(err, s.ErrNotFound) {
				return s.ErrBucketNotFound
			}
			return err
		}
		if !b.Active || b.ID != r.Object.BucketID {
			return s.ErrBucketNotFound
		}
		if b.OwnerID != r.OwnerID {
			return s.ErrInvalid
		}
		head, err := optional[s.ObjectHead](t, headKey(r.Object))
		if err != nil {
			return err
		}
		if head.Revision != r.BaseRevision || head.Size != r.BaseSize {
			return s.ErrConflict
		}
		q, err := get[s.LogicalQuota](t, key("quotas", r.OwnerID))
		if err != nil {
			return err
		}
		if q.Limit < 0 || q.Used < 0 || q.Reserved < 0 || q.Staged < 0 || q.Used > q.Limit || q.Reserved > q.Limit-q.Used {
			return s.ErrInvalid
		}
		if r.Bytes > q.Limit-q.Used-q.Reserved {
			return s.ErrQuotaExceeded
		}
		const maxInt = int64(^uint64(0) >> 1)
		cap := maxInt
		if q.Limit <= maxInt-s.MaxObjectBytes {
			cap = q.Limit + s.MaxObjectBytes
		}
		if q.Staged > cap || r.StagedBytes > cap-q.Staged {
			return s.ErrQuotaExceeded
		}
		q.Reserved += r.Bytes
		q.Staged += r.StagedBytes
		if err := put(t, key("quotas", q.OwnerID), q); err != nil {
			return err
		}
		if err := put(t, key("reservation-ops", r.OpID), r.ID); err != nil {
			return err
		}
		return put(t, key("reservations", r.ID), r)
	})
}
