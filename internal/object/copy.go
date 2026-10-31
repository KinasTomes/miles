package object

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/storage"
)

// MaxCopyBytes is the single CopyObject source cap (API v2 4.5).
const MaxCopyBytes int64 = 5 << 30

var (
	ErrCopySourceTooLarge = errors.New("object: copy source exceeds 5 GiB")
	ErrCopyToItself       = errors.New("object: copy to itself without changing metadata")
	ErrCopyChecksumChange = errors.New("object: copy cannot change the checksum algorithm")
	ErrCrossDomainCopy    = errors.New("object: copy across dedup domains is not implemented")
	ErrNoSuchCopySource   = fmt.Errorf("%w: copy source key", storage.ErrNotFound)
	// ErrNoSuchCopySourceBucket: the source incarnation went away after the
	// router admitted it.
	ErrNoSuchCopySourceBucket = fmt.Errorf("%w: copy source bucket", storage.ErrBucketNotFound)
)

// CopyRequest is one CopyObject. Source is the read snapshot the router
// admitted on the source bucket and Bucket the write snapshot on the
// destination; quota and domain are the destination's.
type CopyRequest struct {
	Source    storage.Bucket
	SourceKey string
	Bucket    storage.Bucket
	Key       string
	// Metadata replaces the source HTTP metadata (REPLACE); nil keeps it (COPY).
	Metadata map[string]string
	// ChecksumAlgorithm is the algorithm the request names, "" for none.
	ChecksumAlgorithm    string
	UserID, CredentialID string
}

type CopyResult struct {
	ETag         string
	LastModified time.Time
	Checksum     storage.Checksum
}

// CopyObject publishes a new destination head over a fresh manifest that
// repeats the source refs (API v2 4.5, architecture 5.3). It never reads the
// payload: ETag and checksum are the source's, kept opaque. The source head is
// read once, so a concurrent source overwrite cannot mix two versions. Only
// same-domain copies are served; across domains is W06.
func (s *Service) CopyObject(ctx context.Context, r CopyRequest) (CopyResult, error) {
	if ctx == nil || r.Key == "" || r.SourceKey == "" || r.UserID == "" || r.CredentialID == "" ||
		r.Source.Name == "" || r.Source.ID == "" || r.Source.Domain == "" ||
		r.Bucket.Name == "" || r.Bucket.ID == "" || r.Bucket.OwnerID == "" || r.Bucket.Domain == "" {
		return CopyResult{}, storage.ErrInvalid
	}
	source := storage.ObjectKey{Bucket: r.Source.Name, BucketID: r.Source.ID, Key: r.SourceKey}
	object := storage.ObjectKey{Bucket: r.Bucket.Name, BucketID: r.Bucket.ID, Key: r.Key}
	if source.Validate() != nil || object.Validate() != nil {
		return CopyResult{}, storage.ErrInvalid
	}
	src, err := s.o.Metadata.GetObject(ctx, source)
	switch {
	case errors.Is(err, storage.ErrBucketNotFound):
		return CopyResult{}, ErrNoSuchCopySourceBucket
	case errors.Is(err, storage.ErrNotFound):
		return CopyResult{}, ErrNoSuchCopySource
	case err != nil:
		return CopyResult{}, err
	}
	switch {
	case src.Deleted:
		return CopyResult{}, ErrNoSuchCopySource
	case src.Size > MaxCopyBytes:
		return CopyResult{}, ErrCopySourceTooLarge
	case src.Domain != r.Bucket.Domain:
		return CopyResult{}, ErrCrossDomainCopy
	case r.ChecksumAlgorithm != "" && !strings.EqualFold(r.ChecksumAlgorithm, src.Checksum.Algorithm):
		return CopyResult{}, ErrCopyChecksumChange
	case r.Metadata == nil && source.BucketID == object.BucketID && source.Key == object.Key:
		return CopyResult{}, ErrCopyToItself
	}
	metadata := r.Metadata
	if metadata == nil {
		metadata = info(src).Metadata
	}
	if err := ValidatePut(src.Size, metadata); err != nil {
		return CopyResult{}, err
	}

	base, err := s.o.Metadata.GetObject(ctx, object)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return CopyResult{}, err
	}
	var baseRevision uint64
	var baseSize int64
	if err == nil {
		baseRevision, baseSize = base.Revision, base.Size
		if base.Deleted {
			baseSize = 0
		}
	}
	ctx, session, opID, fence, wallDeadline, err := s.begin(ctx, object, r.CredentialID)
	if err != nil {
		return CopyResult{}, err
	}
	open := true
	defer func() {
		if open {
			_ = session.Close()
		}
	}()
	reservationID := "reservation-" + opID
	if err := session.Reserve(storage.Reservation{
		Version: storage.ModelVersion, OpID: opID, BaseRevision: baseRevision, BaseSize: baseSize,
		Revision: 1, State: storage.ReservationActive, Deadline: wallDeadline, ID: reservationID,
		OwnerID: r.Bucket.OwnerID, Object: object, Bytes: max(0, src.Size-baseSize), StagedBytes: src.Size,
		Writer: fence,
	}); err != nil {
		return CopyResult{}, fmt.Errorf("reserve: %w", err)
	}
	manifestID, err := write.NewManifestID()
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return CopyResult{}, err
	}
	prepared, err := session.PrepareCopy(src, manifestID)
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return CopyResult{}, fmt.Errorf("prepare copy: %w", err)
	}
	stored := make(map[string]string, len(metadata)+1)
	for k, v := range metadata {
		stored[k] = v
	}
	stored[commitTimeKey] = s.o.Clock.Now().WallUTC.UTC().Format(time.RFC3339Nano)
	head, err := prepared.BindHead(storage.ObjectHead{
		Version: storage.ModelVersion, Object: object, Domain: r.Bucket.Domain,
		ETag: src.ETag, ETagKind: src.ETagKind, Checksum: src.Checksum, Metadata: stored, OpID: opID,
	})
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return CopyResult{}, err
	}
	event, err := s.o.Events.ObjectCommit(ctx, CommitEvent{OpID: opID, Object: object, Revision: baseRevision + 1, Head: head})
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return CopyResult{}, err
	}
	open = false
	result, err := s.publish(ctx, session, storage.CommitRequest{
		ReservationRevision: 1, Head: head, ExpectedRevision: baseRevision,
		UserID: r.UserID, CredentialID: r.CredentialID, ReservationID: reservationID, Writer: fence, Event: event,
	})
	if err != nil {
		return CopyResult{}, err
	}
	return CopyResult{ETag: result.Head.ETag, LastModified: lastModified(result.Head), Checksum: result.Head.Checksum}, nil
}
