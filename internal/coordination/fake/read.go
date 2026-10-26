// Package fake provides an in-memory executable coordination model for fault
// tests. Its mutex is a model LP, not proof of a distributed transaction. It is
// not a MetadataStore, authorization provider or production R1 implementation.
package fake

import (
	"errors"
	"sync"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

var (
	ErrInvalid  = storage.ErrInvalid
	ErrConflict = storage.ErrConflict
	ErrLost     = errors.New("coordination: execution authority lost")
	ErrBusy     = errors.New("coordination: protected or in-flight")
)

// Seed is trusted fixture state, never evidence of real authorization. Roots
// enumerate logical keys; locations may change after binding a logical root.
type Seed struct {
	Heads  []storage.ObjectHead
	Roots  map[string][]storage.ChunkKey
	Chunks []storage.Chunk
	Packs  []storage.PackRef
}

type Coordinator struct {
	mu          sync.Mutex
	clock       clock.Clock
	incarnation string
	batchLimit  int
	heads       map[storage.ObjectKey]storage.ObjectHead
	roots       map[string]map[storage.ChunkKey]bool
	chunks      map[storage.ChunkKey]storage.Chunk
	packs       map[storage.PackRef]bool // true means retired, never reused
	views       map[string]*viewState
}

type viewState struct {
	object                         storage.ObjectKey
	incarnation                    string
	deadline                       clock.Instant
	lease                          clock.Instant
	initialLease                   clock.Instant
	leaseRevision                  uint64
	root                           string
	rootRevision                   uint64
	guard, closing, released, lost bool
	pins                           map[storage.PackRef]bool
	batches                        map[string][]Location
	work                           int
}

// Handles contain private pointers and are bound to their creator. Copies share
// lifecycle. They cannot be reconstructed from IDs or serialized into authority.
type View struct {
	owner *Coordinator
	state *viewState
}
type Work struct{ token *workState }
type workState struct {
	owner    *Coordinator
	state    *viewState
	finished bool
}
type Location struct {
	Key      storage.ChunkKey
	Revision uint64
	Pack     storage.PackRef
}

func New(c clock.Clock, incarnation string, batchLimit int, seed Seed) (*Coordinator, error) {
	if c == nil || incarnation == "" || batchLimit <= 0 {
		return nil, ErrInvalid
	}
	m := &Coordinator{clock: c, incarnation: incarnation, batchLimit: batchLimit, heads: map[storage.ObjectKey]storage.ObjectHead{}, roots: map[string]map[storage.ChunkKey]bool{}, chunks: map[storage.ChunkKey]storage.Chunk{}, packs: map[storage.PackRef]bool{}, views: map[string]*viewState{}}
	for _, p := range seed.Packs {
		if p.Validate() != nil {
			return nil, ErrInvalid
		}
		m.packs[p] = false
	}
	for _, c := range seed.Chunks {
		if c.Validate() != nil {
			return nil, ErrInvalid
		}
		if _, ok := m.packs[c.Location.Pack]; !ok {
			return nil, ErrInvalid
		}
		if _, ok := m.chunks[c.Key]; ok {
			return nil, ErrInvalid
		}
		m.chunks[c.Key] = c
	}
	for id, keys := range seed.Roots {
		if id == "" {
			return nil, ErrInvalid
		}
		r := map[storage.ChunkKey]bool{}
		for _, k := range keys {
			if k.Validate() != nil {
				return nil, ErrInvalid
			}
			r[k] = true
		}
		m.roots[id] = r
	}
	for _, h := range seed.Heads {
		if h.Object.Validate() != nil || h.Revision == 0 || h.Version != storage.ModelVersion {
			return nil, ErrInvalid
		}
		if _, ok := m.roots[h.ManifestID]; !ok {
			return nil, ErrInvalid
		}
		h.Metadata = nil
		h.PageIndex = nil
		m.heads[h.Object] = h
	}
	return m, nil
}

func (m *Coordinator) valid(v View) (*viewState, error) {
	if v.owner != m || v.state == nil {
		return nil, ErrInvalid
	}
	return v.state, nil
}
func (m *Coordinator) live(s *viewState) error {
	if s.lost || s.incarnation != m.incarnation {
		s.lost = true
		return ErrLost
	}
	n, e := m.clock.Now().Instant.Compare(s.deadline)
	if e != nil {
		return e
	}
	if n >= 0 {
		s.lost = true
		return ErrLost
	}
	n, e = m.clock.Now().Instant.Compare(s.lease)
	if e != nil {
		return e
	}
	if n >= 0 {
		s.lost = true
		return ErrLost
	}
	if h, ok := m.heads[s.object]; !ok || h.Object.BucketID != s.object.BucketID {
		s.lost = true
		return ErrLost
	}
	if s.closing || s.released {
		return ErrLost
	}
	return nil
}

// Acquire models admission AFTER external read authorization. Deadline is a
// conservative local use budget, not a UTC lease/skew proof. Equality expires.
func (m *Coordinator) Acquire(id string, object storage.ObjectKey, deadline clock.Instant) (View, error) {
	return m.AcquireWithLease(id, object, deadline, deadline)
}

// AcquireWithLease separates the immutable request deadline from a renewable
// local lease-use budget. Both are injected conservative monotonic coordinates.
func (m *Coordinator) AcquireWithLease(id string, object storage.ObjectKey, deadline, lease clock.Instant) (View, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if id == "" || object.Validate() != nil {
		return View{}, ErrInvalid
	}
	n, err := lease.Compare(deadline)
	if err != nil {
		return View{}, err
	}
	if n > 0 {
		return View{}, ErrInvalid
	}
	if s := m.views[id]; s != nil {
		cmp, e := s.initialLease.Compare(lease)
		if e != nil {
			return View{}, e
		}
		if cmp != 0 {
			return View{}, ErrConflict
		}
		n, e := s.deadline.Compare(deadline)
		if e != nil {
			return View{}, e
		}
		if s.object != object || n != 0 {
			return View{}, ErrConflict
		}
		if e = m.live(s); e != nil {
			return View{}, e
		}
		return View{m, s}, nil
	}
	s := &viewState{object: object, incarnation: m.incarnation, deadline: deadline, lease: lease, initialLease: lease, leaseRevision: 1, guard: true, pins: map[storage.PackRef]bool{}, batches: map[string][]Location{}}
	if e := m.live(s); e != nil {
		return View{}, e
	}
	m.views[id] = s
	return View{m, s}, nil
}

// Renew cannot resurrect a lost view or move the hard request deadline.
func (m *Coordinator) Renew(v View, expected uint64, next clock.Instant) (uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, e := m.valid(v)
	if e != nil {
		return 0, e
	}
	if e = m.live(s); e != nil {
		return 0, e
	}
	if s.leaseRevision != expected || expected == ^uint64(0) {
		return 0, ErrConflict
	}
	cmp, e := next.Compare(s.lease)
	if e != nil {
		return 0, e
	}
	if cmp <= 0 {
		return 0, ErrInvalid
	}
	cmp, e = next.Compare(s.deadline)
	if e != nil {
		return 0, e
	}
	if cmp > 0 {
		return 0, ErrInvalid
	}
	s.lease = next
	s.leaseRevision++
	return s.leaseRevision, nil
}

// BindRoot atomically checks the observed head, attaches its root and drops the
// resolving guard. Failed CAS leaves the guard in place. Root != generation pin.
func (m *Coordinator) BindRoot(v View, revision uint64, manifest string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, e := m.valid(v)
	if e != nil {
		return e
	}
	if e = m.live(s); e != nil {
		return e
	}
	if s.root != "" {
		if s.root == manifest && s.rootRevision == revision {
			return nil
		}
		return ErrConflict
	}
	h := m.heads[s.object]
	if h.Deleted || h.Revision != revision || h.ManifestID != manifest {
		return ErrConflict
	}
	s.root = manifest
	s.rootRevision = revision
	s.guard = false
	return nil
}

// Pin validates every location before applying a bounded atomic batch. Caller
// must resolve again after a conflict; same-ID retries require identical input.
func (m *Coordinator) Pin(v View, id string, locations []Location) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, e := m.valid(v)
	if e != nil {
		return e
	}
	if e = m.live(s); e != nil {
		return e
	}
	if id == "" || len(locations) == 0 || len(locations) > m.batchLimit || s.root == "" {
		return ErrInvalid
	}
	if old, ok := s.batches[id]; ok {
		if len(old) != len(locations) {
			return ErrConflict
		}
		for i := range old {
			if old[i] != locations[i] {
				return ErrConflict
			}
		}
		return nil
	}
	for _, l := range locations {
		c, ok := m.chunks[l.Key]
		retired, exists := m.packs[l.Pack]
		if !ok || !exists || retired || !m.roots[s.root][l.Key] || c.LocationRevision != l.Revision || c.Location.Pack != l.Pack {
			return ErrConflict
		}
	}
	for _, l := range locations {
		s.pins[l.Pack] = true
	}
	s.batches[id] = append([]Location(nil), locations...)
	return nil
}

// BeginWork validates exact generation coverage. Caller still owns logical
// range authorization, whole-range archive preflight, verification and emission.
func (m *Coordinator) BeginWork(v View, ref storage.PackRef) (*Work, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, e := m.valid(v)
	if e != nil {
		return nil, e
	}
	if e = m.live(s); e != nil {
		return nil, e
	}
	if !s.pins[ref] {
		return nil, ErrConflict
	}
	s.work++
	return &Work{token: &workState{owner: m, state: s}}, nil
}
func (m *Coordinator) ValidateWork(w *Work) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w == nil || w.token == nil || w.token.owner != m || w.token.finished {
		return ErrInvalid
	}
	return m.live(w.token.state)
}
func (m *Coordinator) FinishWork(w *Work) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w == nil || w.token == nil || w.token.owner != m {
		return ErrInvalid
	}
	if !w.token.finished {
		w.token.finished = true
		w.token.state.work--
	}
	return nil
}
func (m *Coordinator) BeginRelease(v View) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, e := m.valid(v)
	if e != nil {
		return e
	}
	s.closing = true
	return nil
}
func (m *Coordinator) FinishRelease(v View) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, e := m.valid(v)
	if e != nil {
		return e
	}
	if s.released {
		return nil
	}
	if !s.closing {
		return ErrInvalid
	}
	if s.work != 0 {
		return ErrBusy
	}
	s.pins = nil
	s.root = ""
	s.guard = false
	s.released = true
	return nil
}

// Retire tests only the guard/pin interlock, assuming the fixture driver has
// established independent logical reachability, protection and retention gates.
func (m *Coordinator) Retire(ref storage.PackRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.packs[ref]; !ok {
		return storage.ErrNotFound
	}
	for _, s := range m.views {
		if s.guard || s.pins[ref] {
			return ErrBusy
		}
	}
	m.packs[ref] = true
	return nil
}

// Move models location CAS before retirement; an old pinned generation remains
// usable. It does not implement compaction physical durability or R2 outbox.
func (m *Coordinator) Move(key storage.ChunkKey, expected uint64, next storage.ChunkLocation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.chunks[key]
	retired, exists := m.packs[next.Pack]
	if !ok || c.LocationRevision != expected || expected == ^uint64(0) || !exists || retired || next.Validate() != nil {
		return ErrConflict
	}
	c.Location = next
	c.LocationRevision++
	m.chunks[key] = c
	return nil
}

// ChangeHead models overwrite/delete, not logical root reclamation.
func (m *Coordinator) ChangeHead(object storage.ObjectKey, expected uint64, manifest string, deleted bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	h, ok := m.heads[object]
	if !ok || h.Revision != expected || expected == ^uint64(0) {
		return ErrConflict
	}
	if !deleted {
		if _, ok := m.roots[manifest]; !ok {
			return ErrInvalid
		}
	}
	h.Revision++
	h.ManifestID = manifest
	h.Deleted = deleted
	m.heads[object] = h
	return nil
}
func (m *Coordinator) ReclaimRoot(root string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.views {
		if s.guard || s.root == root {
			return ErrBusy
		}
	}
	for _, h := range m.heads {
		if !h.Deleted && h.ManifestID == root {
			return ErrBusy
		}
	}
	delete(m.roots, root)
	return nil
}

// Restart invalidates execution authority but retains protection for cleanup.
func (m *Coordinator) Restart(incarnation string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if incarnation == "" || incarnation == m.incarnation {
		return ErrInvalid
	}
	m.incarnation = incarnation
	for _, s := range m.views {
		s.lost = true
	}
	return nil
}

func (m *Coordinator) InvalidateBucket(bucketID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.views {
		if s.object.BucketID == bucketID {
			s.lost = true
		}
	}
	for k := range m.heads {
		if k.BucketID == bucketID {
			delete(m.heads, k)
		}
	}
}
