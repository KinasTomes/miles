// Package object joins the S3 object operations to the streaming write session
// and the Warm read path. It owns the metadata transaction inputs (quota
// reservation, head, outbox event) but not HTTP parsing or authorization.
package object

import (
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
)

// commitTimeKey carries the commit wall time in the head's HTTP metadata map:
// ObjectHead has no timestamp field yet (metadata owner gap). The reserved
// x-dsg- prefix is never echoed to clients. Replace with a head field when the
// shared model owns one.
const commitTimeKey = "x-dsg-commit-time"

// ErrLengthMismatch reports a body shorter or longer than the decoded length
// the request declared. The HTTP layer maps it to IncompleteBody.
var ErrLengthMismatch = errors.New("object: body length does not match Content-Length")

// Body is the verified logical payload the API layer hands to the engine. The
// API's *s3.Payload implements it; checksums are checked before the engine
// sees the last byte.
type Body interface {
	io.Reader
	MD5() []byte
	Checksum() (algorithm string, digest []byte)
}

type PutRequest struct {
	Bucket        storage.Bucket // authorized snapshot from the router
	Key           string
	Body          Body
	ContentLength int64 // decoded length, never negative
	Metadata      map[string]string
	UserID        string
	CredentialID  string
}

type PutResult struct {
	ETag              string
	ChecksumAlgorithm string
	ChecksumValue     string
}

type GetRequest struct {
	Bucket       string
	BucketID     string
	Key          string
	CredentialID string
	Range        *storage.ByteRange
	// SelectRange runs against the read view's bound head before payload planning.
	// Mutually exclusive with Range; HTTP callers use it for Range and If-Match.
	SelectRange func(Info) (*storage.ByteRange, error)
	// Ready sees the committed head and resolved range before the first body
	// byte; an error aborts the read before any header is committed.
	Ready ReadyFunc
	Emit  func(context.Context, []byte) error
}

// Info is the committed object state GET and HEAD report.
type Info struct {
	ETag         string
	Size         int64
	LastModified time.Time
	Metadata     map[string]string
	Checksum     storage.Checksum
}

type GetResult struct {
	Info  Info
	Range storage.ByteRange
}

type ReadyFunc func(GetResult) error

// DeleteRequest names the key DeleteObject tombstones in the authorized bucket.
type DeleteRequest struct {
	Bucket       storage.Bucket // authorized snapshot from the router
	Key          string
	UserID       string
	CredentialID string
}

// ListEntry is one committed object of a LIST page.
type ListEntry struct {
	Key, ETag    string
	Size         int64
	LastModified time.Time
}

// ListPage is one logical LIST page; Next is the internal scan state of the
// following page, nil on the last one. The API layer seals it into its token.
type ListPage struct {
	Objects        []ListEntry
	CommonPrefixes []string
	Next           *storage.ObjectListCursor
}

// Metadata is the slice of the shared store the object service needs. It is a
// projection of storage.MetadataStore, not a second store.
type Metadata interface {
	write.CoordinationStore
	GetObject(context.Context, storage.ObjectKey) (storage.ObjectHead, error)
	ListObjects(context.Context, storage.ListObjectsRequest) (storage.ObjectListPage, error)
	ReleaseLogicalQuota(context.Context, storage.ReservationRelease) error
}

// CommitEvent is the input the outbox producer turns into the authoritative
// event. The producer is not part of W03: the shared R2 journal belongs to
// W04-HAI-02. Until then only tests supply a builder.
// Head contains the complete prepared descriptor with Revision still zero;
// Revision is the resulting revision assigned by the publication transaction.
type CommitEvent struct {
	OpID     string
	Object   storage.ObjectKey
	Revision uint64
	Head     storage.ObjectHead
}

type EventBuilder interface {
	ObjectCommit(context.Context, CommitEvent) (storage.OutboxEvent, error)
}

// WriterRequest binds admission to one logical PUT and its immutable deadline.
// The trusted coordinator owns backend registration, UTC/protection proof and
// later durable reconciliation; the object service never mints a fence.
type WriterRequest struct {
	Object             storage.ObjectKey
	OpID, CredentialID string
	Deadline           clock.Instant
	WallDeadline       time.Time
}

// AdmittedWriter carries fresh backend authority and the coordinator's verified
// protection window for this request, not a process-wide reusable fence.
type AdmittedWriter struct {
	Fence          storage.WriterFence
	ProtectedUntil time.Time
}

type Options struct {
	Metadata       Metadata
	Packs          storage.PackStore
	Registry       *coordination.WriterRegistry
	Clock          clock.Clock
	AdmitWriter    func(context.Context, WriterRequest) (AdmittedWriter, error)
	Chunking       engine.ChunkingParams
	Compression    engine.CompressionPolicy
	Events         EventBuilder
	Reader         *readpath.WarmReader
	BucketPolicy   func(bucketID string) (config.BucketSnapshot, error)
	Budget         readpath.Budget
	MaxOutstanding int
	RequestTimeout time.Duration
}

type Service struct{ o Options }

func New(o Options) (*Service, error) {
	switch {
	case o.Metadata == nil, o.Packs == nil, o.Registry == nil, o.Clock == nil, o.Events == nil, o.Reader == nil:
		return nil, storage.ErrInvalid
	case o.AdmitWriter == nil:
		return nil, storage.ErrInvalid
	case o.BucketPolicy == nil, o.MaxOutstanding <= 0, o.RequestTimeout <= 0:
		return nil, storage.ErrInvalid
	case o.Chunking.Validate() != nil, o.Compression.Validate() != nil:
		return nil, storage.ErrInvalid
	}
	return &Service{o: o}, nil
}

// PutObject streams one logical body into packs and manifest pages, then
// publishes a head only after the whole body passed the API checksum checks.
func (s *Service) PutObject(ctx context.Context, r PutRequest) (PutResult, error) {
	if ctx == nil || r.Body == nil || r.ContentLength < 0 || r.Key == "" || r.UserID == "" || r.CredentialID == "" ||
		r.Bucket.Name == "" || r.Bucket.ID == "" || r.Bucket.OwnerID == "" || r.Bucket.Domain == "" {
		return PutResult{}, storage.ErrInvalid
	}
	if err := ValidatePut(r.ContentLength, r.Metadata); err != nil {
		return PutResult{}, err
	}
	object := storage.ObjectKey{Bucket: r.Bucket.Name, BucketID: r.Bucket.ID, Key: r.Key}
	if object.Validate() != nil {
		return PutResult{}, storage.ErrInvalid
	}
	base, err := s.o.Metadata.GetObject(ctx, object)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return PutResult{}, err
	}
	var baseRevision uint64
	var baseSize int64
	if err == nil {
		baseRevision, baseSize = base.Revision, base.Size
		if base.Deleted {
			baseSize = 0
		}
	}
	session, opID, fence, wallDeadline, err := s.begin(ctx, object, r.CredentialID)
	if err != nil {
		return PutResult{}, err
	}
	open := true
	defer func() {
		if open {
			_ = session.Close()
		}
	}()

	reservationID := "reservation-" + opID
	reserve := storage.Reservation{
		Version: storage.ModelVersion, OpID: opID, BaseRevision: baseRevision, BaseSize: baseSize,
		Revision: 1, State: storage.ReservationActive, Deadline: wallDeadline, ID: reservationID,
		OwnerID: r.Bucket.OwnerID, Object: object, Bytes: max(0, r.ContentLength-baseSize), StagedBytes: r.ContentLength,
		Writer: fence,
	}
	if err := session.Reserve(reserve); err != nil {
		return PutResult{}, fmt.Errorf("reserve: %w", err)
	}
	manifestID, err := write.NewManifestID()
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return PutResult{}, err
	}
	prepared, err := session.Prepare(write.Pipeline{Chunking: s.o.Chunking, Compression: s.o.Compression}, r.Body, manifestID)
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return PutResult{}, fmt.Errorf("prepare: %w", err)
	}
	if prepared.Size != r.ContentLength {
		s.release(ctx, reservationID, opID, fence)
		return PutResult{}, ErrLengthMismatch
	}
	etag, err := putETag(r.Body)
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return PutResult{}, err
	}
	algorithm, digest := r.Body.Checksum()
	if algorithm == "" || len(digest) == 0 {
		s.release(ctx, reservationID, opID, fence)
		return PutResult{}, storage.ErrInvalid
	}
	checksum := storage.Checksum{Algorithm: algorithm, Type: "FULL_OBJECT", Value: base64.StdEncoding.EncodeToString(digest)}
	metadata := make(map[string]string, len(r.Metadata)+1)
	for k, v := range r.Metadata {
		metadata[k] = v
	}
	metadata[commitTimeKey] = s.o.Clock.Now().WallUTC.UTC().Format(time.RFC3339Nano)
	head := storage.ObjectHead{
		Version: storage.ModelVersion, Object: object, Domain: r.Bucket.Domain,
		ManifestID: prepared.ManifestID, Size: prepared.Size,
		ETag: etag, ETagKind: "put-md5", Checksum: checksum,
		Metadata: metadata, OpID: opID,
	}
	head, err = prepared.BindHead(head)
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return PutResult{}, err
	}
	event, err := s.o.Events.ObjectCommit(ctx, CommitEvent{OpID: opID, Object: object, Revision: baseRevision + 1, Head: head})
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		return PutResult{}, err
	}
	commit := storage.CommitRequest{
		ReservationRevision: 1, Head: head, ExpectedRevision: baseRevision,
		UserID: r.UserID, CredentialID: r.CredentialID, ReservationID: reservationID, Writer: fence, Event: event,
	}
	open = false
	result, err := s.publish(ctx, session, commit)
	if err != nil {
		return PutResult{}, err
	}
	return putResult(result.Head), nil
}

// DeleteObject publishes a tombstone revision (API v2 4.2): one R2 commit
// credits the old size to the bucket owner's quota and records the outbox
// event. An absent key or an existing tombstone publishes nothing, so a
// repeated DELETE never credits quota twice; HTTP answers 204 either way.
func (s *Service) DeleteObject(ctx context.Context, r DeleteRequest) error {
	if ctx == nil || r.Key == "" || r.UserID == "" || r.CredentialID == "" ||
		r.Bucket.Name == "" || r.Bucket.ID == "" || r.Bucket.OwnerID == "" || r.Bucket.Domain == "" {
		return storage.ErrInvalid
	}
	object := storage.ObjectKey{Bucket: r.Bucket.Name, BucketID: r.Bucket.ID, Key: r.Key}
	if object.Validate() != nil {
		return storage.ErrInvalid
	}
	base, err := s.o.Metadata.GetObject(ctx, object)
	switch {
	case errors.Is(err, storage.ErrBucketNotFound):
		return err
	case errors.Is(err, storage.ErrNotFound):
		return nil
	case err != nil:
		return err
	case base.Deleted:
		return nil
	}
	session, opID, fence, wallDeadline, err := s.begin(ctx, object, r.CredentialID)
	if err != nil {
		return err
	}
	reservationID := "reservation-" + opID
	reserve := storage.Reservation{
		Version: storage.ModelVersion, OpID: opID, BaseRevision: base.Revision, BaseSize: base.Size,
		Revision: 1, State: storage.ReservationActive, Deadline: wallDeadline, ID: reservationID,
		OwnerID: r.Bucket.OwnerID, Object: object, Writer: fence,
	}
	if err := session.Reserve(reserve); err != nil {
		_ = session.Close()
		return fmt.Errorf("reserve: %w", err)
	}
	head := storage.ObjectHead{Version: storage.ModelVersion, Object: object, Domain: r.Bucket.Domain, OpID: opID, Deleted: true}
	event, err := s.o.Events.ObjectCommit(ctx, CommitEvent{OpID: opID, Object: object, Revision: base.Revision + 1, Head: head})
	if err != nil {
		s.release(ctx, reservationID, opID, fence)
		_ = session.Close()
		return err
	}
	_, err = s.publish(ctx, session, storage.CommitRequest{
		ReservationRevision: 1, Head: head, ExpectedRevision: base.Revision,
		UserID: r.UserID, CredentialID: r.CredentialID, ReservationID: reservationID, Writer: fence, Event: event,
	})
	return err
}

// ListObjects returns one logical page of committed objects: the store skips
// tombstones and groups CommonPrefixes; the request is a trusted internal
// port whose principal comes from the router's authentication.
func (s *Service) ListObjects(ctx context.Context, r storage.ListObjectsRequest) (ListPage, error) {
	if ctx == nil {
		return ListPage{}, storage.ErrInvalid
	}
	page, err := s.o.Metadata.ListObjects(ctx, r)
	if err != nil {
		return ListPage{}, err
	}
	out := ListPage{Objects: make([]ListEntry, len(page.Objects)), CommonPrefixes: page.CommonPrefixes, Next: page.Next}
	for i, h := range page.Objects {
		out.Objects[i] = ListEntry{Key: h.Object.Key, ETag: h.ETag, Size: h.Size, LastModified: lastModified(h)}
	}
	return out, nil
}

// begin admits one logical write of object and opens its managed session. The
// caller must end the session through publish or Close.
func (s *Service) begin(ctx context.Context, object storage.ObjectKey, credentialID string) (*write.WriteSession, string, storage.WriterFence, time.Time, error) {
	deadline, wallDeadline, err := s.deadline(ctx)
	if err != nil {
		return nil, "", storage.WriterFence{}, time.Time{}, err
	}
	opID, err := write.NewManifestID()
	if err != nil {
		return nil, "", storage.WriterFence{}, time.Time{}, err
	}
	admitted, err := s.o.AdmitWriter(ctx, WriterRequest{Object: object, OpID: opID, CredentialID: credentialID, Deadline: deadline, WallDeadline: wallDeadline})
	if err != nil {
		return nil, "", storage.WriterFence{}, time.Time{}, err
	}
	if admitted.Fence.LeaseID == "" || admitted.Fence.ServiceIncarnation == "" || admitted.Fence.Epoch == 0 || admitted.ProtectedUntil.Before(wallDeadline.Add(15*time.Minute)) {
		return nil, "", storage.WriterFence{}, time.Time{}, storage.ErrFenced
	}
	session, err := write.BeginWriteSession(ctx, write.SessionOptions{
		Registry: s.o.Registry, Metadata: s.o.Metadata, Packs: s.o.Packs, Fence: admitted.Fence,
		Deadline: deadline, Object: object, OpID: opID, CredentialID: credentialID, ProtectedUntil: admitted.ProtectedUntil,
	})
	if err != nil {
		return nil, "", storage.WriterFence{}, time.Time{}, fmt.Errorf("begin: %w", err)
	}
	return session, opID, admitted.Fence, wallDeadline, nil
}

// publish commits a head or tombstone through session and always closes it.
// A known failure releases the reservation; an unknown outcome is reconciled
// first, because a missing outcome is not proof that the commit did not happen.
func (s *Service) publish(ctx context.Context, session *write.WriteSession, commit storage.CommitRequest) (storage.CommitResult, error) {
	var result storage.CommitResult
	var err error
	if commit.Head.Deleted {
		result, err = session.Delete(commit)
	} else {
		result, err = session.Commit(commit)
	}
	if err == nil {
		// The head is committed; a registry release failure must not turn a
		// durable success into a client-visible error.
		_ = session.Close()
		return result, nil
	}
	err = fmt.Errorf("commit: %w", err)
	if !unknownOutcome(err) {
		s.release(ctx, commit.ReservationID, commit.Head.OpID, commit.Writer)
		_ = session.Close()
		return storage.CommitResult{}, err
	}
	_ = session.Close()
	reconcileCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	committed, reconcileErr := session.Reconcile(reconcileCtx)
	cancel()
	if reconcileErr == nil && committed.Head.Revision > 0 {
		return committed, nil
	} else if errors.Is(reconcileErr, storage.ErrNotFound) {
		s.release(ctx, commit.ReservationID, commit.Head.OpID, commit.Writer)
	}
	return storage.CommitResult{}, err
}

func putResult(head storage.ObjectHead) PutResult {
	return PutResult{ETag: head.ETag, ChecksumAlgorithm: head.Checksum.Algorithm, ChecksumValue: head.Checksum.Value}
}

func putETag(body Body) (string, error) {
	sum := body.MD5()
	if len(sum) != md5.Size {
		return "", storage.ErrInvalid
	}
	return `"` + hex.EncodeToString(sum) + `"`, nil
}

// GetObject streams a Warm read. The Ready callback runs after preflight and
// before the first emitted byte; an error there aborts with no headers sent.
func (s *Service) GetObject(ctx context.Context, r GetRequest) (GetResult, error) {
	if ctx == nil || r.Bucket == "" || r.Key == "" || r.CredentialID == "" || r.Emit == nil {
		return GetResult{}, storage.ErrInvalid
	}
	deadline, _, err := s.deadline(ctx)
	if err != nil {
		return GetResult{}, fmt.Errorf("get deadline: %w", err)
	}
	policy, err := s.o.BucketPolicy(r.BucketID)
	if err != nil {
		return GetResult{}, fmt.Errorf("get policy: %w", err)
	}
	var selectRange func(storage.ObjectHead) (*storage.ByteRange, error)
	if r.SelectRange != nil {
		selectRange = func(head storage.ObjectHead) (*storage.ByteRange, error) { return r.SelectRange(info(head)) }
	}
	var result GetResult
	_, err = s.o.Reader.Read(ctx, readpath.WarmRequest{
		CredentialID: r.CredentialID, Bucket: r.Bucket, Key: r.Key, Range: r.Range, SelectRange: selectRange,
		Deadline: deadline, BucketPolicy: policy, Budget: s.o.Budget, MaxOutstanding: s.o.MaxOutstanding,
		Ready: func(w readpath.WarmResult) error {
			result = GetResult{Info: info(w.Head), Range: w.Range}
			if r.Ready != nil {
				return r.Ready(result)
			}
			return nil
		},
	}, r.Emit)
	if err != nil {
		err = fmt.Errorf("read: %w", err)
		switch {
		case errors.Is(err, readpath.ErrExhausted):
			return result, storage.ErrUnavailable
		case errors.Is(err, readpath.ErrAdmissionInvalid):
			return result, storage.ErrInvalid
		}
		return result, err
	}
	return result, nil
}

// Stat reports the committed head for HEAD without reading pack payload.
func (s *Service) Stat(ctx context.Context, r GetRequest) (GetResult, error) {
	if ctx == nil || r.Bucket == "" || r.Key == "" || r.CredentialID == "" {
		return GetResult{}, storage.ErrInvalid
	}
	auth, err := s.o.Metadata.Authorize(ctx, r.CredentialID, r.Bucket)
	if err != nil {
		return GetResult{}, err
	}
	if !auth.Bucket.Active || !(auth.Read || auth.UserID == auth.Bucket.OwnerID) {
		return GetResult{}, storage.ErrAccessDenied
	}
	if r.BucketID != "" && auth.Bucket.ID != r.BucketID {
		return GetResult{}, storage.ErrConflict
	}
	head, err := s.o.Metadata.GetObject(ctx, storage.ObjectKey{Bucket: r.Bucket, BucketID: auth.Bucket.ID, Key: r.Key})
	if err != nil {
		return GetResult{}, err
	}
	if head.Deleted {
		return GetResult{}, storage.ErrNotFound
	}
	result := GetResult{Info: info(head), Range: storage.ByteRange{Offset: 0, Length: head.Size}}
	if r.Range != nil {
		if err := r.Range.Validate(head.Size); err != nil {
			return result, storage.ErrInvalid
		}
		result.Range = *r.Range
	}
	return result, nil
}

func info(h storage.ObjectHead) Info {
	metadata := make(map[string]string, len(h.Metadata))
	for k, v := range h.Metadata {
		if k == commitTimeKey || strings.HasPrefix(k, "x-dsg-") {
			continue
		}
		metadata[k] = v
	}
	return Info{ETag: h.ETag, Size: h.Size, LastModified: lastModified(h), Metadata: metadata, Checksum: h.Checksum}
}

// lastModified is the commit wall time the head carries under commitTimeKey,
// or the Unix epoch for a head without one: clients reject an empty timestamp.
func lastModified(h storage.ObjectHead) time.Time {
	if v := h.Metadata[commitTimeKey]; v != "" {
		if t, err := time.Parse(time.RFC3339Nano, v); err == nil {
			return t.UTC()
		}
	}
	return time.Unix(0, 0).UTC()
}

func (s *Service) deadline(ctx context.Context) (clock.Instant, time.Time, error) {
	reading := s.o.Clock.Now()
	budget := s.o.RequestTimeout
	if at, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(at))
		if budget <= 0 {
			return clock.Instant{}, time.Time{}, context.DeadlineExceeded
		}
	}
	instant, err := reading.Instant.Add(budget)
	if err != nil {
		return clock.Instant{}, time.Time{}, err
	}
	return instant, reading.WallUTC.Add(budget), nil
}

func (s *Service) release(ctx context.Context, reservationID, opID string, fence storage.WriterFence) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = s.o.Metadata.ReleaseLogicalQuota(releaseCtx, storage.ReservationRelease{
		ID: reservationID, OpID: opID, ExpectedRevision: 1, Writer: fence,
	})
}

func unknownOutcome(err error) bool {
	return errors.Is(err, storage.ErrUnavailable) || errors.Is(err, storage.ErrFenced) || errors.Is(err, coordination.ErrReconcilePending)
}
