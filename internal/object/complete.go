package object

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/multipart/core"
	"github.com/KinasTomes/miles/internal/storage"
)

// receiptRetention is how long a Completed upload answers Complete with its
// receipt (API v2 4.3 step 3).
const receiptRetention = 7 * 24 * time.Hour

// CompleteRequest is a CompleteMultipartUpload that passed the upload binding,
// rows 1-5 of API v2 4.3.1 (Input) and the state table.
type CompleteRequest struct {
	Upload               storage.MultipartUpload // as read for the binding check
	Input                core.Input
	UserID, CredentialID string
}

// CompleteUpload publishes the object of an upload by API v2 4.3 steps 1-5
// and returns its committed head. A Completed upload answers its receipt; an
// Active one is frozen with Input; a Completing one (past its lease and grace)
// is taken over under the same completion op.
func (s *Service) CompleteUpload(ctx context.Context, r CompleteRequest) (storage.ObjectHead, error) {
	u := r.Upload
	if s.o.Uploads == nil || ctx == nil || r.UserID == "" || r.CredentialID == "" || u.ID == "" {
		return storage.ObjectHead{}, storage.ErrInvalid
	}
	if u.State == "Completed" {
		return s.receipt(ctx, u.ID, r.CredentialID, r.Input.Digest())
	}
	return s.complete(ctx, u, r.Input, r.UserID, r.CredentialID)
}

// RecoverUpload is recover_uploads for one upload: a Completing upload past
// its completion lease and grace is taken over with its frozen input and the
// upload's own credential, then completed, reopened or aborted like a client
// takeover. Expiry alone never closes a Completing upload.
func (s *Service) RecoverUpload(ctx context.Context, id string) (storage.ObjectHead, error) {
	if s.o.Uploads == nil || ctx == nil {
		return storage.ObjectHead{}, storage.ErrInvalid
	}
	u, err := s.o.Uploads.GetUpload(ctx, id)
	if err != nil {
		return storage.ObjectHead{}, err
	}
	if u.State != "Completing" {
		return storage.ObjectHead{}, storage.ErrConflict
	}
	var b bytes.Buffer
	err = manifest.VerifyCompletionStream(u.Input,
		func(n uint32) ([]byte, error) {
			return s.o.Uploads.ReadCompletionPage(ctx, u.Input.Kind, u.Input.Owner, n)
		},
		func(p []byte) error { _, e := b.Write(p); return e })
	if err != nil {
		return storage.ObjectHead{}, err
	}
	var in core.Input
	if json.Unmarshal(b.Bytes(), &in) != nil {
		return storage.ObjectHead{}, storage.ErrInvalid
	}
	return s.complete(ctx, u, in, u.Initiator, u.Credential)
}

func (s *Service) receipt(ctx context.Context, id, credentialID, digest string) (storage.ObjectHead, error) {
	r, err := s.o.Uploads.GetMultipartReceipt(ctx, storage.MultipartRequest{UploadID: id, Credential: credentialID}, digest)
	return r.Head, err
}

// renewEvery is the completion lease heartbeat of API v2 4.3 (lease 60 s).
const renewEvery = 15 * time.Second

// completion is one attempt at an upload's completion op: its admitted writer
// and the latest upload record the attempt wrote or read. Once run starts,
// the lease heartbeat shares u, so it is read and written under mu.
type completion struct {
	s                *Service
	ctx              context.Context
	session          *write.WriteSession
	mu               sync.Mutex
	u                storage.MultipartUpload
	op               string
	fence            storage.WriterFence
	user, credential string
	takeover         bool
}

func (c *completion) upload() storage.MultipartUpload {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.u
}

// observe keeps the newest record of this attempt: a reply that arrives
// after a newer CAS of the same fence never moves u back.
func (c *completion) observe(u storage.MultipartUpload) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u.Fence == c.u.Fence && u.Revision > c.u.Revision {
		c.u = u
	}
}

// request is the next CAS on the upload this attempt holds.
func (c *completion) request() storage.MultipartRequest {
	u := c.upload()
	return storage.MultipartRequest{UploadID: u.ID, Credential: c.credential, OpID: c.op,
		ExpectedRevision: u.Revision, Fence: u.Fence, Writer: c.fence}
}

func (s *Service) complete(ctx context.Context, u storage.MultipartUpload, in core.Input, userID, credentialID string) (storage.ObjectHead, error) {
	takeover := u.State == "Completing"
	if in.BucketID != u.Object.BucketID || in.Key != u.Object.Key || takeover && in.Digest() != u.Input.SHA256 {
		return storage.ObjectHead{}, storage.ErrInvalid
	}
	// After the first freeze every attempt, takeover or not, keeps the
	// upload's completion op: its outcome and receipt are keyed by it.
	ctx, session, op, fence, _, err := s.begin(ctx, u.Object, credentialID, u.CompletionOp)
	if err != nil {
		return storage.ObjectHead{}, err
	}
	c := &completion{s: s, ctx: ctx, session: session, u: u, op: op, fence: fence, user: userID, credential: credentialID, takeover: takeover}
	if takeover {
		err = c.claim()
	} else {
		err = c.freeze(in)
	}
	if err != nil {
		_ = session.Close()
		return storage.ObjectHead{}, err
	}
	head, err := c.run(in)
	if err == nil {
		_ = session.Close()
		return head, nil
	}
	return c.fail(err)
}

// freeze persists the normalized input as kind-3 pages, then CASes the upload
// Active to Completing with its digest: no part registers or publishes after.
func (c *completion) freeze(in core.Input) error {
	owner, err := write.NewManifestID()
	if err != nil {
		return err
	}
	d, err := manifest.WriteCompletionStream(manifest.CompletionInput, owner, bytes.NewReader(in.Canonical()), func(n uint32, b []byte) error {
		return c.s.o.Uploads.WriteCompletionPage(c.ctx, manifest.CompletionInput, owner, n, b, c.fence)
	})
	if err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		u, err := c.s.o.Uploads.FreezeCompletion(c.ctx, storage.FreezeRequest{Request: c.request(), Input: d})
		if err == nil {
			c.u = u
			return nil
		}
		if !errors.Is(err, storage.ErrConflict) || attempt == maxPartAttempts {
			return err
		}
		// A part publish or cancellation moved the revision.
		if c.u, err = c.s.o.Uploads.GetUpload(c.ctx, c.u.ID); err != nil {
			return err
		}
		if c.u.State != "Active" {
			return storage.ErrConflict
		}
	}
}

// claim takes a Completing upload over with a new fence once its lease and
// grace have passed, even past expires_at (API v2 4.3.1). A revoked
// initiator cannot resume it: the attempt is closed and the upload aborted,
// releasing once.
func (c *completion) claim() error {
	u, err := c.s.o.Uploads.ClaimCompletion(c.ctx, c.request())
	if err == nil {
		c.u = u
		return nil
	}
	if errors.Is(err, storage.ErrAccessDenied) {
		c.abandon()
	}
	return err
}

// run is steps 1-3 after the freeze: close every provisional publisher,
// snapshot and check the selected part heads (rows 6-11), prepare the object
// manifest and commit head, quota, outbox and receipt in one transaction.
// The completion lease is renewed every 15 s meanwhile, so a long snapshot
// or prepare keeps its fence.
func (c *completion) run(in core.Input) (storage.ObjectHead, error) {
	stop := c.keepAlive()
	defer stop()
	if err := c.closePublishers(); err != nil {
		return storage.ObjectHead{}, err
	}
	snapshot, err := c.snapshot(in)
	if err != nil {
		return storage.ObjectHead{}, err
	}
	manifestID, err := write.NewManifestID()
	if err != nil {
		return storage.ObjectHead{}, err
	}
	_, err = c.session.PrepareCompletion(manifestID, func(ctx context.Context, _ storage.WriterFence) (storage.ObjectHead, error) {
		// Prepare is bound to this attempt's fence, not to the revision the
		// heartbeat keeps moving.
		u, err := c.s.o.Uploads.PrepareCompletion(ctx, storage.PrepareCompletionRequest{Request: c.request(), Snapshot: snapshot,
			Head: storage.ObjectHead{Object: c.upload().Object, ManifestID: manifestID}})
		if err != nil {
			return storage.ObjectHead{}, err
		}
		c.observe(u)
		return u.Prepared, nil
	})
	if err != nil {
		return storage.ObjectHead{}, err
	}
	// The commit conditions on the exact Completing revision: stop the
	// heartbeat, then one renewal rechecks the initiator's right and fixes
	// the revision the transaction expects.
	stop()
	if err := c.renew(); err != nil {
		return storage.ObjectHead{}, err
	}
	return c.commit()
}

// keepAlive renews the completion lease every renewEvery until the returned
// stop, which waits for an in-flight renewal. A failed renewal is retried on
// the next beat; losing the lease fails the step that needs it.
func (c *completion) keepAlive() (stop func()) {
	ctx, cancel := context.WithCancel(c.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for c.s.o.Clock.Sleep(ctx, renewEvery) == nil {
			_ = c.renew()
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { cancel(); <-done }) }
}

// closePublishers cancels every publisher still open at the freeze: the
// cancellation CAS on candidate and upload fences its late publish, and its
// provisional bytes return. Packs it wrote wait for GC.
func (c *completion) closePublishers() error {
	for after := ""; ; {
		page, err := c.s.o.Uploads.ListPublishers(c.ctx, c.upload().ID, after, 1000)
		if err != nil {
			return err
		}
		for _, p := range page.Publishers {
			if p.State != "Open" {
				continue
			}
			if err := c.s.o.Uploads.CancelPartPublisher(c.ctx, c.request(), p.ID); err != nil {
				return err
			}
		}
		if page.Next == "" {
			break
		}
		after = page.Next
	}
	u, err := c.s.o.Uploads.GetUpload(c.ctx, c.upload().ID)
	if err != nil {
		return err
	}
	if u.State != "Completing" || u.Fence != c.upload().Fence || u.Publishers != 0 || u.ProvisionalBytes != 0 {
		return storage.ErrUnavailable
	}
	c.observe(u)
	return nil
}

// snapshot reads the current head of every selected part, checks rows 6-11
// on them and persists them as the immutable kind-4 frozen snapshot.
func (c *completion) snapshot(in core.Input) (storage.StreamDescriptor, error) {
	heads := make(map[int]core.Head, len(in.Parts))
	parts := make(map[int]storage.UploadPart, len(in.Parts))
	for _, p := range in.Parts {
		part, err := c.s.o.Uploads.GetPart(c.ctx, c.request(), uint32(p.PartNumber))
		if errors.Is(err, storage.ErrNotFound) {
			continue // InvalidPart below
		}
		if err != nil {
			return storage.StreamDescriptor{}, err
		}
		parts[p.PartNumber] = part
		heads[p.PartNumber] = core.Head{PartNumber: p.PartNumber, ETag: part.ETag, Size: part.Size, Checksum: part.Checksum.Value}
	}
	if _, _, err := core.Assemble(in, heads); err != nil {
		return storage.StreamDescriptor{}, err
	}
	frozen := make([]manifest.SnapshotPart, len(in.Parts))
	for i, p := range in.Parts {
		h := parts[p.PartNumber]
		frozen[i] = manifest.SnapshotPart{Number: h.Number, Revision: h.Revision, Size: uint64(h.Size), Pages: h.Pages,
			ETag: h.ETag, ManifestID: h.ManifestID, Checksum: h.Checksum.Value}
	}
	body, err := manifest.EncodeSnapshot(frozen)
	if err != nil {
		return storage.StreamDescriptor{}, err
	}
	owner, err := write.NewManifestID()
	if err != nil {
		return storage.StreamDescriptor{}, err
	}
	return manifest.WriteCompletionStream(manifest.CompletionSnapshot, owner, bytes.NewReader(body), func(n uint32, b []byte) error {
		return c.s.o.Uploads.WriteCompletionPage(c.ctx, manifest.CompletionSnapshot, owner, n, b, c.fence)
	})
}

// renew extends this attempt's lease. Renewal CASes the revision, which this
// attempt's own cancellations, prepare or heartbeat may have moved: it
// rereads the upload and retries while the fence is still this attempt's.
func (c *completion) renew() error {
	for attempt := 1; ; attempt++ {
		u, err := c.s.o.Uploads.RenewCompletion(c.ctx, c.request())
		if err == nil {
			c.observe(u)
			return nil
		}
		if !errors.Is(err, storage.ErrConflict) || attempt == maxPartAttempts {
			return err
		}
		current, e := c.s.o.Uploads.GetUpload(c.ctx, c.upload().ID)
		if e != nil {
			return e
		}
		if current.State != "Completing" || current.Fence != c.upload().Fence {
			return err
		}
		c.observe(current)
	}
}

// commit is the R2 transaction of step 3: head CAS, quota, outbox and the
// Completed upload with its receipt, conditioned on this attempt's fence.
func (c *completion) commit() (storage.ObjectHead, error) {
	u := c.u
	base, err := c.s.o.Metadata.GetObject(c.ctx, u.Object)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return storage.ObjectHead{}, err
	}
	ledger, err := c.s.o.Uploads.GetReservation(c.ctx, u.ReservationID)
	if err != nil {
		return storage.ObjectHead{}, err
	}
	head := u.Prepared
	event, err := c.s.o.Events.ObjectCommit(c.ctx, CommitEvent{OpID: c.op, Object: u.Object, Revision: base.Revision + 1, Head: head})
	if err != nil {
		return storage.ObjectHead{}, err
	}
	r := storage.CommitRequest{
		MultipartAttempt: u.Fence, ReservationRevision: ledger.Revision, Head: head, ExpectedRevision: base.Revision,
		UserID: c.user, CredentialID: c.credential, ReservationID: u.ReservationID, Writer: c.fence, Event: event,
		Receipt: &storage.CompletedReceipt{Version: storage.ModelVersion, UploadID: u.ID, BucketID: u.Object.BucketID, Key: u.Object.Key,
			Initiator: u.Initiator, OpID: c.op, InputDigest: u.Input.SHA256, ManifestID: head.ManifestID, ETag: head.ETag,
			Checksum: head.Checksum, ExpiresAt: c.s.o.Clock.Now().WallUTC.Add(receiptRetention)},
		Completion: &storage.Completion{UploadID: u.ID, BucketID: u.Object.BucketID, Key: u.Object.Key, Initiator: u.Initiator,
			Revision: u.Revision, Fence: u.Fence, State: u.State},
	}
	if producer, ok := storage.MutationFromContext(c.ctx); ok {
		r.Producer = &producer
	}
	result, err := c.session.Commit(r)
	return result.Head, err
}

// fail settles an attempt that failed after its freeze or claim. A possibly
// committed transaction is decided only by its outcome or the receipt; with
// neither the upload stays Completing with its roots and reservation until a
// retry or recover_uploads takes it over. A failure of rows 6-13 (the store
// writer reports a rejected commit as unknown until its outcome is proven
// absent) reopens the upload Active, except that a takeover denied the
// initiator's right aborts. Reopen and abort both close the completion op
// first, so a late commit of this attempt can no longer land.
func (c *completion) fail(err error) (storage.ObjectHead, error) {
	_ = c.session.Close()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	if unknownOutcome(err) {
		out, e := c.session.Reconcile(ctx)
		if e == nil && out.Head.Revision > 0 {
			return out.Head, nil
		}
		if u, e := c.s.o.Uploads.GetUpload(ctx, c.u.ID); e == nil && u.State == "Completed" {
			return c.s.receipt(ctx, u.ID, c.credential, u.Input.SHA256)
		}
		if !errors.Is(e, storage.ErrNotFound) {
			return storage.ObjectHead{}, err
		}
	}
	var invalid *core.ValidationError
	switch {
	case c.takeover && errors.Is(err, storage.ErrAccessDenied):
		c.abandonWith(ctx)
	case errors.As(err, &invalid), errors.Is(err, storage.ErrQuotaExceeded), errors.Is(err, storage.ErrAccessDenied), errors.Is(err, storage.ErrConflict):
		if _, e := c.s.o.Uploads.ReopenUpload(ctx, c.request()); errors.Is(e, storage.ErrFenced) {
			// Reopen refuses an expired upload: abort it instead.
			c.abandonWith(ctx)
		}
	}
	return storage.ObjectHead{}, err
}

func (c *completion) abandon() {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), 5*time.Second)
	defer cancel()
	c.abandonWith(ctx)
}

// abandonWith closes this completion op and aborts the upload through the
// recovery port; the reservation and counters release once. Open publishers
// keep it Aborting for recovery.
func (c *completion) abandonWith(ctx context.Context) {
	_, _ = c.s.o.Uploads.ResolveCompletion(ctx, c.request())
}
