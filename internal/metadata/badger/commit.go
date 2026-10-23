package badger

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

type outcome struct {
	Digest [32]byte
	Head   s.ObjectHead
}
type manifestSeal struct {
	OpID      string
	PageCount uint32
}

func (m *Store) CommitObject(ctx context.Context, r s.CommitRequest) (s.CommitResult, error) {
	if r.Head.Deleted {
		return s.CommitResult{}, s.ErrInvalid
	}
	return m.commit(ctx, r)
}
func (m *Store) DeleteObject(ctx context.Context, r s.CommitRequest) (s.CommitResult, error) {
	if !r.Head.Deleted {
		return s.CommitResult{}, s.ErrInvalid
	}
	return m.commit(ctx, r)
}
func (m *Store) commit(ctx context.Context, r s.CommitRequest) (s.CommitResult, error) {
	var result s.CommitResult
	err := m.update(ctx, func(t *db.Txn) error {
		if err := r.Validate(); err != nil {
			return err
		}
		if r.ExpectedRevision == ^uint64(0) {
			return s.ErrConflict
		}
		if err := m.fence(t, r.Writer); err != nil {
			return err
		}
		a, err := m.authorize(t, r.CredentialID, r.Head.Object.Bucket)
		if err != nil {
			return err
		}
		if a.Bucket.ID != r.Head.Object.BucketID {
			return s.ErrNotFound
		}
		if a.UserID != r.UserID || !a.Write {
			return s.ErrAccessDenied
		}
		if a.Bucket.Domain != r.Head.Domain {
			return s.ErrInvalid
		}
		input := r
		input.Writer = s.WriterFence{}
		input.Completion = nil
		input.ReservationRevision = 0
		b, err := json.Marshal(input)
		if err != nil {
			return s.ErrInvalid
		}
		digest := sha256.Sum256(b)
		done, err := get[outcome](t, key("outcomes", r.Head.OpID))
		if err == nil {
			if done.Digest != digest {
				return s.ErrConflict
			}
			result = s.CommitResult{Head: done.Head, AlreadyCommitted: true}
			return nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		old, err := optional[s.ObjectHead](t, headKey(r.Head.Object))
		if err != nil {
			return err
		}
		if old.Revision != r.ExpectedRevision {
			return s.ErrConflict
		}
		found, err := exists(t, key("events", r.Event.ID))
		if err != nil {
			return err
		}
		if found {
			return s.ErrConflict
		}
		reservation, err := get[s.Reservation](t, key("reservations", r.ReservationID))
		if err != nil {
			return err
		}
		if reservation.Object != r.Head.Object || reservation.OwnerID != a.Bucket.OwnerID || reservation.OpID != r.Head.OpID {
			return s.ErrInvalid
		}
		if reservation.State != s.ReservationActive || reservation.Revision != r.ReservationRevision || reservation.Revision == ^uint64(0) {
			return s.ErrConflict
		}
		if reservation.Writer != r.Writer || !m.now().Before(reservation.Deadline) {
			return s.ErrFenced
		}
		if reservation.StagedBytes != r.Head.Size {
			return s.ErrInvalid
		}
		q, err := get[s.LogicalQuota](t, key("quotas", a.Bucket.OwnerID))
		if err != nil {
			return err
		}
		used := q.Used - old.Size
		if q.Limit < 0 || old.Size < 0 || q.Used < old.Size || used > q.Limit || q.Reserved < reservation.Bytes || q.Staged < reservation.StagedBytes {
			return s.ErrInvalid
		}
		if r.Head.Size > q.Limit-used || q.Reserved-reservation.Bytes > q.Limit-used-r.Head.Size {
			return s.ErrQuotaExceeded
		}
		h := r.Head
		if !r.Head.Deleted {
			sealed, err := exists(t, key("manifest-seals", h.ManifestID))
			if err != nil {
				return err
			}
			if sealed {
				return s.ErrConflict
			}
			if err := validateManifest(ctx, t, &h); err != nil {
				return err
			}
			if err := put(t, key("manifest-seals", h.ManifestID), manifestSeal{h.OpID, h.PageCount}); err != nil {
				return err
			}
		} else if h.PageCount != 0 || h.ChunkCount != 0 || len(h.PageIndex) != 0 {
			return s.ErrInvalid
		}
		if r.Receipt != nil {
			u, err := optional[s.Completion](t, key("uploads", r.Completion.UploadID))
			if err != nil {
				return err
			}
			if u != *r.Completion || u.Revision == ^uint64(0) {
				return s.ErrConflict
			}
			if reservation.UploadID != u.UploadID || q.ActiveUploads < 1 || !r.Receipt.ExpiresAt.After(m.now()) {
				return s.ErrInvalid
			}
			found, err := exists(t, key("receipts", u.UploadID))
			if err != nil {
				return err
			}
			if found {
				return s.ErrConflict
			}
			q.ActiveUploads--
			u.State = "Completed"
			u.Revision++
			if err := put(t, key("uploads", u.UploadID), u); err != nil {
				return err
			}
			if err := put(t, key("receipts", u.UploadID), *r.Receipt); err != nil {
				return err
			}
		} else if reservation.UploadID != "" {
			return s.ErrInvalid
		}
		h.Revision = r.ExpectedRevision + 1
		q.Used = used + h.Size
		q.Reserved -= reservation.Bytes
		q.Staged -= reservation.StagedBytes
		reservation.State = s.ReservationCommitted
		reservation.Revision++
		if err := put(t, key("quotas", q.OwnerID), q); err != nil {
			return err
		}
		if err := put(t, headKey(h.Object), h); err != nil {
			return err
		}
		if err := put(t, key("events", r.Event.ID), r.Event); err != nil {
			return err
		}
		if err := put(t, key("event-ops", r.Event.OpID), r.Event.ID); err != nil {
			return err
		}
		if err := put(t, key("reservations", reservation.ID), reservation); err != nil {
			return err
		}
		if err := put(t, key("outcomes", h.OpID), outcome{digest, h}); err != nil {
			return err
		}
		if m.beforePublish != nil {
			if err := m.beforePublish(); err != nil {
				return err
			}
		}
		owned, err := get[outcome](t, key("outcomes", h.OpID))
		if err != nil {
			return err
		}
		result = s.CommitResult{Head: owned.Head}
		return nil
	})
	if err != nil {
		return s.CommitResult{}, err
	}
	if !result.AlreadyCommitted && m.afterPublish != nil {
		if err := m.afterPublish(); err != nil {
			return s.CommitResult{}, err
		}
	}
	return result, nil
}

func (m *Store) ReserveLogicalQuota(ctx context.Context, r s.Reservation) error {
	return m.update(ctx, func(t *db.Txn) error {
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
			return err
		}
		if !b.Active || b.ID != r.Object.BucketID {
			return s.ErrNotFound
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
