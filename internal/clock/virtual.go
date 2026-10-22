package clock

import (
	"container/heap"
	"context"
	"fmt"
	"math"
	"runtime"
	"sync"
	"time"
)

// Virtual is driven explicitly by one scheduler. AfterFunc callbacks run
// synchronously in (due Instant, insertion sequence) order, outside the state
// lock. Callbacks may schedule/cancel events or set wall time, but must not block
// in Sleep or call Advance recursively. Concurrent Advance returns ErrAdvancing.
//
// Determinism requires the driver to serialize event registration and explicit
// cancellations in trace order. Concurrent registration has lock-acquisition
// order; goroutines woken by Sleep have NO deterministic execution order. Model
// decisions belong in AfterFunc callbacks, not in those goroutines. In virtual
// tests, use scheduler-driven cancellation rather than host context timeouts.
// A Virtual must not be copied after construction.
type Virtual struct {
	mu       sync.Mutex
	driver   sync.Mutex
	domain   *domain
	wall     time.Time
	now      int64
	sequence uint64
	budget   int
	events   eventHeap
}

// NewVirtual starts elapsed time at zero. maxEventsPerAdvance is an explicit
// caller budget (including zero-delay events), with no assumed production
// default. Wall values are restricted to RFC3339 years 1..9999.
func NewVirtual(wall time.Time, maxEventsPerAdvance int) (*Virtual, error) {
	if maxEventsPerAdvance <= 0 {
		return nil, ErrBudget
	}
	if !validWall(wall) {
		return nil, ErrOverflow
	}
	return &Virtual{domain: &domain{}, wall: wall.UTC(), budget: maxEventsPerAdvance}, nil
}

func validWall(w time.Time) bool { return w.UTC().Year() >= 1 && w.UTC().Year() <= 9999 }

func (v *Virtual) Now() Reading {
	v.mu.Lock()
	defer v.mu.Unlock()
	return Reading{WallUTC: v.wall, Instant: Instant{domain: v.domain, ticks: v.now}}
}

// SetWall changes wall UTC alone, including backwards jumps. It never runs events
// or changes pending elapsed deadlines.
func (v *Virtual) SetWall(wall time.Time) error {
	if !validWall(wall) {
		return ErrOverflow
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.wall = wall.UTC()
	return nil
}

// Event is a one-shot virtual callback. Cancel removes its queued state eagerly.
// Cancel returns false once selected for execution or previously cancelled; it
// cannot revoke an in-flight callback. Cancel is safe from the callback itself.
// Copies share the same event state. A zero-value Event is already inactive.
type Event struct {
	event *event
}

type event struct {
	clock    *Virtual
	due      int64
	sequence uint64
	index    int
	callback func()
	// Only Sleep events have a context. The driver filters cancelled sleepers
	// itself, so goroutine cleanup timing cannot consume the event budget.
	ctx context.Context
}

func (e *Event) Cancel() bool {
	if e == nil || e.event == nil {
		return false
	}
	node := e.event
	v := node.clock
	v.mu.Lock()
	defer v.mu.Unlock()
	if node.index < 0 {
		return false
	}
	heap.Remove(&v.events, node.index)
	node.callback = nil
	return true
}

func (v *Virtual) AfterFunc(d time.Duration, fn func()) (*Event, error) {
	return v.schedule(d, fn, nil)
}

func (v *Virtual) schedule(d time.Duration, fn func(), ctx context.Context) (*Event, error) {
	if d < 0 {
		return nil, ErrDuration
	}
	if fn == nil {
		return nil, ErrCallback
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if ctx != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	due, err := add(v.now, int64(d))
	if err != nil {
		return nil, err
	}
	if v.sequence == math.MaxUint64 {
		return nil, ErrOverflow
	}
	v.sequence++
	e := &event{clock: v, due: due, sequence: v.sequence, callback: fn, ctx: ctx}
	heap.Push(&v.events, e)
	return &Event{event: e}, nil
}

// Pending counts live queued events, excluding cancelled sleepers and callbacks
// already selected for execution.
func (v *Virtual) Pending() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Observation must not rearrange the heap. Cancel and Advance own cleanup.
	live := 0
	for _, e := range v.events {
		if e.ctx == nil || e.ctx.Err() == nil {
			live++
		}
	}
	return live
}

func (v *Virtual) Sleep(ctx context.Context, d time.Duration) error {
	if d < 0 {
		return ErrDuration
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if d == 0 {
		runtime.Gosched()
		return ctx.Err()
	}
	done := make(chan struct{})
	event, err := v.schedule(d, func() { close(done) }, ctx)
	if err != nil {
		return err
	}
	defer event.Cancel()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return ctx.Err()
	}
}

// Advance moves elapsed and wall time by d, visiting every intermediate event.
// SetWall can inject independent wall jumps, including from callbacks. A negative
// or initially overflowing advance has no effects. Event-limit errors leave
// progress at the last dispatched event, with remaining events queued; no work
// is silently dropped. A callback panic propagates with locks released and the
// selected event consumed. Neither errors nor panics roll back prior callbacks.
func (v *Virtual) Advance(d time.Duration) error {
	if d < 0 {
		return ErrDuration
	}
	if !v.driver.TryLock() {
		return ErrAdvancing
	}
	defer v.driver.Unlock()
	v.mu.Lock()
	target, err := add(v.now, int64(d))
	if err != nil || !validWall(v.wall.Add(d)) {
		v.mu.Unlock()
		return ErrOverflow
	}
	v.mu.Unlock()
	for count := 0; ; {
		v.mu.Lock()
		for len(v.events) > 0 {
			e := v.events[0]
			if e.ctx == nil || e.ctx.Err() == nil {
				break
			}
			heap.Pop(&v.events)
			e.callback = nil
		}
		if len(v.events) == 0 || v.events[0].due > target {
			err = v.move(target)
			v.mu.Unlock()
			return err
		}
		if count == v.budget {
			err := fmt.Errorf("%w: processed=%d pending=%d remaining=%s", ErrEventLimit, count, len(v.events), time.Duration(target-v.now))
			v.mu.Unlock()
			return err
		}
		e := v.events[0]
		if err = v.move(e.due); err != nil {
			v.mu.Unlock()
			return err
		}
		heap.Pop(&v.events)
		fn := e.callback
		e.callback = nil
		count++
		v.mu.Unlock()
		fn()
	}
}

// move is called with mu held. Callbacks may have changed wall since entry.
func (v *Virtual) move(target int64) error {
	w := v.wall.Add(time.Duration(target - v.now))
	if !validWall(w) {
		return ErrOverflow
	}
	v.wall, v.now = w, target
	return nil
}

type eventHeap []*event

func (h eventHeap) Len() int { return len(h) }
func (h eventHeap) Less(i, j int) bool {
	if h[i].due == h[j].due {
		return h[i].sequence < h[j].sequence
	}
	return h[i].due < h[j].due
}
func (h eventHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].index = i
	h[j].index = j
}
func (h *eventHeap) Push(x any) {
	e := x.(*event)
	e.index = len(*h)
	*h = append(*h, e)
}
func (h *eventHeap) Pop() any {
	old := *h
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	e.index = -1
	return e
}

var _ Clock = (*Virtual)(nil)
