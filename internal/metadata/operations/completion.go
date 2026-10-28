package operations

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	multipart "github.com/KinasTomes/miles/internal/multipart/core"
	s "github.com/KinasTomes/miles/internal/storage"
)

func projection(u s.MultipartUpload) s.Completion {
	return s.Completion{UploadID: u.ID, BucketID: u.Object.BucketID, Key: u.Object.Key, Initiator: u.Initiator, Revision: u.Revision, Fence: u.Fence, State: u.State}
}
func saveCompletion(t Tx, u s.MultipartUpload) error {
	if err := put(t, "uploads", projection(u), u.ID); err != nil {
		return err
	}
	return put(t, "upload-record", u, u.ID)
}

func ValidStaging(t Tx, r s.CommitRequest, ledger s.Reservation) bool {
	if r.MultipartAttempt == 0 {
		return ledger.StagedBytes == r.Head.Size
	}
	if r.Completion == nil {
		return false
	}
	u, err := get[s.MultipartUpload](t, "upload-record", r.Completion.UploadID)
	return err == nil && u.Fence == r.MultipartAttempt && u.Publishers == 0 && u.ProvisionalBytes == 0 && u.PublishedBytes == ledger.StagedBytes && ledger.StagedBytes >= r.Head.Size
}

func (m Multipart) Prepare(ctx context.Context, r s.PrepareCompletionRequest) (s.MultipartUpload, error) {
	u, err := m.Get(ctx, r.Request.UploadID)
	if err != nil {
		return u, err
	}
	if u.State != "Completing" || check(u, r.Request) != nil || u.Publishers != 0 || u.Writer != r.Request.Writer || !m.Now().Before(u.LeaseUntil) {
		return u, s.ErrFenced
	}
	input, err := m.stream(ctx, u.Input)
	if err != nil {
		return u, err
	}
	snapshot, err := m.stream(ctx, r.Snapshot)
	if err != nil {
		return u, err
	}
	if r.Snapshot.Kind != manifest.CompletionSnapshot || r.Snapshot.Owner == u.Input.Owner {
		return u, s.ErrInvalid
	}
	parts, err := manifest.DecodeSnapshot(snapshot)
	if err != nil {
		return u, err
	}
	var in multipart.Input
	if json.Unmarshal(input, &in) != nil || len(parts) != len(in.Parts) {
		return u, s.ErrInvalid
	}
	heads := map[int]multipart.Head{}
	for i, p := range parts {
		if int(p.Number) != in.Parts[i].PartNumber {
			return u, s.ErrInvalid
		}
		var stored s.UploadPart
		err = m.View(ctx, func(t Tx) error {
			var e error
			stored, e = get[s.UploadPart](t, "part-head", u.ID, partNumber(p.Number))
			return e
		})
		if err != nil {
			return u, err
		}
		if stored.Revision != p.Revision || stored.ManifestID != p.ManifestID || stored.Size != int64(p.Size) || stored.Pages != p.Pages || stored.ETag != p.ETag || stored.Checksum.Value != p.Checksum {
			return u, s.ErrInvalid
		}
		heads[int(p.Number)] = multipart.Head{PartNumber: int(p.Number), ETag: p.ETag, Size: int64(p.Size), Checksum: p.Checksum}
	}
	_, assembled, err := multipart.Assemble(in, heads)
	if err != nil {
		return u, err
	}
	h := r.Head
	if h.ManifestID == "" || h.Object != u.Object {
		return u, s.ErrInvalid
	}
	h.Version = 1
	h.Revision = 0
	h.Domain = u.Domain
	h.OpID = u.CompletionOp
	h.ETag = assembled.ETag
	h.ETagKind = "multipart"
	h.Metadata = u.Metadata
	h.Checksum = s.Checksum{Algorithm: u.ChecksumAlgorithm, Type: assembled.ChecksumType, Value: assembled.Checksum, PartCount: len(parts)}
	w, err := manifest.NewStreamWriter(manifest.ObjectPage, h.ManifestID, u.Domain, func(p s.ManifestPage, _ [32]byte) error {
		return m.Metadata().WriteManifestPages(ctx, []s.ManifestPage{p}, r.Request.Writer)
	})
	if err != nil {
		return u, err
	}
	var offset int64
	for _, p := range parts {
		for n := uint32(0); n < p.Pages; n++ {
			page, e := m.Part(ctx, p.ManifestID, n)
			if e != nil {
				return u, e
			}
			for _, ref := range page.Chunks {
				ref.LogicalOffset = offset
				if e = w.Add(ref); e != nil {
					return u, e
				}
				offset += ref.RawBytes
			}
		}
	}
	if err = w.Finish(); err != nil {
		return u, err
	}
	h.Size, h.ChunkCount, h.PageCount, h.PageIndex, _, err = w.Descriptor()
	if err != nil {
		return u, err
	}
	err = m.Update(ctx, func(t Tx) error {
		current, err := get[s.MultipartUpload](t, "upload-record", u.ID)
		if err != nil {
			return err
		}
		if err = m.auth(t, r.Request, current, true); err != nil {
			return err
		}
		if check(current, r.Request) != nil || current.State != "Completing" || current.Publishers != 0 || current.Writer != r.Request.Writer || !m.Now().Before(current.LeaseUntil) {
			return s.ErrFenced
		}
		current.Snapshot = r.Snapshot
		current.Prepared = h
		if err = advance(&current); err != nil {
			return err
		}
		if err = saveCompletion(t, current); err != nil {
			return err
		}
		u = current
		return nil
	})
	return u, err
}
func (m Multipart) Lease(ctx context.Context, r s.MultipartRequest, takeover bool) (s.MultipartUpload, error) {
	var result s.MultipartUpload
	err := m.Update(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.UploadID)
		if err != nil {
			return err
		}
		if err = t.Fence(r.Writer, u.ID); err != nil {
			return err
		}
		if u.State != "Completing" || u.CompletionOp != r.OpID || check(u, r) != nil {
			return s.ErrConflict
		}
		if !takeover {
			if err = m.auth(t, r, u, true); err != nil {
				return err
			}
		}
		if takeover {
			if m.Now().Before(u.LeaseUntil.Add(5 * time.Second)) {
				return s.ErrConflict
			}
			// Terminally fence the exact old attempt before assigning a new worker.
			// Native CAS serializes this with any late final publication.
			if err = t.CloseOperation(u.CompletionOp, u.ReservationID); err != nil {
				return err
			}
			if u.Fence == ^uint64(0) {
				return s.ErrInvalid
			}
			u.Fence++
			u.Writer = r.Writer
			if !m.Now().Before(u.ExpiresAt) {
				return s.ErrFenced
			}
			if err = m.auth(t, r, u, true); err != nil {
				return err
			}
			if err = t.ResetOperation(u.CompletionOp, u.ReservationID); err != nil {
				return err
			}
		} else if u.Writer != r.Writer || !m.Now().Before(u.LeaseUntil) {
			return s.ErrFenced
		}
		u.LeaseUntil = m.Now().Add(60 * time.Second)
		if err = advance(&u); err != nil {
			return err
		}
		ledger, err := get[s.Reservation](t, "reservations", u.ReservationID)
		if err != nil {
			return err
		}
		ledger.Writer = u.Writer
		ledger.Deadline = u.LeaseUntil
		ledger.Revision++
		if err = put(t, "reservations", ledger, ledger.ID); err != nil {
			return err
		}
		if err = saveCompletion(t, u); err != nil {
			return err
		}
		result = u
		return nil
	})
	return result, err
}
func (m Multipart) Terminal(ctx context.Context, r s.MultipartRequest, reopen bool) (s.MultipartUpload, error) {
	var result s.MultipartUpload
	err := m.Update(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.UploadID)
		if err != nil {
			return err
		}
		if err = t.Fence(r.Writer, u.ID); err != nil {
			return err
		}
		if u.State == "Completed" {
			return s.ErrConflict
		}
		if u.State == "Aborted" {
			result = u
			return nil
		}
		if err = check(u, r); err != nil {
			return err
		}
		// Public Abort cannot interrupt Complete. Recovery has a distinct trusted
		// port which closes the pending attempt before changing this state.
		if !reopen && u.State == "Completing" || reopen && u.State != "Completing" {
			return s.ErrConflict
		}
		if !reopen && u.State == "Active" && !m.Now().Before(u.ExpiresAt) {
			return s.ErrNotFound
		}
		if !reopen && u.State == "Active" && u.Publishers != 0 {
			if err = m.auth(t, r, u, false); err != nil {
				return err
			}
			u.State = "Aborting"
			if err = advance(&u); err != nil {
				return err
			}
			if err = saveCompletion(t, u); err != nil {
				return err
			}
			result = u
			return nil
		}
		if u.Publishers != 0 {
			return s.ErrConflict
		}
		if u.State == "Completing" {
			if u.Writer != r.Writer || !m.Now().Before(u.LeaseUntil) {
				return s.ErrFenced
			}
			if err = t.CloseOperation(u.CompletionOp, u.ReservationID); err != nil {
				return err
			}
		} else {
			if err = m.auth(t, r, u, false); err != nil {
				return err
			}
		}
		ledger, err := get[s.Reservation](t, "reservations", u.ReservationID)
		if err != nil {
			return err
		}
		if ledger.State != s.ReservationActive {
			return s.ErrConflict
		}
		if reopen {
			if !m.Now().Before(u.ExpiresAt) {
				return s.ErrFenced
			}
			u.State = "Active"
			u.Input = s.StreamDescriptor{}
			u.Snapshot = s.StreamDescriptor{}
			u.Prepared = s.ObjectHead{}
			ledger.Deadline = u.ExpiresAt
			ledger.Revision++
		} else {
			if err = releaseUpload(t, &u, &ledger); err != nil {
				return err
			}
		}
		if err = advance(&u); err != nil {
			return err
		}
		if err = put(t, "reservations", ledger, ledger.ID); err != nil {
			return err
		}
		if err = saveCompletion(t, u); err != nil {
			return err
		}
		result = u
		return nil
	})
	return result, err
}

func releaseUpload(t Tx, u *s.MultipartUpload, ledger *s.Reservation) error {
	q, err := get[s.LogicalQuota](t, "quotas", u.Owner)
	if err != nil {
		return err
	}
	if ledger.State != s.ReservationActive || q.Reserved < ledger.Bytes || q.Staged < ledger.StagedBytes || q.ActiveUploads < 1 || u.Publishers != 0 || u.ProvisionalBytes != 0 {
		return s.ErrUnavailable
	}
	q.Reserved -= ledger.Bytes
	q.Staged -= ledger.StagedBytes
	q.ActiveUploads--
	if err := Namespace(t, u.Object.Bucket, 0, -1, false); err != nil {
		return err
	}
	ledger.State = s.ReservationReleased
	ledger.Revision++
	u.State = "Aborted"
	return put(t, "quotas", q, q.OwnerID)
}

// Finish is called INSIDE the existing atomic object/receipt transaction. Legacy
// fixtures have no upload-record and retain their original v1 behavior.
func Finish(t Tx, r s.CommitRequest, h s.ObjectHead, now time.Time) error {
	if r.Completion == nil {
		return nil
	}
	u, err := get[s.MultipartUpload](t, "upload-record", r.Completion.UploadID)
	if errors.Is(err, s.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.MultipartAttempt != u.Fence || u.State != "Completing" || projection(u) != *r.Completion || u.Writer != r.Writer || u.Publishers != 0 || u.CompletionOp != h.OpID || u.Input.SHA256 != r.Receipt.InputDigest || u.Snapshot.Pages == 0 || !now.Before(u.LeaseUntil) {
		return s.ErrFenced
	}
	prepared := h
	prepared.Revision = 0
	if !reflect.DeepEqual(prepared, u.Prepared) {
		return s.ErrConflict
	}
	u.State = "Completed"
	if err := Namespace(t, u.Object.Bucket, 0, -1, false); err != nil {
		return err
	}
	if err = advance(&u); err != nil {
		return err
	}
	receipt := s.CompletionReceipt{Version: 1, UploadID: u.ID, OpID: h.OpID, Initiator: u.Initiator, InputDigest: u.Input.SHA256, SnapshotDigest: u.Snapshot.SHA256, Object: u.Object, CommittedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour), Head: h}
	if err = put(t, "full-receipt", receipt, u.ID); err != nil {
		return err
	}
	return put(t, "upload-record", u, u.ID)
}
func (m Multipart) Receipt(ctx context.Context, r s.MultipartRequest, digest string) (s.CompletionReceipt, error) {
	var result s.CompletionReceipt
	err := m.View(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.UploadID)
		if err != nil {
			return err
		}
		if err = authorize(t, r, u, true); err != nil {
			return err
		}
		if u.State != "Completed" {
			return s.ErrNotFound
		}
		p, err := get[s.CompletionReceipt](t, "full-receipt", u.ID)
		if err != nil {
			return err
		}
		if !m.Now().Before(p.ExpiresAt) {
			return s.ErrNotFound
		}
		if p.InputDigest != digest {
			return s.ErrConflict
		}
		result = p
		return nil
	})
	return result, err
}
func (m Multipart) Retain(ctx context.Context, r s.MultipartRequest, publisher string, packs []s.PackRef) error {
	if len(packs) > 1024 {
		return s.ErrInvalid
	}
	for _, p := range packs {
		if p.Validate() != nil {
			return s.ErrInvalid
		}
	}
	return m.Update(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.UploadID)
		if err != nil {
			return err
		}
		if err = t.Fence(r.Writer, u.ID); err != nil {
			return err
		}
		p, err := get[s.PartPublisher](t, "candidate", publisher)
		if err != nil {
			return err
		}
		if p.State != "Open" || p.Writer != r.Writer || p.UploadID != u.ID {
			return s.ErrFenced
		}
		seen := map[s.PackRef]bool{}
		for _, ref := range p.Packs {
			seen[ref] = true
		}
		for _, ref := range packs {
			if !seen[ref] {
				p.Packs = append(p.Packs, ref)
				seen[ref] = true
			}
		}
		if len(p.Packs) > 1024 {
			return s.ErrInvalid
		}
		if err = put(t, "candidate", p, p.ID); err != nil {
			return err
		}
		return put(t, "publisher", p, u.ID, p.ID)
	})
}
func partNumber(n uint32) string { return strconv.FormatUint(uint64(n), 10) }
