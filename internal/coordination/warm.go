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
// boundary. Distributed R1 leases/maintenance remain W03-PHONG-02. No lock is
// held across a GET. Restart loses these handles, never reconstructs authority.
type WarmGate struct {
	mu        sync.Mutex
	clock     clock.Clock
	resolving int
	pins      map[storage.PackRef]int
	retired   map[storage.PackRef]bool
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
	v := &warmView{owner: g, deadline: deadline, object: object, resolving: true, pins: map[storage.PackRef]bool{}}
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
	if v.released || v.lost {
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

// Release is idempotent. It must not run while synchronous read/decode/emit is
// using the view. Cancellation or deadline expiry alone does not release pins.
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
