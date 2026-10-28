package write

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/storage"
)

// SessionOptions bind one logical operation to an already admitted backend
// fence. No lease is written to metadata here. ProtectedUntil is supplied by the
// caller's existing protection policy; this API cannot invent UTC evidence.
type SessionOptions struct {
	Registry           *coordination.WriterRegistry
	Metadata           CoordinationStore
	Packs              storage.PackStore
	Fence              storage.WriterFence
	Deadline           clock.Instant
	Object             storage.ObjectKey
	OpID, CredentialID string
	ProtectedUntil     time.Time
}

// WriteSession owns one operation's registration, heartbeat and CPU/I/O lifetime.
// Methods serialize per session, not globally across GETs. Close cancels first,
// then waits for the running method/body reader to return before releasing work.
// A body reader which ignores cancellation cannot justify early cleanup.
type WriteSession struct{ *writeSession }
type writeSession struct {
	mu                        sync.Mutex
	ctx                       context.Context
	cancel                    context.CancelFunc
	stopHeartbeat             func()
	writer                    *coordination.Writer
	work                      *coordination.WriterWork
	metadata                  *CoordinatedMetadata
	packs                     storage.PackStore
	auth                      storage.Authorization
	object                    storage.ObjectKey
	opID                      string
	protectedUntil            time.Time
	prepared                  *PreparedObject
	closed, failed, published bool
}

func BeginWriteSession(ctx context.Context, o SessionOptions) (*WriteSession, error) {
	if ctx == nil || o.Registry == nil || o.Metadata == nil || o.Packs == nil || o.Object.Validate() != nil || o.OpID == "" || o.CredentialID == "" || o.ProtectedUntil.IsZero() {
		return nil, storage.ErrInvalid
	}
	if c, ok := storage.MutationFromContext(ctx); ok {
		if c.Validate() != nil || c.Object != o.Object || c.Writer != o.Fence || c.MutationID != o.OpID || c.LogicalOpID != o.OpID {
			return nil, storage.ErrFenced
		}
	}
	auth, err := o.Metadata.Authorize(ctx, o.CredentialID, o.Object.Bucket)
	if err != nil {
		return nil, err
	}
	if auth.UserID == "" || auth.CredentialID != o.CredentialID || auth.Bucket.Name != o.Object.Bucket || auth.Bucket.Domain == "" {
		return nil, storage.ErrUnavailable
	}
	if !auth.Bucket.Active || !(auth.Write || auth.UserID == auth.Bucket.OwnerID) {
		return nil, storage.ErrAccessDenied
	}
	if auth.Bucket.ID != o.Object.BucketID {
		return nil, storage.ErrConflict
	}
	writer, err := o.Registry.Register(ctx, o.Fence, o.Deadline)
	if err != nil {
		return nil, err
	} // unknown registration must be retried with same binding
	work, err := writer.BeginOperation(ctx, o.Object, o.OpID)
	if err != nil {
		// A duplicate executing session must not close the legitimate owner.
		if !errors.Is(err, storage.ErrConflict) {
			_ = writer.Release()
		}
		return nil, err
	}
	base, cancel := context.WithCancel(ctx)
	live, stop, err := writer.KeepAlive(base)
	if err != nil {
		cancel()
		work.Finish()
		return nil, errors.Join(err, writer.Release())
	}
	metadata, packs, err := BindCoordination(o.Metadata, o.Packs, writer)
	if err != nil {
		cancel()
		stop()
		work.Finish()
		return nil, errors.Join(err, writer.Release())
	}
	return &WriteSession{writeSession: &writeSession{ctx: live, cancel: cancel, stopHeartbeat: stop, writer: writer, work: work, metadata: metadata, packs: packs, auth: auth, object: o.Object, opID: o.OpID, protectedUntil: o.ProtectedUntil}}, nil
}

func (s *WriteSession) active() error {
	if s.closed || s.published {
		return storage.ErrFenced
	}
	if s.failed {
		return coordination.ErrReconcilePending
	}
	return s.writer.Check(s.ctx)
}

// Prepare supplies protected ports even if the pipeline template contains other
// Lookup/Publisher values. The caller provides chunking/compression settings,
// while the admitted bucket fixes the dedup domain. Returned descriptor is owned.
func (s *WriteSession) Prepare(pipeline Pipeline, body io.Reader, manifestID string) (PreparedObject, error) {
	if s == nil || s.writeSession == nil || body == nil {
		return PreparedObject{}, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.active(); err != nil {
		return PreparedObject{}, err
	}
	if s.prepared != nil {
		return PreparedObject{}, storage.ErrConflict
	}
	pipeline.Lookup = s.metadata
	pipeline.Publisher = &StorePublisher{Metadata: s.metadata, Packs: s.packs, Guard: s.metadata, Writer: s.writer.Fence(), ProtectedUntil: s.protectedUntil}
	p, err := PrepareObject(s.ctx, pipeline, s.metadata, s.writer.Fence(), body, s.auth.Bucket.Domain, manifestID)
	if err != nil {
		s.failed = true
		return PreparedObject{}, err
	}
	owned := p
	owned.PageIndex = append([]uint64(nil), p.PageIndex...)
	s.prepared = &owned
	return p, nil
}

func (s *WriteSession) Reserve(r storage.Reservation) error {
	if s == nil || s.writeSession == nil {
		return storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.active(); err != nil {
		return err
	}
	if r.OpID != s.opID || r.Object != s.object || r.Writer != s.writer.Fence() || r.OwnerID != s.auth.Bucket.OwnerID {
		return storage.ErrInvalid
	}
	err := s.metadata.ReserveLogicalQuota(s.ctx, r)
	if err != nil {
		s.failed = true
	}
	return err
}

func (s *WriteSession) publish(r storage.CommitRequest, deleting bool) (storage.CommitResult, error) {
	if s == nil || s.writeSession == nil {
		return storage.CommitResult{}, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.active(); err != nil {
		return storage.CommitResult{}, err
	}
	if r.Head.Object != s.object || r.Head.OpID != s.opID || r.Head.Domain != s.auth.Bucket.Domain || r.UserID != s.auth.UserID || r.CredentialID != s.auth.CredentialID || r.Writer != s.writer.Fence() || r.Head.Deleted != deleting {
		return storage.CommitResult{}, storage.ErrInvalid
	}
	if !deleting {
		if s.prepared == nil {
			return storage.CommitResult{}, storage.ErrInvalid
		}
		var err error
		r.Head, err = s.prepared.BindHead(r.Head)
		if err != nil {
			return storage.CommitResult{}, err
		}
	}
	if err := r.Validate(); err != nil {
		return storage.CommitResult{}, err
	}
	var out storage.CommitResult
	var err error
	if deleting {
		out, err = s.metadata.DeleteObject(s.ctx, r)
	} else {
		out, err = s.metadata.CommitObject(s.ctx, r)
	}
	if err != nil {
		s.failed = true
		return storage.CommitResult{}, err
	}
	s.published = true
	return out, nil
}
func (s *WriteSession) Commit(r storage.CommitRequest) (storage.CommitResult, error) {
	return s.publish(r, false)
}
func (s *WriteSession) Delete(r storage.CommitRequest) (storage.CommitResult, error) {
	return s.publish(r, true)
}

func (s *WriteSession) Close() error {
	if s == nil || s.writeSession == nil {
		return storage.ErrInvalid
	}
	s.cancel()            // do not wait for a body/backend/emitter while withholding cancellation
	s.writer.CloseFence() // close attachment/dispatch before quiescence and cleanup
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.stopHeartbeat()
		s.work.Finish()
	}
	return s.writer.Release()
}

// Reconcile reads the original outcome after Close; a fresh reconciliation
// context is independent of the canceled execution context. It never retries
// publication and ErrNotFound never clears the original unknown operation.
func (s *WriteSession) Reconcile(ctx context.Context) (storage.CommitResult, error) {
	if s == nil || s.writeSession == nil || ctx == nil {
		return storage.CommitResult{}, storage.ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		return storage.CommitResult{}, coordination.ErrWorkPending
	}
	out, err := s.writer.ReconcileCommit(ctx, s.metadata, s.opID)
	if err != nil {
		return storage.CommitResult{}, err
	}
	if err := s.writer.Release(); err != nil {
		return storage.CommitResult{}, err
	}
	return out, nil
}
