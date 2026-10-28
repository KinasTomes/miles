package operations

import (
	"context"
	s "github.com/KinasTomes/miles/internal/storage"
	"time"
)

// ResolveCompletion never replays a final publication. The close marker and
// upload transition share the native transaction CAS with the original final
// request. A lost response is deliberately Unknown; retry observes durable state.
func (m Multipart) ResolveCompletion(ctx context.Context, r s.MultipartRequest) (s.MultipartRecoveryResult, error) {
	out := s.MultipartRecoveryResult{Disposition: s.RecoveryUnknown}
	err := m.Update(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.UploadID)
		if err != nil {
			return err
		}
		if err = t.Fence(r.Writer, u.ID); err != nil {
			return err
		}
		if u.CompletionOp != "" && u.CompletionOp != r.OpID {
			return s.ErrConflict
		}
		if u.State == "Completed" {
			p, err := get[s.CompletionReceipt](t, "full-receipt", u.ID)
			if err != nil {
				return err
			}
			out.Upload, out.Receipt, out.Disposition = u, p, s.RecoveryCommitted
			return nil
		}
		if u.State == "Aborted" {
			out.Upload, out.Disposition = u, s.RecoveryClosed
			return nil
		}
		if err = check(u, r); err != nil {
			return err
		}
		if u.State == "Completing" {
			if u.Writer != r.Writer || !m.Now().Before(u.LeaseUntil) {
				if m.Now().Before(u.LeaseUntil.Add(5 * time.Second)) {
					return s.ErrConflict
				}
			}
			if err = t.CloseOperation(u.CompletionOp, u.ReservationID); err != nil {
				return err
			}
			u.ClosedFence, u.ClosedWriter = u.Fence, u.Writer
		} else if u.State == "Active" {
			// Active expiry only allows attempting an atomic admission barrier.
			// It does not cancel outstanding publisher records or their roots.
			if m.Now().Before(u.ExpiresAt) {
				return s.ErrConflict
			}
		} else if u.State != "Aborting" {
			return s.ErrConflict
		}
		if u.State != "Aborting" {
			if u.Fence == ^uint64(0) {
				return s.ErrInvalid
			}
			u.Fence++
			u.State = "Aborting"
		}
		u.Writer = r.Writer
		if u.Publishers == 0 {
			ledger, err := get[s.Reservation](t, "reservations", u.ReservationID)
			if err != nil {
				return err
			}
			if err = releaseUpload(t, &u, &ledger); err != nil {
				return err
			}
			if err = put(t, "reservations", ledger, ledger.ID); err != nil {
				return err
			}
		}
		if err = advance(&u); err != nil {
			return err
		}
		if err = saveCompletion(t, u); err != nil {
			return err
		}
		out.Upload, out.Disposition = u, s.RecoveryClosed
		return nil
	})
	if err != nil {
		return s.MultipartRecoveryResult{Disposition: s.RecoveryUnknown}, err
	}
	return out, nil
}

// Cancellation is a conditional write on candidate AND Upload. Even if a
// publisher is still in flight, only one of publish/cancel can win. Terminal
// membership and packs are retained for GC/recovery until upload termination.
func (m Multipart) ResolvePublisher(ctx context.Context, r s.MultipartRequest, id string) (s.MultipartRecoveryResult, error) {
	out := s.MultipartRecoveryResult{Disposition: s.RecoveryUnknown}
	if err := m.cancel(ctx, r, id, true); err != nil {
		return out, err
	}
	err := m.View(ctx, func(t Tx) error {
		if err := t.Fence(r.Writer, r.UploadID); err != nil {
			return err
		}
		p, err := get[s.PartPublisher](t, "candidate", id)
		if err != nil {
			return err
		}
		if p.UploadID != r.UploadID {
			return s.ErrConflict
		}
		out.Publisher = p
		switch p.State {
		case "Published":
			out.Disposition = s.RecoveryCommitted
		case "Cancelled":
			out.Disposition = s.RecoveryClosed
		default:
			return s.ErrUnavailable
		}
		return nil
	})
	if err != nil {
		return s.MultipartRecoveryResult{Disposition: s.RecoveryUnknown}, err
	}
	return out, nil
}
