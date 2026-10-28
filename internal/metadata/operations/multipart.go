// Package operations shares business rules, not persistence, between adapters.
// Every Update callback is executed by the adapter's native transaction engine.
package operations

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	multipart "github.com/KinasTomes/miles/internal/multipart/core"
	"github.com/KinasTomes/miles/internal/recovery/format"
	s "github.com/KinasTomes/miles/internal/storage"
)

type Tx interface {
	Attach(byte, string, string, uint32, int64, string) (format.PayloadDependency, error)
	Admit(s.MutationContext) error
	Get(string, []string, any) error
	Put(string, []string, any) error
	Authorize(string, string) (s.Authorization, error)
	Fence(s.WriterFence, string) error
	CloseOperation(string, string) error
	ResetOperation(string, string) error
}
type Backend interface {
	Update(context.Context, func(Tx) error) error
	View(context.Context, func(Tx) error) error
	Now() time.Time
	Metadata() s.MetadataStore
	Part(context.Context, string, uint32) (s.ManifestPage, error)
	CompletionPage(context.Context, byte, string, uint32) ([]byte, error)
	Parts(context.Context, string, uint32, int) (s.PartListPage, error)
	Publishers(context.Context, string, string, int) (s.PublisherPage, error)
}
type Multipart struct{ Backend }

func get[T any](t Tx, kind string, ids ...string) (T, error) {
	var v T
	err := t.Get(kind, ids, &v)
	return v, err
}
func put(t Tx, kind string, value any, ids ...string) error { return t.Put(kind, ids, value) }
func id(value string) bool {
	if len(value) != 32 {
		return false
	}
	for _, c := range value {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}
func advance(u *s.MultipartUpload) error {
	if u.Revision == ^uint64(0) {
		return s.ErrInvalid
	}
	u.Revision++
	return nil
}
func authorize(t Tx, r s.MultipartRequest, u s.MultipartUpload, initiator bool) error {
	a, err := t.Authorize(r.Credential, u.Object.Bucket)
	if err != nil {
		return err
	}
	if a.Bucket.ID != u.Object.BucketID {
		return s.ErrBucketNotFound
	}
	if (!a.Write && (initiator || !a.SystemAdmin)) || initiator && a.UserID != u.Initiator || !initiator && a.UserID != u.Initiator && a.UserID != u.Owner && !a.SystemAdmin {
		return s.ErrAccessDenied
	}
	return nil
}
func (m Multipart) auth(t Tx, r s.MultipartRequest, u s.MultipartUpload, initiator bool) error {
	if err := authorize(t, r, u, initiator); err != nil {
		return err
	}
	return t.Fence(r.Writer, u.ID)
}
func check(u s.MultipartUpload, r s.MultipartRequest) error {
	if u.ID != r.UploadID || u.Revision != r.ExpectedRevision || r.Fence != 0 && u.Fence != r.Fence {
		return s.ErrConflict
	}
	return nil
}
func (m Multipart) Get(ctx context.Context, id string) (s.MultipartUpload, error) {
	var u s.MultipartUpload
	err := m.View(ctx, func(t Tx) error { var e error; u, e = get[s.MultipartUpload](t, "upload-record", id); return e })
	return u, err
}
func (m Multipart) Create(ctx context.Context, r s.CreateUploadRequest) (s.MultipartUpload, error) {
	var result s.MultipartUpload
	err := m.Update(ctx, func(t Tx) error {
		u := r.Upload
		if !id(u.ID) || r.Request.UploadID != u.ID || !id(r.Request.OpID) || u.Object.Validate() != nil || u.ReservationID == "" || !multipart.ValidChecksumMode(u.ChecksumAlgorithm, u.ChecksumType) {
			return s.ErrInvalid
		}
		a, err := t.Authorize(r.Request.Credential, u.Object.Bucket)
		if err != nil {
			return err
		}
		if !a.Write {
			return s.ErrAccessDenied
		}
		if a.Bucket.ID != u.Object.BucketID {
			return s.ErrBucketNotFound
		}
		if err := t.Fence(r.Request.Writer, u.ID); err != nil {
			return err
		}
		binding, _ := json.Marshal(struct {
			Object                       s.ObjectKey
			Reservation, Algorithm, Type string
			Metadata                     map[string]string
		}{u.Object, u.ReservationID, u.ChecksumAlgorithm, u.ChecksumType, u.Metadata})
		sum := sha256.Sum256(binding)
		u.CreateDigest = hex.EncodeToString(sum[:])
		u.CreateOp = r.Request.OpID
		old, err := get[s.MultipartUpload](t, "upload-record", u.ID)
		if err == nil {
			if old.CreateOp != u.CreateOp || old.CreateDigest != u.CreateDigest || old.Object != u.Object || old.Initiator != a.UserID || old.Credential != r.Request.Credential {
				return s.ErrConflict
			}
			result = old
			return nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		q, err := get[s.LogicalQuota](t, "quotas", a.Bucket.OwnerID)
		if err != nil {
			return err
		}
		if q.ActiveUploads >= 16 {
			return s.ErrQuotaExceeded
		}
		base, err := get[s.ObjectHead](t, "heads", u.Object.Bucket, u.Object.BucketID, u.Object.Key)
		if err != nil && !errors.Is(err, s.ErrNotFound) {
			return err
		}
		if _, err := get[s.Reservation](t, "reservations", u.ReservationID); err == nil {
			return s.ErrConflict
		} else if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		u.Version = 1
		u.Owner = a.Bucket.OwnerID
		u.Initiator = a.UserID
		u.Credential = r.Request.Credential
		u.Domain = a.Bucket.Domain
		u.State = "Active"
		u.Revision = 1
		u.Fence = 0
		u.ExpiresAt = m.Now().Add(24 * time.Hour)
		u.Writer = r.Request.Writer
		u.CompletionOp = ""
		u.PublishedBytes = 0
		u.ProvisionalBytes = 0
		u.Publishers = 0
		u.Input = s.StreamDescriptor{}
		u.Snapshot = s.StreamDescriptor{}
		u.Prepared = s.ObjectHead{}
		ledger := s.Reservation{Version: 1, ID: u.ReservationID, OpID: r.Request.OpID, OwnerID: u.Owner, UploadID: u.ID, Object: u.Object, BaseRevision: base.Revision, BaseSize: base.Size, Revision: 1, State: s.ReservationActive, Deadline: u.ExpiresAt, Writer: r.Request.Writer}
		q.ActiveUploads++
		if err := Namespace(t, u.Object.Bucket, 0, 1, false); err != nil {
			return err
		}
		for _, write := range []func() error{func() error { return put(t, "upload-record", u, u.ID) }, func() error { return put(t, "quotas", q, q.OwnerID) }, func() error { return put(t, "reservations", ledger, ledger.ID) }} {
			if err := write(); err != nil {
				return err
			}
		}
		result = u
		return nil
	})
	return result, err
}
func (m Multipart) accounting(t Tx, u s.MultipartUpload) error {
	r, err := get[s.Reservation](t, "reservations", u.ReservationID)
	if err != nil {
		return err
	}
	if r.State != s.ReservationActive {
		return s.ErrConflict
	}
	q, err := get[s.LogicalQuota](t, "quotas", u.Owner)
	if err != nil {
		return err
	}
	if u.PublishedBytes < 0 || u.ProvisionalBytes < 0 || u.PublishedBytes > s.MaxObjectBytes-u.ProvisionalBytes {
		return s.ErrQuotaExceeded
	}
	total := u.PublishedBytes + u.ProvisionalBytes
	q, err = s.AdjustReservationQuota(q, r, max(0, total-r.BaseSize), total)
	if err != nil {
		return err
	}
	if r.Revision == ^uint64(0) {
		return s.ErrInvalid
	}
	r.Bytes = max(0, total-r.BaseSize)
	r.StagedBytes = total
	r.Revision++
	if err := put(t, "reservations", r, r.ID); err != nil {
		return err
	}
	return put(t, "quotas", q, q.OwnerID)
}
func (m Multipart) Register(ctx context.Context, r s.MultipartRequest, p s.PartPublisher) (s.PartPublisher, error) {
	var result s.PartPublisher
	err := m.Update(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.UploadID)
		if err != nil {
			return err
		}
		if err = m.auth(t, r, u, true); err != nil {
			return err
		}
		if !id(p.ID) || !id(p.OpID) || p.UploadID != u.ID || p.PartNumber == 0 || p.PartNumber > 10000 || p.Bytes < 0 || p.Bytes > 5<<30 || p.ManifestID == "" || !m.Now().Before(p.Deadline) || p.Deadline.After(u.ExpiresAt) {
			return s.ErrInvalid
		}
		old, err := get[s.PartPublisher](t, "candidate", p.ID)
		if err == nil {
			candidate := p
			candidate.Version = 1
			candidate.State = "Open"
			candidate.Writer = r.Writer
			candidate.Packs = nil
			if !reflect.DeepEqual(old, candidate) {
				return s.ErrConflict
			}
			result = old
			return nil
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		if u.State != "Active" || !m.Now().Before(u.ExpiresAt) {
			return s.ErrFenced
		}
		part, err := get[s.UploadPart](t, "part-head", u.ID, fmt.Sprint(p.PartNumber))
		if err != nil && !errors.Is(err, s.ErrNotFound) {
			return err
		}
		if part.Revision != p.BaseRevision {
			return s.ErrConflict
		}
		p.Version = 1
		p.Writer = r.Writer
		p.State = "Open"
		p.Packs = nil
		u.ProvisionalBytes += p.Bytes
		u.Publishers++
		if err = advance(&u); err != nil {
			return err
		}
		if err = m.accounting(t, u); err != nil {
			return err
		}
		if err = put(t, "candidate", p, p.ID); err != nil {
			return err
		}
		if err = put(t, "publisher", p, u.ID, p.ID); err != nil {
			return err
		}
		if err = put(t, "upload-record", u, u.ID); err != nil {
			return err
		}
		result = p
		return nil
	})
	return result, err
}
func (m Multipart) verifyPart(ctx context.Context, u s.MultipartUpload, p s.UploadPart) error {
	if p.Version != 1 || p.Number == 0 || p.Number > 10000 || p.Size < 0 || p.Size > 5<<30 || p.Pages > manifest.MaxPages || (p.Size == 0) != (p.Pages == 0) || len(p.ETag) != 32 || p.Checksum.Algorithm != u.ChecksumAlgorithm || p.Checksum.Type != u.ChecksumType {
		return s.ErrInvalid
	}
	var offset int64
	for n := uint32(0); n < p.Pages; n++ {
		page, err := m.Part(ctx, p.ManifestID, n)
		if err != nil {
			return err
		}
		keys := make([]s.ChunkKey, len(page.Chunks))
		for i, c := range page.Chunks {
			keys[i] = c.Key
		}
		chunks, err := m.Metadata().GetChunks(ctx, keys, true)
		if err != nil {
			return err
		}
		for _, ref := range page.Chunks {
			c, ok := chunks[ref.Key]
			if !ok || ref.Key.Domain != u.Domain || ref.LogicalOffset != offset || c.RawBytes != ref.RawBytes {
				return s.ErrInvalid
			}
			offset += ref.RawBytes
		}
	}
	if offset != p.Size {
		return s.ErrInvalid
	}
	return nil
}
func (m Multipart) Publish(ctx context.Context, r s.PartPublishRequest) (s.UploadPart, error) {
	u, err := m.Get(ctx, r.Request.UploadID)
	if err != nil {
		return s.UploadPart{}, err
	}
	if err = m.verifyPart(ctx, u, r.Part); err != nil {
		return s.UploadPart{}, err
	}
	var result s.UploadPart
	err = m.Update(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.Request.UploadID)
		if err != nil {
			return err
		}
		if err = m.auth(t, r.Request, u, true); err != nil {
			return err
		}
		p, err := get[s.PartPublisher](t, "candidate", r.PublisherID)
		if err != nil {
			return err
		}
		old, err := get[s.UploadPart](t, "part-head", u.ID, fmt.Sprint(r.Part.Number))
		if err != nil && !errors.Is(err, s.ErrNotFound) {
			return err
		}
		if p.State == "Published" && old.OpID == p.OpID {
			result = old
			return nil
		}
		if u.State != "Active" || !m.Now().Before(u.ExpiresAt) || !m.Now().Before(p.Deadline) {
			return s.ErrFenced
		}
		if p.State != "Open" || p.Writer != r.Request.Writer || p.UploadID != u.ID || p.PartNumber != r.Part.Number || p.ManifestID != r.Part.ManifestID || p.Bytes != r.Part.Size || old.Revision != p.BaseRevision {
			return s.ErrConflict
		}
		part := r.Part
		part.Revision = old.Revision + 1
		part.UploadID = u.ID
		part.OpID = p.OpID
		u.PublishedBytes += part.Size - old.Size
		u.ProvisionalBytes -= p.Bytes
		if u.Publishers == 0 {
			return s.ErrUnavailable
		}
		u.Publishers--
		if err = advance(&u); err != nil {
			return err
		}
		p.State = "Published"
		if err = m.accounting(t, u); err != nil {
			return err
		}
		for _, w := range []func() error{func() error { return put(t, "candidate", p, p.ID) }, func() error { return put(t, "publisher", p, u.ID, p.ID) }, func() error { return put(t, "part-head", part, u.ID, fmt.Sprint(part.Number)) }, func() error { return put(t, "upload-record", u, u.ID) }} {
			if err = w(); err != nil {
				return err
			}
		}
		result = part
		return nil
	})
	return result, err
}
func (m Multipart) Cancel(ctx context.Context, r s.MultipartRequest, id string) error {
	return m.Update(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.UploadID)
		if err != nil {
			return err
		}
		if err = t.Fence(r.Writer, u.ID); err != nil {
			return err
		}
		p, err := get[s.PartPublisher](t, "candidate", id)
		if err != nil {
			return err
		}
		if p.UploadID != u.ID {
			return s.ErrConflict
		}
		if p.State != "Open" {
			return nil
		}
		// Native CAS on both Upload and candidate fences a late publish. No absent
		// outcome/lease-expiry inference is used to remove provisional accounting.
		if p.Writer != r.Writer && u.Writer != r.Writer {
			return s.ErrFenced
		}
		if u.Publishers == 0 || u.ProvisionalBytes < p.Bytes {
			return s.ErrUnavailable
		}
		u.Publishers--
		u.ProvisionalBytes -= p.Bytes
		if err = advance(&u); err != nil {
			return err
		}
		p.State = "Cancelled"
		if err = m.accounting(t, u); err != nil {
			return err
		}
		if err = put(t, "candidate", p, p.ID); err != nil {
			return err
		}
		if err = put(t, "publisher", p, u.ID, p.ID); err != nil {
			return err
		}
		return put(t, "upload-record", u, u.ID)
	})
}
func (m Multipart) List(ctx context.Context, r s.MultipartRequest, after uint32, limit int) (s.PartListPage, error) {
	u, err := m.Get(ctx, r.UploadID)
	if err != nil {
		return s.PartListPage{}, err
	}
	err = m.View(ctx, func(t Tx) error { return authorize(t, r, u, false) })
	if err != nil {
		return s.PartListPage{}, err
	}
	if u.State != "Active" && u.State != "Completing" || u.State == "Active" && !m.Now().Before(u.ExpiresAt) {
		return s.PartListPage{}, s.ErrNotFound
	}
	return m.Parts(ctx, u.ID, after, limit)
}
func (m Multipart) stream(ctx context.Context, d s.StreamDescriptor) ([]byte, error) {
	if d.Length > 8<<20 {
		return nil, s.ErrInvalid
	}
	var b bytes.Buffer
	err := manifest.VerifyCompletionStream(d, func(n uint32) ([]byte, error) { return m.CompletionPage(ctx, d.Kind, d.Owner, n) }, func(p []byte) error { _, e := b.Write(p); return e })
	return b.Bytes(), err
}
func (m Multipart) Freeze(ctx context.Context, r s.FreezeRequest) (s.MultipartUpload, error) {
	if r.Input.Kind != manifest.CompletionInput || !id(r.Request.OpID) {
		return s.MultipartUpload{}, s.ErrInvalid
	}
	body, err := m.stream(ctx, r.Input)
	if err != nil {
		return s.MultipartUpload{}, err
	}
	var in multipart.Input
	if json.Unmarshal(body, &in) != nil || in.V != 1 || !bytes.Equal(body, in.Canonical()) || in.Digest() != r.Input.SHA256 || len(in.Parts) == 0 || len(in.Parts) > 10000 {
		return s.MultipartUpload{}, s.ErrInvalid
	}
	for i, p := range in.Parts {
		if p.PartNumber < 1 || p.PartNumber > 10000 || i > 0 && p.PartNumber <= in.Parts[i-1].PartNumber {
			return s.MultipartUpload{}, s.ErrInvalid
		}
	}
	var result s.MultipartUpload
	err = m.Update(ctx, func(t Tx) error {
		u, err := get[s.MultipartUpload](t, "upload-record", r.Request.UploadID)
		if err != nil {
			return err
		}
		if err = m.auth(t, r.Request, u, true); err != nil {
			return err
		}
		if in.BucketID != u.Object.BucketID || in.Key != u.Object.Key || in.ChecksumAlgorithm != u.ChecksumAlgorithm || in.ChecksumType != u.ChecksumType {
			return s.ErrInvalid
		}
		if u.State == "Completing" {
			if u.Input.SHA256 != r.Input.SHA256 || u.CompletionOp != r.Request.OpID {
				return s.ErrConflict
			}
			result = u
			return nil
		}
		if err = check(u, r.Request); err != nil {
			return err
		}
		if u.State != "Active" || !m.Now().Before(u.ExpiresAt) {
			return s.ErrFenced
		}
		if u.CompletionOp != "" && u.CompletionOp != r.Request.OpID {
			return s.ErrConflict
		}
		if u.Fence == ^uint64(0) {
			return s.ErrInvalid
		}
		u.State = "Completing"
		u.Fence++
		u.Input = r.Input
		u.Snapshot = s.StreamDescriptor{}
		u.Prepared = s.ObjectHead{}
		u.CompletionOp = r.Request.OpID
		u.Writer = r.Request.Writer
		u.LeaseUntil = m.Now().Add(60 * time.Second)
		if err = advance(&u); err != nil {
			return err
		}
		ledger, err := get[s.Reservation](t, "reservations", u.ReservationID)
		if err != nil {
			return err
		}
		if ledger.State != s.ReservationActive {
			return s.ErrConflict
		}
		ledger.OpID = u.CompletionOp
		ledger.Writer = u.Writer
		ledger.Deadline = u.LeaseUntil
		ledger.Revision++
		if err = t.ResetOperation(u.CompletionOp, u.ReservationID); err != nil {
			return err
		}
		if err = put(t, "reservation-ops", u.ReservationID, u.CompletionOp); err != nil {
			return err
		}
		if err = put(t, "reservations", ledger, ledger.ID); err != nil {
			return err
		}
		if err = put(t, "uploads", s.Completion{UploadID: u.ID, BucketID: u.Object.BucketID, Key: u.Object.Key, Initiator: u.Initiator, Revision: u.Revision, Fence: u.Fence, State: u.State}, u.ID); err != nil {
			return err
		}
		if err = put(t, "upload-record", u, u.ID); err != nil {
			return err
		}
		result = u
		return nil
	})
	return result, err
}
