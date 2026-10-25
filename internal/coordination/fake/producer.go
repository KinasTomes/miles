package fake

import (
	"sort"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
)

type Outcome uint8

const (
	Pending Outcome = iota
	Committed
	NoCommit
)

type Gate uint8

const (
	Open Gate = iota
	Closing
	Closed
)

type Producer struct {
	owner *Producers
	state *producerState
}
type Mutation struct {
	owner *Producers
	state *mutationState
}
type producerState struct {
	id                                  string
	deadline                            clock.Instant
	attachmentClosed, fenceClosed, lost bool
	mutations                           map[string]*mutationState
	generation                          uint64
}
type mutationState struct {
	producer *producerState
	input    string
	outcome  Outcome
}

// Producers models ONE recovery epoch/cohort. Epoch identity is not execution
// authority. The clock budget is a fixture oracle, not production UTC proof.
// All publication and closure LPs share this mutex; no backend is implemented.
type Producers struct {
	mu        sync.Mutex
	clock     clock.Clock
	gate      Gate
	producers map[string]*producerState
	mutations map[string]*mutationState
}

func NewProducers(c clock.Clock) (*Producers, error) {
	if c == nil {
		return nil, ErrInvalid
	}
	return &Producers{clock: c, producers: map[string]*producerState{}, mutations: map[string]*mutationState{}}, nil
}
func (m *Producers) live(p *producerState) error {
	if p.lost || p.fenceClosed {
		return ErrLost
	}
	n, e := m.clock.Now().Instant.Compare(p.deadline)
	if e != nil {
		return e
	}
	if n >= 0 {
		p.lost = true
		return ErrLost
	}
	return nil
}
func (m *Producers) Register(id string, deadline clock.Instant) (Producer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" {
		return Producer{}, ErrInvalid
	}
	if p := m.producers[id]; p != nil {
		n, e := p.deadline.Compare(deadline)
		if e != nil {
			return Producer{}, e
		}
		if n != 0 {
			return Producer{}, ErrConflict
		}
		return Producer{m, p}, nil
	}
	if m.gate != Open {
		return Producer{}, ErrBusy
	}
	p := &producerState{id: id, deadline: deadline, mutations: map[string]*mutationState{}, generation: 1}
	if e := m.live(p); e != nil {
		return Producer{}, e
	}
	m.producers[id] = p
	return Producer{m, p}, nil
}

// Dispatch registers an immutable mutation before a simulated side effect.
// Closing freezes producers, not mutations of the admitted healthy cohort.
// A retry returns the same receipt; it never grants fresh dispatch permission.
func (m *Producers) Dispatch(p Producer, id, input string) (Mutation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.owner != m || p.state == nil || id == "" || input == "" {
		return Mutation{}, ErrInvalid
	}
	s := p.state
	if old := s.mutations[id]; old != nil {
		if old.input != input {
			return Mutation{}, ErrConflict
		}
		return Mutation{m, old}, nil
	}
	if _, exists := m.mutations[id]; exists {
		return Mutation{}, ErrConflict
	}
	if e := m.live(s); e != nil {
		return Mutation{}, e
	}
	if s.attachmentClosed || m.gate == Closed {
		return Mutation{}, ErrLost
	}
	x := &mutationState{producer: s, input: input}
	s.mutations[id] = x
	m.mutations[id] = x
	return Mutation{m, x}, nil
}

// Commit models metadata publication of an ALREADY dispatched operation. Lease
// expiry alone does not abort it; shared fence closure does. Independent request,
// auth and quota predicates are outside this model, not waived in production.
func (m *Producers) Commit(x Mutation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if x.owner != m || x.state == nil {
		return ErrInvalid
	}
	s := x.state
	if s.outcome == Committed {
		return nil
	}
	if s.outcome == NoCommit || s.producer.fenceClosed || m.gate == Closed {
		return ErrLost
	}
	s.outcome = Committed
	return nil
}
func (m *Producers) CloseProducer(p Producer) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.owner != m || p.state == nil {
		return ErrInvalid
	}
	p.state.attachmentClosed = true
	p.state.fenceClosed = true
	return nil
}

// Reconcile uses the model's retained outcome only AFTER fence closure. It is
// not an absence read against a live backend and never erases committed state.
func (m *Producers) Reconcile(x Mutation) (Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if x.owner != m || x.state == nil {
		return Pending, ErrInvalid
	}
	s := x.state
	if s.outcome != Pending {
		return s.outcome, nil
	}
	if !s.producer.fenceClosed {
		return Pending, ErrBusy
	}
	s.outcome = NoCommit
	return s.outcome, nil
}
func (m *Producers) BeginClose() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gate == Closed {
		return ErrConflict
	}
	m.gate = Closing
	return nil
}
func (m *Producers) FinishClose() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gate == Closed {
		return nil
	}
	if m.gate != Closing {
		return ErrInvalid
	}
	for _, p := range m.producers {
		if !p.attachmentClosed || !p.fenceClosed {
			return ErrBusy
		}
		for _, x := range p.mutations {
			if x.outcome == Pending {
				return ErrBusy
			}
		}
	}
	m.gate = Closed
	return nil
}

// AbortClose reopens producer admission, never old closed publication fences.
func (m *Producers) AbortClose() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gate != Closing {
		return ErrConflict
	}
	m.gate = Open
	return nil
}

// Takeover is a two-step protocol: CloseProducer + Reconcile must precede it.
// The fixture clock supplies exact reclaim time; this is not shared UTC proof.
// Closing cohorts cannot gain new authorities. Grace is explicit, never a lease
// extension. Old handles remain closed and committed receipts remain available.
func (m *Producers) Takeover(old Producer, nextDeadline clock.Instant, grace time.Duration) (Producer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if old.owner != m || old.state == nil || grace < 0 {
		return Producer{}, ErrInvalid
	}
	p := old.state
	if m.producers[p.id] != p {
		return Producer{}, ErrConflict
	}
	if m.gate != Open || !p.fenceClosed || !p.attachmentClosed {
		return Producer{}, ErrBusy
	}
	eligible, err := p.deadline.Add(grace)
	if err != nil {
		return Producer{}, err
	}
	cmp, err := m.clock.Now().Instant.Compare(eligible)
	if err != nil {
		return Producer{}, err
	}
	if cmp < 0 {
		return Producer{}, ErrBusy
	}
	for _, x := range p.mutations {
		if x.outcome == Pending {
			return Producer{}, ErrBusy
		}
	}
	if p.generation == ^uint64(0) {
		return Producer{}, ErrInvalid
	}
	n := &producerState{id: p.id, deadline: nextDeadline, generation: p.generation + 1, mutations: map[string]*mutationState{}}
	if err := m.live(n); err != nil {
		return Producer{}, err
	}
	// Retain original receipts. A known-no-commit receipt is not automatically
	// a new attempt; that downstream protocol is deliberately not simulated.
	for id, x := range p.mutations {
		n.mutations[id] = x
	}
	m.producers[p.id] = n
	return Producer{m, n}, nil
}

// ClosedMembers returns stable committed mutation IDs only after closure.
// Mutation identities are unique across the epoch's producer cohort.
func (m *Producers) ClosedMembers() (map[string][]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gate != Closed {
		return nil, ErrBusy
	}
	out := map[string][]string{}
	for id, p := range m.producers {
		out[id] = nil
		for name, x := range p.mutations {
			if x.outcome == Committed {
				out[id] = append(out[id], name)
			}
		}
		sort.Strings(out[id])
	}
	return out, nil
}

// InvalidateAuthority models recovery/service-incarnation invalidation. It
// closes all existing fences without changing any recorded mutation outcome.
// The fixture driver must reconcile; a restart is not proof of no commit.
func (m *Producers) InvalidateAuthority() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.producers {
		p.lost = true
		p.fenceClosed = true
		p.attachmentClosed = true
	}
}
