// Package readpath contains local read resource primitives, not read authority.
package readpath

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/KinasTomes/miles/internal/config"
)

var (
	ErrAdmissionInvalid = errors.New("invalid admission input")
	ErrExhausted        = errors.New("read resource exhausted")
	ErrTicketClosed     = errors.New("read request closed")
)

// ExhaustionError is nonblocking. Impossible means the cost cannot ever fit
// the configured limit. Retrying requires caller policy; there is no queue.
type ExhaustionError struct {
	Scope, Resource string
	Impossible      bool
}

func (e *ExhaustionError) Error() string {
	return fmt.Sprintf("%s: %s/%s (impossible=%t)", ErrExhausted, e.Scope, e.Resource, e.Impossible)
}
func (e *ExhaustionError) Unwrap() error { return ErrExhausted }

// Cost excludes LRU resident memory. Bytes must cover actual simultaneously
// owned buffers, including cache-hit copies and decoder workspace. The caller
// must reserve before allocation/I/O and retain the reservation until quiescent.
// Each reservation requires at least one ChunkSlot, bounding live reservation
// count. Other dimensions may be zero for operations that do not use them.
type Cost struct{ ChunkSlots, StoredBytes, IOSlots, IOBytes, RawBytes, WorkspaceBytes, CopyBytes int64 }

func (c Cost) values() [7]int64 {
	return [7]int64{c.ChunkSlots, c.StoredBytes, c.IOSlots, c.IOBytes, c.RawBytes, c.WorkspaceBytes, c.CopyBytes}
}

var resourceNames = [7]string{"chunks", "stored", "io_slots", "io_bytes", "raw", "workspace", "copy"}

// Budget is explicit; no production defaults are provided. Prefetch is a
// sublimit, strictly below Capacity in each dimension, leaving demand headroom.
// Zero Prefetch disables speculation. Queue/fairness policy remains PROPOSAL:
// this guarantees bounded speculation, not starvation freedom between callers.
type Budget struct{ Capacity, Prefetch Cost }
type ScopeLimits struct {
	ActiveRequests int64
	Budget         Budget
}
type Limits struct{ Node, Bucket ScopeLimits }
type BucketBinding struct {
	Name   string
	Policy config.BucketSnapshot
}

type usage struct{ total, prefetch [7]int64 }
type bucketState struct {
	name   string
	active int64
	used   usage
}

// Admission uses one short accounting critical section, never holding it across
// I/O, callbacks, waits or decoding. It is node-local, not a durable quota store.
// Construct with NewAdmission; do not copy an Admission after use. APIs are a
// PROPOSAL for scheduler integration, not frozen shared ports. Context checks
// observe cancellation at admission; a concurrent cancellation may race a grant.
// In that case the caller still owns and must drain/release the returned grant.
type Admission struct {
	mu      sync.Mutex
	limits  Limits
	active  int64
	used    usage
	buckets map[string]*bucketState
}
type requestState struct {
	owner             *Admission
	bucket            *bucketState
	binding           BucketBinding
	ctx               context.Context
	budget            Budget
	used              usage
	reservations      int64
	closing, finished bool
}

// RequestTicket copies share private lifecycle state. Zero/foreign handles are
// invalid. A pinned policy revision does not create a new bucket quota pool.
type RequestTicket struct{ state *requestState }
type reservationState struct {
	request            *requestState
	cost               [7]int64
	prefetch, released bool
}
type Reservation struct{ state *reservationState }

func validBudget(b Budget) bool {
	c, p := b.Capacity.values(), b.Prefetch.values()
	for i := range c {
		if c[i] <= 0 || p[i] < 0 || p[i] >= c[i] {
			return false
		}
	}
	return true
}
func NewAdmission(l Limits) (*Admission, error) {
	if l.Node.ActiveRequests <= 0 || l.Bucket.ActiveRequests <= 0 || !validBudget(l.Node.Budget) || !validBudget(l.Bucket.Budget) {
		return nil, ErrAdmissionInvalid
	}
	return &Admission{limits: l, buckets: make(map[string]*bucketState)}, nil
}
func (a *Admission) AdmitRequest(ctx context.Context, binding BucketBinding, budget Budget) (*RequestTicket, error) {
	if a == nil || ctx == nil || binding.Name == "" || binding.Policy.BucketID() == "" || binding.Policy.Revision() == 0 || !validBudget(budget) {
		return nil, ErrAdmissionInvalid
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := binding.Policy.BucketID()
	b := a.buckets[id]
	if b != nil && b.name != binding.Name {
		return nil, ErrAdmissionInvalid
	}
	if a.active >= a.limits.Node.ActiveRequests {
		return nil, &ExhaustionError{Scope: "node", Resource: "requests"}
	}
	if b != nil && b.active >= a.limits.Bucket.ActiveRequests {
		return nil, &ExhaustionError{Scope: "bucket", Resource: "requests"}
	}
	if b == nil {
		b = &bucketState{name: binding.Name}
		a.buckets[id] = b
	}
	a.active++
	b.active++
	return &RequestTicket{state: &requestState{owner: a, bucket: b, binding: binding, ctx: ctx, budget: budget}}, nil
}
func (a *Admission) ReserveDemand(ctx context.Context, t *RequestTicket, c Cost) (*Reservation, error) {
	if ctx == nil {
		return nil, ErrAdmissionInvalid
	}
	return a.reserve(ctx, t, c, false)
}
func (a *Admission) TryReservePrefetch(t *RequestTicket, c Cost) (*Reservation, error) {
	return a.reserve(context.Background(), t, c, true)
}
func (a *Admission) reserve(ctx context.Context, t *RequestTicket, c Cost, prefetch bool) (*Reservation, error) {
	if a == nil || t == nil || t.state == nil || t.state.owner != a {
		return nil, ErrAdmissionInvalid
	}
	v := c.values()
	if v[0] <= 0 {
		return nil, ErrAdmissionInvalid
	}
	for _, n := range v {
		if n < 0 {
			return nil, ErrAdmissionInvalid
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	r := t.state
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.ctx.Err(); err != nil {
		return nil, err
	}
	if r.closing {
		return nil, ErrTicketClosed
	}
	scopes := []struct {
		name   string
		budget Budget
		used   *usage
	}{{"node", a.limits.Node.Budget, &a.used}, {"bucket", a.limits.Bucket.Budget, &r.bucket.used}, {"request", r.budget, &r.used}}
	// Check all absolute limits before availability, so impossible costs are
	// distinguished even when another scope is temporarily exhausted.
	for _, s := range scopes {
		cap, pre := s.budget.Capacity.values(), s.budget.Prefetch.values()
		for i, n := range v {
			if n > cap[i] || prefetch && n > pre[i] {
				return nil, &ExhaustionError{s.name, resourceNames[i], true}
			}
		}
	}
	for _, s := range scopes {
		cap, pre := s.budget.Capacity.values(), s.budget.Prefetch.values()
		for i, n := range v {
			if n > cap[i]-s.used.total[i] || prefetch && n > pre[i]-s.used.prefetch[i] {
				return nil, &ExhaustionError{s.name, resourceNames[i], false}
			}
		}
	}
	for _, s := range scopes {
		for i, n := range v {
			s.used.total[i] += n
			if prefetch {
				s.used.prefetch[i] += n
			}
		}
	}
	r.reservations++
	return &Reservation{state: &reservationState{request: r, cost: v, prefetch: prefetch}}, nil
}

// Release is idempotent, including through copied handles. Call only after all
// owned work/buffers have quiesced. Cancellation does NOT release resources.
func (r *Reservation) Release() {
	if r == nil || r.state == nil {
		return
	}
	s := r.state
	a := s.request.owner
	a.mu.Lock()
	defer a.mu.Unlock()
	if s.released {
		return
	}
	s.released = true
	q := s.request
	for _, u := range []*usage{&a.used, &q.bucket.used, &q.used} {
		for i, n := range s.cost {
			u.total[i] -= n
			if s.prefetch {
				u.prefetch[i] -= n
			}
		}
	}
	q.reservations--
	q.finish()
}

// Close prevents new reservations. True means accounting is fully drained.
// Live reservations keep the request/bucket active until their final Release.
// It does not cancel or join consumer work, nor release authorization or pins.
func (t *RequestTicket) Close() bool {
	if t == nil || t.state == nil {
		return true
	}
	r := t.state
	r.owner.mu.Lock()
	defer r.owner.mu.Unlock()
	r.closing = true
	r.finish()
	return r.finished
}
func (r *requestState) finish() {
	if !r.closing || r.finished || r.reservations != 0 {
		return
	}
	r.finished = true
	a := r.owner
	a.active--
	r.bucket.active--
	if r.bucket.active == 0 {
		delete(a.buckets, r.binding.Policy.BucketID())
	}
}
