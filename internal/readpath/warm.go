package readpath

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
)

// WarmOptions accepts contract-conforming metadata adapters. Each adapter must
// validate canonical CRC presence (zero included) when decoding persisted chunks.
// WarmGate remains a single-node boundary; this wiring does not establish R1.
type WarmOptions struct {
	Metadata  storage.MetadataStore
	Packs     *packs3.Store
	Cache     *cache.LRU
	Admission *Admission
	Gate      *coordination.WarmGate
	Clock     clock.Clock
	Global    config.GlobalSnapshot
	Metrics   *WarmMetrics
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
	Range                     *storage.ByteRange // normalized by HTTP consumer; nil means full object
	Deadline                  clock.Instant
	BucketPolicy              config.BucketSnapshot
	Budget                    Budget
	MaxOutstanding            int
	// Ready exposes metadata after preflight, also for empty objects. HTTP
	// consumers may delay committing headers until the first verified body slice.
	Ready func(WarmResult) error
}
type WarmResult struct {
	Head  storage.ObjectHead
	Range storage.ByteRange
}

func NewWarmReader(o WarmOptions) (*WarmReader, error) {
	if o.Metadata == nil || (reflect.ValueOf(o.Metadata).Kind() == reflect.Ptr && reflect.ValueOf(o.Metadata).IsNil()) || o.Packs == nil || o.Cache == nil || o.Admission == nil || o.Gate == nil || o.Clock == nil || o.Metrics == nil || o.Global.Revision() == 0 || o.TransportBytes <= 0 || o.TransportBytes > math.MaxInt64-pack.MaxPackBytes || o.MaxManifestPages == 0 || o.MaxManifestPages > 1024 || o.MaxManifestChunks == 0 {
		return nil, storage.ErrInvalid
	}
	if err := o.Gate.CheckClock(o.Clock); err != nil {
		return nil, err
	}
	return &WarmReader{options: o}, nil
}

// Read emits only verified logical slices, in order. emit borrows each slice
// until return. An error after prior emission requires the HTTP consumer to abort
// the stream; it cannot send a new success/error body. No HTTP endpoint is owned
// here (W03-CHIEN-02). WarmGate is a local no-maintenance boundary, not distributed R1.
func (r *WarmReader) Read(ctx context.Context, q WarmRequest, emit func(context.Context, []byte) error) (result WarmResult, err error) {
	if r == nil || r.options.Metadata == nil || ctx == nil || emit == nil || q.CredentialID == "" || q.MaxOutstanding <= 0 || !validBudget(q.Budget) {
		return result, storage.ErrInvalid
	}
	o := r.options
	o.Metrics.requests.Add(1)
	defer func() {
		if err != nil {
			o.Metrics.aborted.Add(1)
		}
	}()
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
	plan, err := engine.ResolveReadPlan(ctx, o.Metadata, object, head, q.Range)
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
	if err = ex.preflight(ctx, plan, ticket); err != nil {
		return result, err
	}
	result = WarmResult{Head: head, Range: plan.Range}
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
	// Warm-only MVP: never turn cached Cold/Archive bytes into readable content.
	if b.info.State != "Warm" || (b.info.Class != storage.Standard && b.info.Class != storage.StandardIA) {
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
		if err != nil {
			return err
		}
	}
	return nil
}
func (e *warmExecutor) physical(ctx context.Context, b warmBinding) error {
	physical, err := e.reader.options.Packs.HeadPack(ctx, b.key.Pack)
	if err != nil {
		return err
	}
	if physical.Size != b.info.Size || physical.Ref != b.key.Pack {
		return storage.ErrConflict
	}
	if physical.State != "Warm" || physical.Class != b.info.Class {
		return storage.ErrCold
	}
	return nil
}
func (e *warmExecutor) Execute(ctx context.Context, i int, _ engine.PlannedChunk) ([]byte, error) {
	b := e.bindings[i]
	o := e.reader.options
	if err := e.checkView(ctx); err != nil {
		return nil, err
	}
	infos, err := o.Metadata.GetPacks(ctx, []storage.PackRef{b.key.Pack})
	if err != nil {
		return nil, err
	}
	current, ok := infos[b.key.Pack]
	if !ok {
		return nil, storage.ErrNotFound
	}
	if !sameWarmReadLifecycle(current, b.info) {
		return nil, storage.ErrConflict
	}
	if err = e.eligible(ctx, b); err != nil {
		return nil, err
	}
	if err = e.physical(ctx, b); err != nil {
		return nil, err
	}
	if err = e.checkView(ctx); err != nil {
		return nil, err
	}
	stored, hit, err := o.Cache.Get(ctx, b.key)
	if err != nil {
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
