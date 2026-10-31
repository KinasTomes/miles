package object

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/storage"
)

// MaxPartBytes is the UploadPart size cap of API v2 2.5.
const MaxPartBytes int64 = 5 << 30

var ErrPartTooLarge = errors.New("object: part exceeds 5 GiB")

// maxPartAttempts bounds how often UploadPart re-registers on a part revision
// a concurrent replacement of the same part number moved.
const maxPartAttempts = 8

// Uploads is the multipart slice of the shared store (W04-HAI-02): upload,
// part and publisher records, their recovery port, and the receipt that
// answers a Completed upload.
type Uploads interface {
	storage.MultipartStore
	storage.PublisherProtection
	storage.MultipartRecovery
	GetCompletionReceipt(context.Context, string) (storage.CompletedReceipt, error)
}

type CreateUploadRequest struct {
	Bucket                          storage.Bucket // authorized snapshot from the router
	Key                             string
	Metadata                        map[string]string
	ChecksumAlgorithm, ChecksumType string // upper case, a valid pair of API v2 2.7
	CredentialID                    string
}

type PartRequest struct {
	Upload        storage.MultipartUpload // as read for the binding check
	Number        uint32
	Body          Body // Checksum must be of the upload's algorithm
	ContentLength int64
	CredentialID  string
}

// PartResult is the published part: ETag in lower-case hex without quotes,
// checksum in standard Base64 of the upload's algorithm.
type PartResult struct {
	ETag, ChecksumAlgorithm, ChecksumValue string
}

type ListPartsRequest struct {
	UploadID, CredentialID string
	After                  uint32
	Limit                  int // 1..1000
}

type AbortRequest struct {
	Upload       storage.MultipartUpload
	CredentialID string
}

// CreateUpload starts an upload of Bucket/Key. The store binds the caller as
// initiator, the bucket owner and domain, the 24-hour expiry and the upload
// ledger in one transaction.
func (s *Service) CreateUpload(ctx context.Context, r CreateUploadRequest) (storage.MultipartUpload, error) {
	if s.o.Uploads == nil || ctx == nil || r.CredentialID == "" || r.ChecksumAlgorithm == "" || r.ChecksumType == "" {
		return storage.MultipartUpload{}, storage.ErrInvalid
	}
	if err := ValidatePut(0, r.Metadata); err != nil {
		return storage.MultipartUpload{}, err
	}
	object := storage.ObjectKey{Bucket: r.Bucket.Name, BucketID: r.Bucket.ID, Key: r.Key}
	if object.Validate() != nil {
		return storage.MultipartUpload{}, storage.ErrInvalid
	}
	id, err := write.NewManifestID()
	if err != nil {
		return storage.MultipartUpload{}, err
	}
	ctx, a, err := s.admit(ctx, object, r.CredentialID)
	if err != nil {
		return storage.MultipartUpload{}, err
	}
	return s.o.Uploads.CreateUpload(ctx, storage.CreateUploadRequest{
		Request: storage.MultipartRequest{UploadID: id, Credential: r.CredentialID, OpID: a.opID, Writer: a.fence},
		Upload: storage.MultipartUpload{ID: id, Object: object, ReservationID: "reservation-" + a.opID,
			ChecksumAlgorithm: r.ChecksumAlgorithm, ChecksumType: r.ChecksumType, Metadata: r.Metadata},
	})
}

// GetUpload reads the upload record; ErrNotFound when there is none. It never
// reads the receipt: callers check the upload binding first.
func (s *Service) GetUpload(ctx context.Context, id string) (storage.MultipartUpload, error) {
	if s.o.Uploads == nil {
		return storage.MultipartUpload{}, storage.ErrInvalid
	}
	return s.o.Uploads.GetUpload(ctx, id)
}

// ReceiptExpiresAt is the receipt expiry of a Completed upload, zero when its
// receipt is gone. Call it only after the upload binding check.
func (s *Service) ReceiptExpiresAt(ctx context.Context, id string) (time.Time, error) {
	if s.o.Uploads == nil {
		return time.Time{}, storage.ErrInvalid
	}
	receipt, err := s.o.Uploads.GetCompletionReceipt(ctx, id)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return time.Time{}, err
	}
	return receipt.ExpiresAt, nil
}

// UploadPart streams one part into packs and kind-2 part pages, then
// publishes its head with a CAS on the current part revision. The publisher
// is registered before any body byte, so quota and staging caps fail first,
// and its exact pack generations are retained before publication. A
// concurrent replacement of the same part number that publishes first makes
// this one publish again on top of it: the last publisher wins. Publication
// never completes the object.
func (s *Service) UploadPart(ctx context.Context, r PartRequest) (PartResult, error) {
	u := r.Upload
	if s.o.Uploads == nil || ctx == nil || r.Body == nil || r.ContentLength < 0 || r.Number < 1 || r.Number > 10000 || r.CredentialID == "" || u.ID == "" {
		return PartResult{}, storage.ErrInvalid
	}
	if r.ContentLength > MaxPartBytes {
		return PartResult{}, ErrPartTooLarge
	}
	policy, err := s.o.BucketPolicy(u.Object.BucketID)
	if err != nil {
		return PartResult{}, err
	}
	if policy.BucketID() != u.Object.BucketID || policy.Revision() == 0 {
		return PartResult{}, storage.ErrInvalid
	}
	ctx, session, opID, fence, wallDeadline, err := s.begin(ctx, u.Object, r.CredentialID)
	if err != nil {
		return PartResult{}, err
	}
	defer session.Close()
	req := storage.MultipartRequest{UploadID: u.ID, Credential: r.CredentialID, OpID: opID, Writer: fence}
	deadline := wallDeadline
	if u.ExpiresAt.Before(deadline) {
		deadline = u.ExpiresAt
	}
	manifestID, err := write.NewManifestID()
	if err != nil {
		return PartResult{}, err
	}
	candidate := storage.PartPublisher{UploadID: u.ID, OpID: opID, ManifestID: manifestID, PartNumber: r.Number, Bytes: r.ContentLength, Deadline: deadline}
	pub, err := s.register(ctx, req, candidate)
	if err != nil {
		return PartResult{}, err
	}
	fail := func(err error) (PartResult, error) {
		s.cancelPart(ctx, req, pub.ID)
		return PartResult{}, err
	}
	prepared, packs, err := session.PreparePart(write.Pipeline{Chunking: s.o.Chunking, Compression: s.o.Compression, WriteCache: s.o.Cache, AdmitOnWrite: policy.Cache().AdmitOnWrite}, r.Body, manifestID)
	if err != nil {
		return fail(fmt.Errorf("prepare: %w", err))
	}
	if prepared.Size != r.ContentLength {
		return fail(ErrLengthMismatch)
	}
	sum := r.Body.MD5()
	algorithm, digest := r.Body.Checksum()
	if len(sum) != md5.Size || algorithm != u.ChecksumAlgorithm || len(digest) == 0 {
		return fail(storage.ErrInvalid)
	}
	part := storage.UploadPart{Version: storage.ModelVersion, Number: r.Number, Size: prepared.Size, ETag: hex.EncodeToString(sum),
		Checksum:   storage.Checksum{Algorithm: algorithm, Type: u.ChecksumType, Value: base64.StdEncoding.EncodeToString(digest)},
		ManifestID: manifestID, Pages: prepared.PageCount}
	for attempt := 1; ; attempt++ {
		// Both final side effects go through the managed writer, so a closed
		// R1 registration stops them before dispatch, never after.
		err := session.Do("retain/"+pub.ID, func(ctx context.Context, f storage.WriterFence) error {
			return s.o.Uploads.RetainPublisherPacks(ctx, req, pub.ID, packs)
		})
		if err != nil {
			return fail(err)
		}
		var published storage.UploadPart
		err = session.Do("publish/"+pub.ID, func(ctx context.Context, f storage.WriterFence) error {
			var e error
			published, e = s.o.Uploads.PublishPart(ctx, storage.PartPublishRequest{Request: req, PublisherID: pub.ID, Part: part})
			return e
		})
		switch {
		case err == nil:
			return PartResult{ETag: published.ETag, ChecksumAlgorithm: published.Checksum.Algorithm, ChecksumValue: published.Checksum.Value}, nil
		case errors.Is(err, storage.ErrUnavailable):
			// Unknown outcome: the publisher keeps its provisional bytes and
			// roots, and the writer its pins, until recovery proves publish
			// or cancellation.
			return PartResult{}, err
		case !errors.Is(err, storage.ErrConflict) || attempt == maxPartAttempts:
			return fail(err)
		}
		// A replacement of this part number published first: publish the
		// same immutable pages again on its revision.
		s.cancelPart(ctx, req, pub.ID)
		if pub, err = s.register(ctx, req, candidate); err != nil {
			return PartResult{}, err
		}
	}
}

// register opens a publisher of candidate on the current revision of its part
// number, so provisional quota and staging are charged before the body.
func (s *Service) register(ctx context.Context, req storage.MultipartRequest, candidate storage.PartPublisher) (storage.PartPublisher, error) {
	for attempt := 1; ; attempt++ {
		current, err := s.o.Uploads.GetPart(ctx, req, candidate.PartNumber)
		if err != nil && !errors.Is(err, storage.ErrNotFound) {
			return storage.PartPublisher{}, err
		}
		candidate.BaseRevision = current.Revision
		if candidate.ID, err = write.NewManifestID(); err != nil {
			return storage.PartPublisher{}, err
		}
		p, err := s.o.Uploads.RegisterPartPublisher(ctx, req, candidate)
		if errors.Is(err, storage.ErrUnavailable) {
			// An uncertain registration is retried with the same identity.
			p, err = s.o.Uploads.RegisterPartPublisher(ctx, req, candidate)
		}
		if !errors.Is(err, storage.ErrConflict) || attempt == maxPartAttempts {
			return p, err
		}
	}
}

// cancelPart closes an open publisher this request owns and releases its
// provisional bytes. A failure leaves it to Abort or recovery.
func (s *Service) cancelPart(ctx context.Context, req storage.MultipartRequest, id string) {
	c, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = s.o.Uploads.CancelPartPublisher(c, req, id)
}

// ListParts returns one page of published parts after r.After.
func (s *Service) ListParts(ctx context.Context, r ListPartsRequest) (storage.PartListPage, error) {
	if s.o.Uploads == nil || ctx == nil || r.CredentialID == "" {
		return storage.PartListPage{}, storage.ErrInvalid
	}
	return s.o.Uploads.ListParts(ctx, storage.MultipartRequest{UploadID: r.UploadID, Credential: r.CredentialID}, r.After, r.Limit)
}

// AbortUpload takes an Active upload to Aborted and releases its ledger,
// counters and active-upload slot once. An upload with open publishers goes
// through Aborting: each publisher is closed by the trusted recovery port
// (publish or cancellation wins, never both) before the final transition.
// Packs, chunks and part pages are never deleted here: dedup may share them
// with other objects and uploads, and only GC may reclaim them.
func (s *Service) AbortUpload(ctx context.Context, r AbortRequest) error {
	u := r.Upload
	if s.o.Uploads == nil || ctx == nil || r.CredentialID == "" || u.ID == "" {
		return storage.ErrInvalid
	}
	ctx, a, err := s.admit(ctx, u.Object, r.CredentialID)
	if err != nil {
		return err
	}
	req := storage.MultipartRequest{UploadID: u.ID, Credential: r.CredentialID, OpID: a.opID, Writer: a.fence}
	for attempt := 1; ; attempt++ {
		req.ExpectedRevision = u.Revision
		out, err := s.o.Uploads.AbortUpload(ctx, req)
		switch {
		case err == nil && out.State == "Aborted":
			return nil
		case err == nil && out.State == "Aborting":
			if err := s.closePublishers(ctx, req); err != nil {
				return err
			}
		case err == nil:
			return storage.ErrUnavailable
		case !errors.Is(err, storage.ErrConflict) || attempt == maxPartAttempts:
			return err
		}
		// A publish, cancellation or our own Aborting moved the revision.
		if u, err = s.o.Uploads.GetUpload(ctx, u.ID); err != nil {
			return err
		}
		if u.State != "Active" && u.State != "Aborting" {
			return storage.ErrConflict
		}
	}
}

// closePublishers resolves every open publisher of an Aborting upload.
func (s *Service) closePublishers(ctx context.Context, req storage.MultipartRequest) error {
	for after := ""; ; {
		page, err := s.o.Uploads.ListPublishers(ctx, req.UploadID, after, 1000)
		if err != nil {
			return err
		}
		for _, p := range page.Publishers {
			if p.State != "Open" {
				continue
			}
			out, err := s.o.Uploads.ResolvePartPublisher(ctx, req, p.ID)
			if err != nil {
				return err
			}
			if out.Disposition == storage.RecoveryUnknown {
				return storage.ErrUnavailable
			}
		}
		if page.Next == "" {
			return nil
		}
		after = page.Next
	}
}
