package coordination

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

var (
	ErrReconcilePending    = errors.New("coordination: reconcile original operation before release or takeover")
	ErrMaintenanceDisabled = errors.New("coordination: destructive lifecycle disabled until durable R1 integration")
	ErrWorkPending         = errors.New("coordination: synchronous work or acknowledgement still in flight")
)

// Hooks are instance-local fault-test seams. They run outside the model lock.
// After may hide an acknowledgement, never roll back the completed effect.
type Hooks struct {
	Before, After func(context.Context, string, storage.WriterFence) error
}

// WriterRegistry owns local CPU/I/O lifetime and unknown-result tracking. With a
// shared gate, BeginOperation also acquires a durable owner-bound lease and Do
// registers the action before dispatch. Metadata still owns publication LPs.
// Its mutex serializes local effects against local closure. Other workers close
// shared authority via metadata CAS; expiry/timeout never proves quiescence.
type WriterRegistry struct {
	mu      sync.Mutex
	gate    *WarmGate
	limits  LeaseLimits
	hooks   Hooks
	closed  bool
	writers map[string]*writerState
}
type Writer struct {
	owner *WriterRegistry
	state *writerState
}
type writerState struct {
	fence                  storage.WriterFence
	deadline, lease        clock.Instant
	closed, released, lost bool
	pins                   map[storage.PackRef]bool
	unknown                map[string]bool
	inflight               int
	operationID            string
	object                 storage.ObjectKey
	work                   int
	shared                 bool
	sharedDeadline         time.Time
}

// WriterWork retains admission/pins during CPU/body work BETWEEN storage calls.
// A registration can be claimed by one logical operation only. Duplicate
// registration may recover a handle, never create a second executing session.
type WriterWork struct{ state *writerWork }
type writerWork struct {
	writer   *Writer
	finished bool
}

func (w *Writer) BeginOperation(ctx context.Context, object storage.ObjectKey, opID string) (*WriterWork, error) {
	if w == nil || w.state == nil || ctx == nil || object.Validate() != nil || opID == "" {
		return nil, storage.ErrInvalid
	}
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	if w.state.operationID != "" {
		return nil, storage.ErrConflict
	}
	if err := w.checkLocked(ctx); err != nil {
		return nil, err
	}
	if g := w.owner.gate; g.shared != nil {
		wall := w.state.sharedDeadline
		if wall.IsZero() {
			var err error
			wall, err = g.sharedDeadline(ctx, w.state.deadline)
			if err != nil {
				return nil, err
			}
			w.state.sharedDeadline = wall
		}
		if _, err := g.shared.AcquireR1(ctx, storage.R1Acquire{Fence: w.Fence(), Owner: g.owner, Kind: "writer", OpID: opID, Object: object, Deadline: wall}); err != nil {
			return nil, err
		}
		w.state.shared = true
	}
	w.state.operationID, w.state.object = opID, object
	w.state.work++
	return &WriterWork{state: &writerWork{writer: w}}, nil
}
func (work *WriterWork) Finish() {
	if work == nil || work.state == nil {
		return
	}
	w := work.state.writer
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	if work.state.finished {
		return
	}
	work.state.finished = true
	w.state.work--
}

func NewWriterRegistry(g *WarmGate, limits LeaseLimits, hooks Hooks) (*WriterRegistry, error) {
	if g == nil || g.clock == nil || g.incarnation == "" || limits.Validate() != nil {
		return nil, storage.ErrInvalid
	}
	return &WriterRegistry{gate: g, limits: limits, hooks: hooks, writers: map[string]*writerState{}}, nil
}

func (r *WriterRegistry) Register(ctx context.Context, fence storage.WriterFence, deadline clock.Instant) (*Writer, error) {
	if r == nil || ctx == nil || fence.LeaseID == "" || fence.Epoch == 0 || fence.ServiceIncarnation != r.gate.incarnation {
		return nil, storage.ErrInvalid
	}
	if r.hooks.Before != nil {
		if err := r.hooks.Before(ctx, "register", fence); err != nil {
			return nil, err
		}
	}
	w, err := r.register(ctx, fence, deadline)
	if err != nil {
		return nil, err
	}
	if r.hooks.After != nil {
		if err := r.hooks.After(ctx, "register", fence); err != nil {
			return nil, errors.Join(storage.ErrUnavailable, err)
		}
	}
	return w, nil
}

func (r *WriterRegistry) register(ctx context.Context, fence storage.WriterFence, deadline clock.Instant) (*Writer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, storage.ErrFenced
	}
	if old := r.writers[fence.LeaseID]; old != nil {
		if old.fence == fence {
			cmp, err := deadline.Compare(old.deadline)
			if err != nil {
				return nil, err
			}
			if cmp != 0 {
				return nil, storage.ErrConflict
			}
			w := &Writer{r, old}
			return w, w.checkLocked(ctx)
		}
		// Local takeover is possible only after explicit quiescent release and
		// known outcomes. Backend ownership/grace/CAS is NOT supplied here.
		if !old.released || len(old.unknown) != 0 {
			return nil, ErrReconcilePending
		}
		if fence.Epoch <= old.fence.Epoch {
			return nil, storage.ErrFenced
		}
	}
	end, err := leaseEnd(r.gate.clock, deadline, r.limits.Lifetime)
	if err != nil {
		return nil, err
	}
	s := &writerState{fence: fence, deadline: deadline, lease: end, pins: map[storage.PackRef]bool{}, unknown: map[string]bool{}}
	w := &Writer{r, s}
	if err := w.checkLocked(ctx); err != nil {
		return nil, err
	}
	r.gate.mu.Lock()
	r.gate.resolving++
	r.gate.mu.Unlock()
	r.writers[fence.LeaseID] = s
	return w, nil
}

func (w *Writer) Fence() storage.WriterFence {
	if w == nil || w.state == nil {
		return storage.WriterFence{}
	}
	return w.state.fence
}
func (w *Writer) checkLocked(ctx context.Context) error {
	r, s := w.owner, w.state
	if err := ctx.Err(); err != nil {
		return err
	}
	r.gate.mu.Lock()
	invalid := r.gate.closed
	r.gate.mu.Unlock()
	if r.closed || invalid || s.closed || s.released || s.lost {
		return storage.ErrFenced
	}
	if r.gate.shared != nil && s.shared {
		if _, err := r.gate.shared.CheckR1(ctx, s.fence); err != nil {
			return err
		}
	}
	for _, end := range []clock.Instant{s.deadline, s.lease} {
		cmp, err := r.gate.clock.Now().Instant.Compare(end)
		if err != nil {
			return err
		}
		if cmp >= 0 {
			s.lost = true
			return storage.ErrFenced
		}
	}
	return nil
}
func (w *Writer) Check(ctx context.Context) error {
	if w == nil || w.state == nil || ctx == nil {
		return storage.ErrInvalid
	}
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	return w.checkLocked(ctx)
}
func (w *Writer) Heartbeat(ctx context.Context) error {
	if w == nil || w.state == nil || ctx == nil {
		return storage.ErrInvalid
	}
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	next, err := leaseEnd(w.owner.gate.clock, w.state.deadline, w.owner.limits.Lifetime)
	if err != nil {
		return err
	}
	if err := w.checkLocked(ctx); err != nil {
		return err
	}
	if w.owner.gate.shared != nil && w.state.shared {
		if _, err := w.owner.gate.shared.RenewR1(ctx, w.Fence()); err != nil {
			return err
		}
	}
	w.state.lease = next
	return nil
}
func (w *Writer) KeepAlive(ctx context.Context) (context.Context, func(), error) {
	if err := w.Check(ctx); err != nil {
		return nil, nil, err
	}
	return keepAlive(ctx, w.owner.gate.clock, w.owner.limits.Heartbeat, w.Heartbeat)
}

// Do dispatches ONE synchronous side effect. No automatic retry/rebase. Local
// closure and the callback are serialized; it may settle after local lease E.
// The backend still owns its transaction LP, auth, quota and fencing checks.
func (w *Writer) Do(ctx context.Context, operation string, effect func(storage.WriterFence) error) error {
	if w == nil || w.state == nil || ctx == nil || operation == "" || effect == nil {
		return storage.ErrInvalid
	}
	r := w.owner
	if r.hooks.Before != nil {
		if err := r.hooks.Before(ctx, operation, w.Fence()); err != nil {
			return err
		}
	}
	r.mu.Lock()
	if err := w.checkLocked(ctx); err != nil {
		r.mu.Unlock()
		return err
	}
	if len(w.state.unknown) != 0 {
		r.mu.Unlock()
		return ErrReconcilePending
	}
	if r.gate.shared != nil {
		if !w.state.shared {
			r.mu.Unlock()
			return storage.ErrFenced
		}
		if err := r.gate.shared.BeginR1Action(ctx, w.Fence(), operation); err != nil {
			r.mu.Unlock()
			return err
		}
		// Registration is an RPC, not dispatch permission: its ACK can cross E.
		if err := w.checkLocked(ctx); err != nil {
			settle, stop := cleanupContext()
			e := r.gate.shared.SettleR1Action(settle, w.Fence(), operation)
			stop()
			if e != nil {
				w.state.unknown[operation] = true
			}
			r.mu.Unlock()
			return errors.Join(err, e)
		}
	}
	err := w.effectLocked(operation, effect)
	if err != nil {
		if r.gate.shared != nil && (errors.Is(err, storage.ErrFenced) || errors.Is(err, storage.ErrInvalid) || errors.Is(err, storage.ErrConflict) || errors.Is(err, storage.ErrAccessDenied)) {
			settle, stop := cleanupContext()
			defer stop()
			if e := r.gate.shared.SettleR1Action(settle, w.Fence(), operation); e != nil {
				return errors.Join(err, e)
			}
		}
		return err
	}
	ackErr := w.acknowledge(ctx, operation)
	if ackErr != nil {
		return errors.Join(storage.ErrUnavailable, ackErr)
	}
	if r.gate.shared != nil {
		if err := r.gate.shared.SettleR1Action(ctx, w.Fence(), operation); err != nil {
			r.mu.Lock()
			w.state.unknown[operation] = true
			r.mu.Unlock()
			return err
		}
	}
	return nil
}

// Called with registry.mu held; always unlock, including adapter panic. Panic
// still propagates, but is never mistaken for a known-no-effect operation.
func (w *Writer) effectLocked(operation string, effect func(storage.WriterFence) error) (err error) {
	r := w.owner
	returned := false
	defer func() {
		if !returned {
			w.state.unknown[operation] = true
		}
		r.mu.Unlock()
	}()
	err = effect(w.Fence())
	returned = true
	if errors.Is(err, storage.ErrFenced) {
		w.state.lost = true
	}
	// Cancellation, I/O failure or a lost response AFTER dispatch is not proof
	// of no effect. Only explicit authoritative precondition rejection is known.
	if err != nil && !errors.Is(err, storage.ErrFenced) && !errors.Is(err, storage.ErrInvalid) && !errors.Is(err, storage.ErrConflict) && !errors.Is(err, storage.ErrAccessDenied) {
		w.state.unknown[operation] = true
		err = errors.Join(storage.ErrUnavailable, err)
	}
	if err == nil {
		w.state.inflight++
	}
	return err
}

func (w *Writer) acknowledge(ctx context.Context, operation string) (err error) {
	r := w.owner
	returned := false
	defer func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		w.state.inflight--
		if !returned || err != nil {
			w.state.unknown[operation] = true
		}
	}()
	if r.hooks.After != nil {
		err = r.hooks.After(ctx, operation, w.Fence())
	}
	returned = true
	return err
}

// Pin attaches an exact physical generation. The caller must validate the
// canonical row before AND after attachment; a lookup is not this pin.
func (w *Writer) Pin(ctx context.Context, ref storage.PackRef) error {
	if w == nil || w.state == nil || ctx == nil || ref.Validate() != nil {
		return storage.ErrInvalid
	}
	r := w.owner
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := w.checkLocked(ctx); err != nil {
		return err
	}
	g := r.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.retired[ref] {
		return storage.ErrConflict
	}
	if g.shared != nil {
		if err := g.shared.PinR1(ctx, w.Fence(), []storage.PackRef{ref}); err != nil {
			return err
		}
	}
	if !w.state.pins[ref] {
		w.state.pins[ref] = true
		g.pins[ref]++
	}
	return nil
}

type OutcomeReader interface {
	GetCommitOutcome(context.Context, string) (storage.CommitResult, error)
}

// ReconcileCommit reads the original retained result, never the current head.
// ErrNotFound does not clear uncertainty. No caller-provided boolean/result can
// authorize cleanup; a trusted authoritative adapter supplies the observation.
func (w *Writer) ReconcileCommit(ctx context.Context, store OutcomeReader, op string) (storage.CommitResult, error) {
	if w == nil || w.state == nil || ctx == nil || store == nil || op == "" {
		return storage.CommitResult{}, storage.ErrInvalid
	}
	result, err := store.GetCommitOutcome(ctx, op)
	if err != nil {
		return storage.CommitResult{}, err
	}
	if result.Head.OpID != op || result.Head.Revision == 0 {
		return storage.CommitResult{}, storage.ErrInvalid
	}
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	if w.state.operationID != "" && (op != w.state.operationID || result.Head.Object != w.state.object) {
		return storage.CommitResult{}, storage.ErrConflict
	}
	if w.state.inflight != 0 {
		return storage.CommitResult{}, ErrWorkPending
	}
	if w.owner.gate.shared != nil {
		if err := w.owner.gate.shared.SettleR1Action(ctx, w.Fence(), "commit/"+op); err != nil {
			return storage.CommitResult{}, err
		}
	}
	delete(w.state.unknown, "commit/"+op)
	return result, nil
}
func (w *Writer) CloseFence() {
	if w == nil || w.state == nil {
		return
	}
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	w.state.closed = true
	if w.owner.gate.shared != nil && w.state.shared {
		ctx, cancel := cleanupContext()
		defer cancel()
		_ = w.owner.gate.shared.CloseR1(ctx, w.Fence())
	}
}

// Release waits for synchronous callbacks by sharing their lock. Unknown
// outcomes retain guard and pins; cancellation/expiry is never no-commit proof.
func (w *Writer) Release() error {
	if w == nil || w.state == nil {
		return storage.ErrInvalid
	}
	r := w.owner
	r.mu.Lock()
	defer r.mu.Unlock()
	s := w.state
	s.closed = true
	if s.released {
		return nil
	}
	if s.inflight != 0 || s.work != 0 {
		return ErrWorkPending
	}
	if len(s.unknown) != 0 {
		return ErrReconcilePending
	}
	if r.gate.shared != nil && s.shared {
		ctx, cancel := cleanupContext()
		defer cancel()
		if err := r.gate.shared.CloseR1(ctx, w.Fence()); err != nil {
			return err
		}
		if err := r.gate.shared.ReleaseR1(ctx, w.Fence()); err != nil {
			return err
		}
	}
	g := r.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	g.resolving--
	for ref := range s.pins {
		g.pins[ref]--
		if g.pins[ref] == 0 {
			delete(g.pins, ref)
		}
	}
	s.pins = nil
	s.released = true
	return nil
}
func (r *WriterRegistry) Invalidate() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for _, s := range r.writers {
		s.closed = true
	}
	r.gate.Invalidate()
}
