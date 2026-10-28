// Package coordination provides the single-process Warm read lifetime boundary.
package coordination

import (
	"context"
	"sync"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

// WarmGate is deliberately local to one runtime. All physical retirement must
// enter Retire; direct backend deletion and other processes are outside this
// boundary. W03 adds local R1 integration; durable coordination remains W04.
// No lock is held across a GET. Restart loses these handles, never reconstructs
// authority.
type WarmGate struct {
	mu          sync.Mutex
	clock       clock.Clock
	resolving   int
	pins        map[storage.PackRef]int
	retired     map[storage.PackRef]bool
	incarnation string
	limits      LeaseLimits
	closed      bool
}

// WarmView is opaque. Its root is logical; physical protection comes only from
// Pin. The reader releases it after ALL workers and borrowed output quiesce.
type WarmView struct{ state *warmView }
type warmView struct {
	owner                     *WarmGate
	deadline                  clock.Instant
	object                    storage.ObjectKey
	root                      string
	revision                  uint64
	resolving, released, lost bool
	pins                      map[storage.PackRef]bool
	incarnation               string
	lease                     clock.Instant
	leaseRevision             uint64
	closing                   bool
	work                      int
}

func NewWarmGate(c clock.Clock) (*WarmGate, error) {
	if c == nil {
		return nil, storage.ErrInvalid
	}
	return &WarmGate{clock: c, pins: map[storage.PackRef]int{}, retired: map[storage.PackRef]bool{}}, nil
}

// CheckClock requires the same process-local monotonic domain as the reader.
// A wall UTC match is not sufficient and zero-value gates are invalid.
func (g *WarmGate) CheckClock(c clock.Clock) error {
	if g == nil || g.clock == nil || c == nil {
		return storage.ErrInvalid
	}
	_, err := g.clock.Now().Instant.Compare(c.Now().Instant)
	return err
}
func (g *WarmGate) Acquire(ctx context.Context, object storage.ObjectKey, deadline clock.Instant) (*WarmView, error) {
	if g == nil || g.clock == nil || ctx == nil || object.Validate() != nil {
		return nil, storage.ErrInvalid
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	v := &warmView{owner: g, deadline: deadline, object: object, resolving: true, pins: map[storage.PackRef]bool{}, incarnation: g.incarnation, lease: deadline, leaseRevision: 1}
	if g.limits.Lifetime > 0 {
		var err error
		v.lease, err = leaseEnd(g.clock, deadline, g.limits.Lifetime)
		if err != nil {
			return nil, err
		}
	}
	if err := v.check(ctx); err != nil {
		return nil, err
	}
	g.resolving++
	return &WarmView{state: v}, nil
}

// Caller holds owner.mu. Expiry is sticky; equality never grants work.
func (v *warmView) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if v.released || v.lost || v.closing || v.owner.closed || v.incarnation != v.owner.incarnation {
		v.lost = true
		return storage.ErrFenced
	}
	cmp, err := v.owner.clock.Now().Instant.Compare(v.deadline)
	if err != nil {
		return err
	}
	if cmp >= 0 {
		v.lost = true
		return storage.ErrFenced
	}
	cmp, err = v.owner.clock.Now().Instant.Compare(v.lease)
	if err != nil {
		return err
	}
	if cmp >= 0 {
		v.lost = true
		return storage.ErrFenced
	}
	return nil
}
func (h *WarmView) Check(ctx context.Context) error {
	if h == nil || h.state == nil || ctx == nil {
		return storage.ErrInvalid
	}
	v := h.state
	v.owner.mu.Lock()
	defer v.owner.mu.Unlock()
	return v.check(ctx)
}
func (handle *WarmView) Bind(ctx context.Context, h storage.ObjectHead) error {
	if handle == nil || handle.state == nil || ctx == nil {
		return storage.ErrInvalid
	}
	v := handle.state
	v.owner.mu.Lock()
	defer v.owner.mu.Unlock()
	if err := v.check(ctx); err != nil {
		return err
	}
	if h.Object != v.object || h.Deleted || h.Revision == 0 || h.ManifestID == "" {
		return storage.ErrConflict
	}
	if v.root != "" && (v.root != h.ManifestID || v.revision != h.Revision) {
		return storage.ErrConflict
	}
	v.root = h.ManifestID
	v.revision = h.Revision
	return nil
}

// Pin batches are atomic under the same short lock as retirement admission.
// Keep resolving protection until every requested generation has been pinned.
func (h *WarmView) Pin(ctx context.Context, refs []storage.PackRef) error {
	if h == nil || h.state == nil || ctx == nil || len(refs) == 0 || len(refs) > 100 {
		return storage.ErrInvalid
	}
	v := h.state
	g := v.owner
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := v.check(ctx); err != nil {
		return err
	}
	if v.root == "" || !v.resolving {
		return storage.ErrInvalid
	}
	for _, r := range refs {
		if r.Validate() != nil {
			return storage.ErrInvalid
		}
		if g.retired[r] {
			return storage.ErrConflict
		}
	}
	for _, r := range refs {
		if !v.pins[r] {
			v.pins[r] = true
			g.pins[r]++
		}
	}
	return nil
}
func (h *WarmView) Resolved(ctx context.Context) error {
	if h == nil || h.state == nil || ctx == nil {
		return storage.ErrInvalid
	}
	v := h.state
	g := v.owner
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := v.check(ctx); err != nil {
		return err
	}
	if v.root == "" {
		return storage.ErrInvalid
	}
	if v.resolving {
		v.resolving = false
		g.resolving--
	}
	return nil
}

// Release is idempotent and closes new work immediately. Existing work tokens
// defer cleanup until Finish; callers without tokens must first quiesce their
// synchronous read/decode/emit. Cancellation/expiry alone never releases pins.
func (h *WarmView) Release() {
	if h == nil || h.state == nil {
		return
	}
	v := h.state
	g := v.owner
	g.mu.Lock()
	defer g.mu.Unlock()
	if v.released {
		return
	}
	v.closing = true
	if v.work != 0 {
		return
	}
	v.releaseLocked()
}

func (v *warmView) releaseLocked() {
	g := v.owner
	v.released = true
	if v.resolving {
		g.resolving--
		v.resolving = false
	}
	for r := range v.pins {
		g.pins[r]--
		if g.pins[r] == 0 {
			delete(g.pins, r)
		}
	}
	v.pins = nil
}

// NewR1Gate adds process-local incarnation and renewable lease-use budgets to
// the existing Warm boundary. These coordinates are not shared UTC evidence or
// a durable coordinator. A runtime must not enable destructive maintenance.
func NewR1Gate(c clock.Clock, incarnation string, limits LeaseLimits) (*WarmGate, error) {
	if incarnation == "" || limits.Validate() != nil {
		return nil, storage.ErrInvalid
	}
	g, err := NewWarmGate(c)
	if err != nil {
		return nil, err
	}
	g.incarnation, g.limits = incarnation, limits
	return g, nil
}

// Invalidate closes read admission permanently. With writers, use
// WriterRegistry.Invalidate to serialize their fence closure first. Restart
// creates a new gate; existing roots/pins still await quiescence.
func (g *WarmGate) Invalidate() {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = true
}

// WarmWork explicitly retains the view across all read/decode/emitter work.
// Copied tokens share completion and cannot double-decrement protection.
type WarmWork struct{ state *warmWork }
type warmWork struct {
	view     *warmView
	finished bool
}

func (h *WarmView) BeginWork(ctx context.Context) (*WarmWork, error) {
	if h == nil || h.state == nil || ctx == nil {
		return nil, storage.ErrInvalid
	}
	v := h.state
	v.owner.mu.Lock()
	defer v.owner.mu.Unlock()
	if err := v.check(ctx); err != nil {
		return nil, err
	}
	v.work++
	return &WarmWork{state: &warmWork{view: v}}, nil
}
func (w *WarmWork) Finish() {
	if w == nil || w.state == nil {
		return
	}
	s := w.state
	g := s.view.owner
	g.mu.Lock()
	defer g.mu.Unlock()
	if s.finished {
		return
	}
	s.finished = true
	s.view.work--
	if s.view.closing && s.view.work == 0 {
		s.view.releaseLocked()
	}
}

// Heartbeat renews only a still-live local lease, never a lost view or hard
// request deadline. Lease equality is expired even if a heartbeat arrives now.
func (h *WarmView) Heartbeat(ctx context.Context) error {
	if h == nil || h.state == nil || ctx == nil {
		return storage.ErrInvalid
	}
	v := h.state
	g := v.owner
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.limits.Lifetime == 0 {
		return v.check(ctx)
	}
	if v.leaseRevision == ^uint64(0) {
		return storage.ErrInvalid
	}
	next, err := leaseEnd(g.clock, v.deadline, g.limits.Lifetime)
	if err != nil {
		return err
	}
	// Sample the candidate BEFORE validating the old lease. A stall between
	// validation and a fresh sample must not manufacture a later renewal budget.
	if err := v.check(ctx); err != nil {
		return err
	}
	v.lease = next
	v.leaseRevision++
	return nil
}

// KeepAlive must be stopped AND joined before view release. Cancellation asks
// workers to stop; only their return proves quiescence. Sleep uses the injected
// Clock, including Virtual Clock; goroutine execution itself is not deterministic.
func (h *WarmView) KeepAlive(ctx context.Context) (context.Context, func(), error) {
	if err := h.Check(ctx); err != nil {
		return nil, nil, err
	}
	return keepAlive(ctx, h.state.owner.clock, h.state.owner.limits.Heartbeat, h.Heartbeat)
}

// Retire closes local admission before a caller performs physical maintenance.
// Failure/unknown backend outcomes must not reopen this generation. This is not
// a GC eligibility proof; reachability/retention and distributed R1 are separate.
func (g *WarmGate) Retire(ref storage.PackRef) error {
	if ref.Validate() != nil {
		return storage.ErrInvalid
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.resolving != 0 || g.pins[ref] != 0 {
		return storage.ErrConflict
	}
	g.retired[ref] = true
	return nil
}
