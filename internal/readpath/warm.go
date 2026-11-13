package readpath

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
)

// WarmOptions accepts contract-conforming metadata adapters. Each adapter must
// validate canonical CRC presence (zero included) when decoding persisted chunks.
// WarmGate remains a single-node boundary; this wiring does not establish R1.
type WarmOptions struct {
	Metadata  storage.MetadataStore
	Packs     storage.PackStore
	Cache     storage.Cache
	Admission *Admission
	Gate      *coordination.WarmGate
	Clock     clock.Clock
	Global    config.GlobalSnapshot
	Metrics   *WarmMetrics
	// UTC is required for temporary archive copies; nil never upgrades wall time
	// to UTC evidence. Restore enqueues work only after the entire requested set
	// has been pinned and preflighted. It never makes this GET readable. A nil
	// explicit request is GET's policy auto-restore; a RestoreObject passes its
	// Days/Tier. It reports whether this call created the restore job.
	UTC          storage.R1TimeSource
	Restore      func(ctx context.Context, view *coordination.WarmView, head storage.ObjectHead, ref storage.PackRef, revision uint64, required time.Time, explicit *storage.RestoreRequest) (bool, error)
	RestoreError func(context.Context, error)
	// Bound on simultaneously owned transport buffers per RangeGet, supplied by
	// the runtime's transport audit. Excludes shared pools/RSS headroom. No inferred
	// default; an unbounded/custom buffering transport must not be enabled.
	TransportBytes int64
	// Metadata planning limits are independent of payload/LRU admission.
	MaxManifestPages  uint32
	MaxManifestChunks uint64
}
type WarmReader struct{ options WarmOptions }
type WarmRequest struct {
	CredentialID, Bucket, Key string
	Range                     *storage.ByteRange // already normalized; nil means full object
	// SelectRange resolves a size-dependent range or precondition against the
	// head bound by this read view, before resolving pages or fetching payload.
	// Mutually exclusive with Range; the callback must treat the head as read-only.
	SelectRange    func(storage.ObjectHead) (*storage.ByteRange, error)
	Deadline       clock.Instant
	BucketPolicy   config.BucketSnapshot
	Budget         Budget
	MaxOutstanding int
	// Ready exposes metadata after preflight, also for empty objects. HTTP
	// consumers may delay committing headers until the first verified body slice.
	Ready func(WarmResult) error
	// restore turns Read into RestoreObject: same admission, binding and pins,
	// then restore requests instead of payload.
	// Debug enables diagnostic pre-flight cache residency probing (x-dsg-cache).
	// Only set for authorized debug requests; ordinary GETs skip the scan.
	Debug   bool
	restore *restoreCall
}
type WarmResult struct {
	Head        storage.ObjectHead
	Range       storage.ByteRange
	CacheStatus string
}

// ColdRestoreError reports storage.ErrCold together with the auto-restore
// state ("requested" or "in-progress") for debug-gated x-dsg-restore.
type ColdRestoreError struct {
	State string
}

func (e *ColdRestoreError) Error() string { return "readpath: cold restore " + e.State }
func (e *ColdRestoreError) Unwrap() error { return storage.ErrCold }
func NewWarmReader(o WarmOptions) (*WarmReader, error) {
	if o.Metadata == nil || (reflect.ValueOf(o.Metadata).Kind() == reflect.Ptr && reflect.ValueOf(o.Metadata).IsNil()) || o.Packs == nil || (reflect.ValueOf(o.Packs).Kind() == reflect.Ptr && reflect.ValueOf(o.Packs).IsNil()) || o.Cache == nil || (reflect.ValueOf(o.Cache).Kind() == reflect.Ptr && reflect.ValueOf(o.Cache).IsNil()) || o.Admission == nil || o.Gate == nil || o.Clock == nil || o.Metrics == nil || o.Global.Revision() == 0 || o.TransportBytes <= 0 || o.TransportBytes > math.MaxInt64-pack.MaxPackBytes || o.MaxManifestPages == 0 || o.MaxManifestPages > 1024 || o.MaxManifestChunks == 0 {
		return nil, storage.ErrInvalid
	}
	if err := o.Gate.CheckClock(o.Clock); err != nil {
		return nil, err
	}
	return &WarmReader{options: o}, nil
}

func (r *WarmReader) PolicyRevision() uint64 {
	if r == nil {
		return 0
	}
	return r.options.Global.Revision()
}

// Read emits only verified logical slices, in order. emit borrows each slice
// until return. An error after prior emission requires the HTTP consumer to abort
// the stream; it cannot send a new success/error body. No HTTP endpoint is owned
// here (the S3 GET handler owns it). WarmGate is a local no-maintenance boundary, not distributed R1.
func (r *WarmReader) Read(ctx context.Context, q WarmRequest, emit func(context.Context, []byte) error) (result WarmResult, err error) {
	if r == nil || r.options.Metadata == nil || ctx == nil || emit == nil || q.CredentialID == "" || q.MaxOutstanding <= 0 || !validBudget(q.Budget) || (q.Range != nil && q.SelectRange != nil) {
		return result, storage.ErrInvalid
	}
	o := r.options
	if q.restore == nil { // miles_warm_read_* counts GETs only
		o.Metrics.requests.Add(1)
		defer func() {
			if err != nil {
				o.Metrics.aborted.Add(1)
			}
		}()
	}
	auth, err := o.Metadata.Authorize(ctx, q.CredentialID, q.Bucket)
	if err != nil {
		return result, err
	}
	if !auth.Bucket.Active || !(auth.Read || auth.UserID == auth.Bucket.OwnerID) {
		return result, storage.ErrAccessDenied
	}
	if q.BucketPolicy.BucketID() != auth.Bucket.ID {
		return result, storage.ErrConflict
	}
	if _, err = config.EffectivePolicy(o.Global, q.BucketPolicy); err != nil {
		return result, err
	}
	object := storage.ObjectKey{Bucket: q.Bucket, BucketID: auth.Bucket.ID, Key: q.Key}
	ticket, err := o.Admission.AdmitRequest(ctx, BucketBinding{Name: q.Bucket, Policy: q.BucketPolicy}, q.Budget)
	if err != nil {
		return result, err
	}
	defer ticket.Close()
	view, err := o.Gate.Acquire(ctx, object, q.Deadline)
	if err != nil {
		return result, err
	}
	defer view.Release()
	work, err := view.BeginWork(ctx)
	if err != nil {
		return result, err
	}
	defer work.Finish()
	ctx, stopHeartbeat, err := view.KeepAlive(ctx)
	if err != nil {
		return result, err
	}
	defer stopHeartbeat()
	head, err := o.Metadata.GetObject(ctx, object)
	if err != nil {
		return result, err
	}
	if head.Deleted {
		return result, storage.ErrNotFound
	}
	if err = view.Bind(ctx, head); err != nil {
		return result, err
	}
	if head.PageCount > o.MaxManifestPages || head.ChunkCount > o.MaxManifestChunks {
		return result, ErrExhausted
	}
	span := q.Range
	if q.SelectRange != nil {
		span, err = q.SelectRange(head)
		if err != nil {
			return result, err
		}
	}
	plan, err := engine.ResolveReadPlan(ctx, o.Metadata, object, head, span)
	if err != nil {
		return result, err
	}
	if head.Domain != auth.Bucket.Domain || head.Size > 0 && !plan.Validation.DescriptorVerified {
		return result, engine.ErrIncompleteManifest
	}
	ex := &warmExecutor{reader: r, request: q, view: view, admitted: auth, bindings: make([]warmBinding, len(plan.Chunks)), access: map[storage.PackRef]time.Time{}}
	// Bound metadata batch fanout; preserve every logical occurrence, including
	// repeated hashes. No physical I/O or cache lookup before full preflight.
	for start := 0; start < len(plan.Chunks); start += 100 {
		end := min(start+100, len(plan.Chunks))
		keys := []storage.ChunkKey{}
		for _, c := range plan.Chunks[start:end] {
			keys = append(keys, c.Ref.Key)
		}
		chunks, e := o.Metadata.GetChunks(ctx, keys, true)
		if e != nil {
			return result, e
		}
		refs := []storage.PackRef{}
		for _, key := range keys {
			c, ok := chunks[key]
			if !ok {
				return result, storage.ErrNotFound
			}
			refs = append(refs, c.Location.Pack)
		}
		infos, e := o.Metadata.GetPacks(ctx, refs)
		if e != nil {
			return result, e
		}
		if e = view.Pin(ctx, refs); e != nil {
			return result, e
		}
		// Re-read after pin: snapshot comparison is not itself the pin. Retirement
		// admission uses WarmGate; concurrent canonical CAS is detected, never retried
		// under a different unpinned generation.
		again, e := o.Metadata.GetChunks(ctx, keys, true)
		if e != nil {
			return result, e
		}
		for i, c := range plan.Chunks[start:end] {
			row := chunks[c.Ref.Key]
			if again[c.Ref.Key] != row {
				return result, storage.ErrConflict
			}
			info, ok := infos[row.Location.Pack]
			if !ok {
				return result, storage.ErrNotFound
			}
			key, entry, e := MapCanonicalRange(c.Ref, row, info, row.LocationRevision)
			if e != nil {
				return result, e
			}
			b := warmBinding{key: key, entry: entry, info: info}
			if e = ex.eligible(ctx, b); e != nil {
				return result, e
			}
			ex.bindings[start+i] = b
		}
	}
	if err = view.Resolved(ctx); err != nil {
		return result, err
	}
	if q.restore != nil {
		return result, ex.restore(ctx, plan, *q.restore)
	}
	if err = ex.preflight(ctx, plan, ticket); err != nil {
		return result, err
	}
	cacheStatus := ""
	if q.Debug {
		cacheStatus = "miss"
		if prober, ok := o.Cache.(interface {
			Has(context.Context, storage.CacheKey) bool
		}); ok && len(ex.bindings) > 0 {
			hits := 0
			for _, b := range ex.bindings {
				if prober.Has(ctx, b.key) {
					hits++
				}
			}
			switch {
			case hits == len(ex.bindings):
				cacheStatus = "hit"
			case hits > 0:
				cacheStatus = "partial"
			}
		}
	}
	result = WarmResult{Head: head, Range: plan.Range, CacheStatus: cacheStatus}
	if err = ex.checkView(ctx); err != nil {
		return result, err
	}
	if q.Ready != nil {
		if err = q.Ready(result); err != nil {
			return result, err
		}
	}
	err = runAdmittedReadAhead(ctx, o.Admission, BucketBinding{Name: q.Bucket, Policy: q.BucketPolicy}, plan, q.MaxOutstanding, ex, func(ctx context.Context, index int, b []byte) error {
		if e := ex.checkView(ctx); e != nil {
			return e
		}
		// A completed speculative result may have waited behind the emitter.
		// Revalidate the temporary copy and lifecycle before releasing its bytes.
		if e := ex.revalidate(ctx, ex.bindings[index]); e != nil {
			return e
		}
		if e := emit(ctx, b); e != nil {
			return e
		}
		o.Metrics.emitted.Add(uint64(len(b)))
		ref := ex.bindings[index].key.Pack
		at := o.Clock.Now().WallUTC.UTC()
		if old, ok := ex.access[ref]; !ok || at.After(old) {
			ex.access[ref] = at
		}
		return nil
	}, ticket)
	// Observations are non-authoritative and never turn delivered, verified bytes
	// into a failed/rolled-back read. Flush a successful logical read in bounded
	// batches; canceled/aborted requests do not start this extra I/O. There is no
	// detached worker, hidden retry or protection dependency on access stats.
	if err == nil && ctx.Err() == nil && len(ex.access) > 0 {
		if statsErr := persistPackAccess(ctx, ex.access, ex.reader.options.Metadata.UpdateAccessStats); statsErr != nil {
			o.Metrics.accessStatsErrors.Add(1)
		}
	}
	return result, err
}

// RestoreResult is a RestoreObject outcome over the unique GLACIER packs of
// the bound head (API v2 4.4). Archive zero means a Warm/IR-only object.
type RestoreResult struct {
	Archive, Initiated, InProgress int
}

type restoreCall struct {
	request storage.RestoreRequest
	result  *RestoreResult
}

// Restore admits, binds and pins the object exactly like Read, then requests
// restore of every unique GLACIER pack whose backend copy is neither restoring
// nor restored for at least request.Days; a shorter Restored copy is extended.
// It never fetches payload or waits for the archive: acceptance is not
// readability.
func (r *WarmReader) Restore(ctx context.Context, q WarmRequest, request storage.RestoreRequest) (RestoreResult, error) {
	var out RestoreResult
	if q.Range != nil || q.SelectRange != nil || q.Ready != nil || request.Validate() != nil {
		return out, storage.ErrInvalid
	}
	q.restore = &restoreCall{request: request, result: &out}
	_, err := r.Read(ctx, q, func(context.Context, []byte) error { return storage.ErrInvalid })
	return out, err
}

// ArchiveState is the backend restore state of a head's unique GLACIER packs,
// the input of HEAD's x-amz-restore (API v2 4.4).
type ArchiveState struct {
	Archive, Restoring, Restored int
	Expiry                       time.Time // earliest unexpired expiry of the Restored packs
}

// ArchiveState reads the head's chunk locations and pack records and HEADs
// each GLACIER pack on the backend, never payload. It pins nothing, grants no
// readability and never starts a restore: HEAD is an informational snapshot.
func (r *WarmReader) ArchiveState(ctx context.Context, head storage.ObjectHead) (ArchiveState, error) {
	var out ArchiveState
	o := r.options
	if head.PageCount > o.MaxManifestPages || head.ChunkCount > o.MaxManifestChunks {
		return out, ErrExhausted
	}
	plan, err := engine.ResolveReadPlan(ctx, o.Metadata, head.Object, head, nil)
	if err != nil {
		return out, err
	}
	now := o.Clock.Now().WallUTC
	seen := map[storage.PackRef]bool{}
	for start := 0; start < len(plan.Chunks); start += 100 {
		keys := []storage.ChunkKey{}
		for _, c := range plan.Chunks[start:min(start+100, len(plan.Chunks))] {
			keys = append(keys, c.Ref.Key)
		}
		chunks, err := o.Metadata.GetChunks(ctx, keys, true)
		if err != nil {
			return out, err
		}
		refs := []storage.PackRef{}
		for _, key := range keys {
			c, ok := chunks[key]
			if !ok {
				return out, storage.ErrNotFound
			}
			if !seen[c.Location.Pack] {
				seen[c.Location.Pack] = true
				refs = append(refs, c.Location.Pack)
			}
		}
		if len(refs) == 0 {
			continue
		}
		infos, err := o.Metadata.GetPacks(ctx, refs)
		if err != nil {
			return out, err
		}
		for _, ref := range refs {
			info, ok := infos[ref]
			if !ok {
				return out, storage.ErrNotFound
			}
			if info.Class != storage.Glacier {
				continue
			}
			out.Archive++
			// The backend, not the metadata record, knows whether a temporary
			// copy exists and when it expires.
			physical, err := o.Packs.HeadPack(ctx, ref)
			if err != nil {
				return out, err
			}
			switch {
			case physical.Class != storage.Glacier:
			case physical.State == "Restored" && physical.RestoredUntil.After(now):
				out.Restored++
				if out.Expiry.IsZero() || physical.RestoredUntil.Before(out.Expiry) {
					out.Expiry = physical.RestoredUntil
				}
			case physical.State == "Restoring" || info.State == "Restoring":
				out.Restoring++
			}
		}
	}
	return out, nil
}

type warmBinding struct {
	key   storage.CacheKey
	entry pack.Entry
	info  storage.PackInfo
}
type warmExecutor struct {
	reader   *WarmReader
	request  WarmRequest
	view     *coordination.WarmView
	admitted storage.Authorization
	bindings []warmBinding
	access   map[storage.PackRef]time.Time // emitter-owned; speculation alone is not logical access
}

// Read permission is decided ONCE at admission (API 2.2.3 / R1 14).
// Later revoke/grant changes affect new requests. View authority and bucket
// incarnation/lifetime remain independent checks; this never reauthorizes.
func (e *warmExecutor) checkView(ctx context.Context) error {
	if err := e.view.Check(ctx); err != nil {
		return err
	}
	bucket, err := e.reader.options.Metadata.GetBucket(ctx, e.admitted.Bucket.Name)
	if err != nil {
		return err
	}
	if bucket.ID != e.admitted.Bucket.ID || bucket.Domain != e.admitted.Bucket.Domain {
		return storage.ErrConflict
	}
	if !bucket.Active {
		return storage.ErrFenced
	}
	return nil
}
func (e *warmExecutor) eligible(ctx context.Context, b warmBinding) error {
	if err := e.view.Check(ctx); err != nil {
		return err
	}
	// This only admits metadata to physical preflight, never cache or emission.
	if b.info.Class == storage.Glacier && (b.info.State == "Cold" || b.info.State == "Restoring" || b.info.State == "Restored") {
		return nil
	}
	if !(b.info.State == "Warm" && (b.info.Class == storage.Standard || b.info.Class == storage.StandardIA) || b.info.State == "Cold" && b.info.Class == storage.GlacierIR) {
		return storage.ErrCold
	}
	d, err := policy.EvaluateRead(policy.ReadInput{Policy: policy.PinnedPolicy{Snapshot: e.reader.options.Global, RequiredRevision: e.reader.options.Global.Revision()}, Request: policy.Request{Now: e.reader.options.Clock.Now().Instant, Deadline: e.request.Deadline, Cancelled: ctx.Err() != nil}, Pack: b.info, Facts: policy.ReadFacts{Logical: policy.Valid, Metadata: policy.Valid, Authority: policy.Valid, PinCoverage: policy.Valid, Physical: policy.Valid}})
	if err != nil {
		return err
	}
	if d.Verdict != policy.Eligible {
		return fmt.Errorf("%w: %s", storage.ErrFenced, d.Reason)
	}
	return nil
}
func (e *warmExecutor) Estimate(i int, _ engine.PlannedChunk) (Cost, error) {
	m, err := pack.EstimateDecodeMemory(e.bindings[i].entry)
	if err != nil {
		return Cost{}, err
	}
	// FetchRange owns exactly S bytes. LRU uses bytes.Clone: each copy's capacity
	// is covered by S plus an 8KiB Go page/class rounding envelope (pinned build).
	// Charge both hit/fill copy envelopes conservatively, independently of resident LRU.
	return Cost{ChunkSlots: 1, StoredBytes: m.InputBytes, IOSlots: 1, IOBytes: m.InputBytes + e.reader.options.TransportBytes, RawBytes: m.OutputBytes, WorkspaceBytes: m.WorkspaceBytes, CopyBytes: 2 * (m.InputBytes + 8192)}, nil
}

// Preflight the WHOLE requested physical set before any cache access or emit.
// HEADs also have transport ownership: reserve the full conservative occurrence
// cost first, close each synchronous call, then release. Scheduler reservations
// independently cover subsequent fetch/decode/output through emitter completion.
func (e *warmExecutor) preflight(ctx context.Context, plan engine.ReadPlan, ticket *RequestTicket) error {
	o := e.reader.options
	seen := map[storage.PackRef]bool{}
	archive := []storage.PackRef{}
	for i, b := range e.bindings {
		if seen[b.key.Pack] {
			continue
		}
		seen[b.key.Pack] = true
		cost, err := e.Estimate(i, plan.Chunks[i])
		if err != nil {
			return err
		}
		reservation, err := o.Admission.ReserveDemand(ctx, ticket, cost)
		if err != nil {
			return err
		}
		err = e.checkView(ctx)
		if err == nil {
			err = e.physical(ctx, b)
		}
		reservation.Release()
		if errors.Is(err, storage.ErrCold) {
			archive = append(archive, b.key.Pack)
			continue
		}
		if err != nil {
			return err
		}
	}
	if len(archive) > 0 {
		if o.Global.Policy().Tiering.AutoRestore && o.Restore != nil {
			r, proof, proofErr := coordination.RequestUTC(ctx, o.Clock, o.UTC, e.request.Deadline)
			required, requiredErr := policy.RestoreRequiredUntil(r, proof)
			if proofErr != nil || requiredErr != nil || required.IsZero() {
				o.Metrics.restoreErrors.Add(1)
				if o.RestoreError != nil {
					o.RestoreError(ctx, errors.Join(storage.ErrFenced, proofErr, requiredErr))
				}
				return storage.ErrCold
			}
			requested, inProgress := false, false
			for _, ref := range archive {
				if err := e.checkView(ctx); err != nil {
					return err
				}
				created, err := o.Restore(ctx, e.view, storage.ObjectHead{Object: plan.Object, Revision: plan.HeadRevision, ManifestID: plan.ManifestID}, ref, o.Global.Revision(), required, nil)
				if err != nil {
					o.Metrics.restoreErrors.Add(1)
					if o.RestoreError != nil {
						o.RestoreError(ctx, err)
					}
					continue
				}
				if created {
					requested = true
				} else {
					inProgress = true
				}
			}
			if requested {
				return &ColdRestoreError{State: "requested"}
			}
			if inProgress {
				return &ColdRestoreError{State: "in-progress"}
			}
		}
		return storage.ErrCold
	}
	return nil
}

// restore requests one restore per unique pinned GLACIER pack, in the order
// of first use. Every backend pack is checked against its pinned record before
// the first request, so a mismatch queues nothing. A later enqueue failure
// returns at once: restores already accepted stay accepted and a retry finds
// them restoring (API v2 4.4).
func (e *warmExecutor) restore(ctx context.Context, plan engine.ReadPlan, c restoreCall) error {
	o := e.reader.options
	seen := map[storage.PackRef]bool{}
	archive := []warmBinding{}
	for _, b := range e.bindings {
		if b.info.Class == storage.Glacier && !seen[b.key.Pack] {
			seen[b.key.Pack] = true
			archive = append(archive, b)
		}
	}
	c.result.Archive = len(archive)
	if len(archive) == 0 {
		return nil
	}
	if o.Restore == nil {
		return storage.ErrUnavailable
	}
	r, proof, err := coordination.RequestUTC(ctx, o.Clock, o.UTC, e.request.Deadline)
	if err != nil {
		return err
	}
	required, err := policy.RestoreRequiredUntil(r, proof)
	if err != nil || required.IsZero() {
		return errors.Join(storage.ErrFenced, err)
	}
	// A temporary copy that already outlives the requested Days is left alone.
	covered := proof.Upper.UTC().Add(time.Duration(c.request.Days) * 24 * time.Hour)
	pending := []storage.PackRef{}
	for _, b := range archive {
		if err := e.checkView(ctx); err != nil {
			return err
		}
		physical, err := o.Packs.HeadPack(ctx, b.key.Pack)
		if err != nil {
			return err
		}
		// Same identity checks as GET's physical(), without requiring Restored.
		if physical.Ref != b.key.Pack || physical.Size != b.info.Size || physical.Class != b.info.Class || !archiveState(b.info.State) || !archiveState(physical.State) {
			return storage.ErrConflict
		}
		switch {
		case physical.State == "Restoring":
			c.result.InProgress++
		case physical.State == "Restored" && !physical.RestoredUntil.Before(covered):
		default:
			pending = append(pending, b.key.Pack)
		}
	}
	head := storage.ObjectHead{Object: plan.Object, Revision: plan.HeadRevision, ManifestID: plan.ManifestID}
	for _, ref := range pending {
		if err := e.checkView(ctx); err != nil {
			return err
		}
		created, err := o.Restore(ctx, e.view, head, ref, o.Global.Revision(), required, &c.request)
		if err != nil {
			return err
		}
		if created {
			c.result.Initiated++
		} else {
			c.result.InProgress++
		}
	}
	return nil
}

func archiveState(state string) bool {
	return state == "Cold" || state == "Restoring" || state == "Restored"
}
func (e *warmExecutor) physical(ctx context.Context, b warmBinding) error {
	physical, err := e.reader.options.Packs.HeadPack(ctx, b.key.Pack)
	if err != nil {
		return err
	}
	if physical.Size != b.info.Size || physical.Ref != b.key.Pack {
		return storage.ErrConflict
	}
	if physical.Class != b.info.Class {
		return storage.ErrCold
	}
	if b.info.Class != storage.Glacier {
		if physical.State != b.info.State {
			return storage.ErrCold
		}
		return e.eligible(ctx, b)
	}
	if physical.State != "Restored" {
		return storage.ErrCold
	}
	r, proof, err := coordination.RequestUTC(ctx, e.reader.options.Clock, e.reader.options.UTC, e.request.Deadline)
	if err != nil {
		return err
	}
	physical.StateRevision = b.info.StateRevision
	d, err := policy.EvaluateRead(policy.ReadInput{Policy: policy.PinnedPolicy{Snapshot: e.reader.options.Global, RequiredRevision: e.reader.options.Global.Revision()}, Request: r, UTC: proof, Pack: physical,
		Facts: policy.ReadFacts{Logical: policy.Valid, Metadata: policy.Valid, Authority: policy.Valid, PinCoverage: policy.Valid, Physical: policy.Valid}, RestoreVerified: policy.Valid})
	if err != nil {
		return err
	}
	if d.Verdict != policy.Eligible {
		if d.Reason == policy.RestoreRequired {
			return storage.ErrCold
		}
		return fmt.Errorf("%w: %s", storage.ErrFenced, d.Reason)
	}
	return nil
}
func (e *warmExecutor) revalidate(ctx context.Context, b warmBinding) error {
	o := e.reader.options
	if err := e.checkView(ctx); err != nil {
		return err
	}
	infos, err := o.Metadata.GetPacks(ctx, []storage.PackRef{b.key.Pack})
	if err != nil {
		return err
	}
	current, ok := infos[b.key.Pack]
	if !ok {
		return storage.ErrNotFound
	}
	if !sameWarmReadLifecycle(current, b.info) {
		return storage.ErrConflict
	}
	if err = e.eligible(ctx, b); err != nil {
		return err
	}
	if err = e.physical(ctx, b); err != nil {
		return err
	}
	return e.checkView(ctx)
}
func (e *warmExecutor) Execute(ctx context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
	b := e.bindings[i]
	o := e.reader.options
	if err := e.revalidate(ctx, b); err != nil {
		return nil, err
	}
	var err error
	if err = e.checkView(ctx); err != nil {
		return nil, err
	}
	stored, hit, err := o.Cache.Get(ctx, b.key)
	if err != nil {
		// Cache detects immutable-byte corruption before VerifyPackEntry runs.
		// Cancellation and ordinary cache I/O errors are not integrity failures.
		if errors.Is(err, storage.ErrConflict) {
			o.Metrics.integrity.Add(1)
		}
		return nil, err
	}
	if hit {
		o.Metrics.hits.Add(1)
	} else {
		o.Metrics.misses.Add(1)
		stored, err = FetchRange(ctx, o.Packs, b.key.Pack, b.key.Range, int64(b.entry.StoredLen))
		if err != nil {
			if errors.Is(err, ErrShortRange) || errors.Is(err, ErrOversizedRange) {
				o.Metrics.integrity.Add(1)
			}
			return nil, err
		}
		o.Metrics.backendBytes.Add(uint64(len(stored)))
	}
	if err = e.checkView(ctx); err != nil {
		return nil, err
	}
	raw, err := VerifyPackEntry(ctx, b.entry, stored)
	if err != nil {
		var format *pack.FormatError
		if errors.As(err, &format) {
			o.Metrics.integrity.Add(1)
		}
		if hit {
			_ = o.Cache.Evict(context.WithoutCancel(ctx), b.key.Pack)
		}
		return nil, err
	}
	if err = e.checkView(ctx); err != nil {
		return nil, err
	}
	if !hit {
		if err = o.Cache.Put(ctx, b.key, stored); err != nil {
			if errors.Is(err, storage.ErrConflict) {
				o.Metrics.integrity.Add(1)
				return nil, err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			o.Metrics.cacheErrors.Add(1)
		}
	}
	return raw, nil
}

// LastAccess is a non-authoritative observation. Writer ownership and retention
// deadlines do not change this pinned Warm generation's read eligibility either.
// Keep identity, size, class/state/revision and restore-expiry checks explicit.
func sameWarmReadLifecycle(a, b storage.PackInfo) bool {
	return a.Version == b.Version && a.Ref == b.Ref && a.Size == b.Size && a.Class == b.Class && a.State == b.State && a.StateRevision == b.StateRevision && a.RestoredUntil.Equal(b.RestoredUntil)
}
