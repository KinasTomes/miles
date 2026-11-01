package dynamodb

import (
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/metadata/operations"

	s "github.com/KinasTomes/miles/internal/storage"
)

var _ s.MetadataStore = (*Store)(nil)

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
	if err := r.Validate(); err != nil {
		return s.CommitResult{}, err
	}
	if resumed, handled, err := m.resumeCommit(ctx, r); handled {
		return resumed, err
	}
	var result s.CommitResult
	err := m.update(ctx, func(t *transaction) error {
		result = s.CommitResult{}

		if m.requireProducers && r.Producer == nil {
			return s.ErrFenced
		}
		if r.Producer == nil {
			if _, shared := m.authority.(*SharedAuthority); shared {
				g, err := r1Read[s.R1Gate](t, r1GateKey())
				if err != nil {
					return err
				}
				if g.RecoveryEnabled {
					return s.ErrFenced
				}
			}
		}
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
			return s.ErrBucketNotFound
		}
		if a.UserID != r.UserID || !a.Write {
			return s.ErrAccessDenied
		}
		if a.Bucket.Domain != r.Head.Domain {
			return s.ErrInvalid
		}
		digest, err := commitDigest(r)
		if err != nil {
			return s.ErrInvalid
		}
		t.operationID, t.operationDigest = r.Head.OpID, digest
		t.dispatchRequest = &r
		operation, err := t.load(key("outcomes", r.Head.OpID))
		if err != nil {
			return err
		}
		if operation.after != nil && operation.after["pending_request"] != nil {
			return s.ErrUnavailable
		}
		if operationClosed(operation.after) {
			return s.ErrFenced
		}

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
		found, err = exists(t, key("event-ops", r.Event.OpID))
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
		if !operations.ValidStaging(multipartTx{t}, r, reservation) {
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
			if err := m.prepareManifest(ctx, r.Writer, &h); err != nil {
				return err
			}
			ownerAddress, _ := resolve(key("manifest-owner", h.ManifestID))
			delete(t.rows, identity(ownerAddress.key))
			owner, err := get[manifestOwner](t, key("manifest-owner", h.ManifestID))
			if err != nil {
				return err
			}
			if owner.State != "Verified" || owner.OpID != h.OpID {
				return s.ErrConflict
			}
			if err := m.protect(t, r.Writer, h.ManifestID); err != nil {
				return err
			}
			owner.State = "Published"
			if err := advanceOwner(&owner); err != nil {
				return err
			}
			if err := put(t, key("manifest-owner", h.ManifestID), owner); err != nil {
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
		if err := operations.Finish(multipartTx{t}, r, h, m.now()); err != nil {
			return err
		}
		q.Used = used + h.Size
		q.Reserved -= reservation.Bytes
		q.Staged -= reservation.StagedBytes
		reservation.State = s.ReservationCommitted
		reservation.Revision++
		delta := int64(0)
		if old.Revision == 0 || old.Deleted {
			if !h.Deleted {
				delta = 1
			}
		} else if h.Deleted {
			delta = -1
		}
		if err := operations.Namespace(multipartTx{t}, h.Object.Bucket, delta, 0, m.requireProducers); err != nil {
			return err
		}
		event, eventErr := operations.ObjectEvent(multipartTx{t}, r, h, old, q, reservation)
		if eventErr != nil {
			return eventErr
		}

		if err := put(t, key("quotas", q.OwnerID), q); err != nil {
			return err
		}
		if err := put(t, headKey(h.Object), h); err != nil {
			return err
		}
		if err := put(t, key("events", event.ID), event); err != nil {
			return err
		}
		if err := put(t, key("event-ops", event.OpID), event.ID); err != nil {
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
