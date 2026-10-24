// Package fake provides in-memory reference stores for contract tests only.
// It is not a production metadata adapter, an R1 coordinator or an AWS emulator.
package fake

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
)

type Seed struct {
	Now         func() time.Time
	Buckets     []s.Bucket
	Users       []s.User
	Credentials []s.Credential
	Grants      []s.Grant
	Quotas      []s.LogicalQuota
	Packs       []s.PackInfo
	Writers     []s.WriterLease
	Uploads     []s.Completion
}
type committed struct {
	Digest [32]byte
	Head   s.ObjectHead
}
type Metadata struct {
	mu                                sync.Mutex
	now                               func() time.Time
	buckets                           map[string]s.Bucket
	users                             map[string]s.User
	credentials                       map[string]s.Credential
	grants                            map[[2]string]s.Grant
	quotas                            map[string]s.LogicalQuota
	packs                             map[s.PackRef]s.PackInfo
	writers                           map[string]s.WriterLease
	uploads                           map[string]s.Completion
	chunks                            map[s.ChunkKey]s.Chunk
	manifests                         map[string]map[uint32]s.ManifestPage
	manifestSeals                     map[string]uint32
	heads                             map[s.ObjectKey]s.ObjectHead
	reservations                      map[string]s.Reservation
	events                            map[string]s.OutboxEvent
	receipts                          map[string]s.CompletedReceipt
	commits                           map[string]committed
	failBeforeCommit, failAfterCommit bool
	failReconcile                     bool
	policies                          map[uint64]s.PolicySnapshot
	currentPolicy                     uint64
}

var _ s.MetadataStore = (*Metadata)(nil)

func copyOf[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err = json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}
func NewMetadata(seed Seed) *Metadata {
	if seed.Now == nil {
		seed.Now = time.Now
	}
	m := &Metadata{policies: map[uint64]s.PolicySnapshot{}, now: seed.Now, buckets: map[string]s.Bucket{}, users: map[string]s.User{}, credentials: map[string]s.Credential{}, grants: map[[2]string]s.Grant{}, quotas: map[string]s.LogicalQuota{}, packs: map[s.PackRef]s.PackInfo{}, writers: map[string]s.WriterLease{}, uploads: map[string]s.Completion{}, chunks: map[s.ChunkKey]s.Chunk{}, manifests: map[string]map[uint32]s.ManifestPage{}, manifestSeals: map[string]uint32{}, heads: map[s.ObjectKey]s.ObjectHead{}, reservations: map[string]s.Reservation{}, events: map[string]s.OutboxEvent{}, receipts: map[string]s.CompletedReceipt{}, commits: map[string]committed{}}
	for _, v := range seed.Buckets {
		m.buckets[v.Name] = v
	}
	for _, v := range seed.Users {
		m.users[v.ID] = v
	}
	for _, v := range seed.Credentials {
		m.credentials[v.ID] = v
	}
	for _, v := range seed.Grants {
		m.grants[[2]string{v.BucketID, v.UserID}] = v
	}
	for _, v := range seed.Quotas {
		m.quotas[v.OwnerID] = v
	}
	for _, v := range seed.Packs {
		m.packs[v.Ref] = v
	}
	for _, v := range seed.Writers {
		m.writers[v.Fence.LeaseID] = v
	}
	for _, v := range seed.Uploads {
		m.uploads[v.UploadID] = v
	}
	return m
}

// Control hooks change authoritative fixture state, not production interfaces.
func (m *Metadata) SetBucket(v s.Bucket) { m.mu.Lock(); defer m.mu.Unlock(); m.buckets[v.Name] = v }
func (m *Metadata) SetCredential(v s.Credential) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.credentials[v.ID] = v
}
func (m *Metadata) SetGrant(v s.Grant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants[[2]string{v.BucketID, v.UserID}] = v
}
func (m *Metadata) SetQuota(v s.LogicalQuota) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.quotas[v.OwnerID] = v
}
func (m *Metadata) SetPack(v s.PackInfo) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.packs[v.Ref] = v
}
func (m *Metadata) SetWriter(v s.WriterLease) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writers[v.Fence.LeaseID] = v
}
func (m *Metadata) FailCommit(before, after bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failBeforeCommit, m.failAfterCommit = before, after
}
func (m *Metadata) fence(f s.WriterFence) error {
	lease, ok := m.writers[f.LeaseID]
	if !ok || lease.Fence != f || f.LeaseID == "" || f.ServiceIncarnation == "" || f.Epoch == 0 || !m.now().Before(lease.ExpiresAt) {
		return s.ErrFenced
	}
	return nil
}
func (m *Metadata) GetBucket(ctx context.Context, name string) (s.Bucket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.Bucket{}, err
	}
	v, ok := m.buckets[name]
	if !ok || !v.Active {
		return s.Bucket{}, s.ErrNotFound
	}
	return v, nil
}
func (m *Metadata) GetCredential(ctx context.Context, id string) (s.Credential, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.Credential{}, err
	}
	v, ok := m.credentials[id]
	if !ok {
		return s.Credential{}, s.ErrNotFound
	}
	return v, nil
}
func (m *Metadata) authorize(credential, bucket string) (s.Authorization, error) {
	c, ok := m.credentials[credential]
	if !ok || !c.Active {
		return s.Authorization{}, s.ErrAccessDenied
	}
	u, ok := m.users[c.UserID]
	if !ok || !u.Active {
		return s.Authorization{}, s.ErrAccessDenied
	}
	b, ok := m.buckets[bucket]
	if !ok || !b.Active {
		return s.Authorization{}, s.ErrNotFound
	}
	g := m.grants[[2]string{b.ID, u.ID}]
	owner := u.ID == b.OwnerID
	return s.Authorization{UserID: u.ID, CredentialID: c.ID, Bucket: b, Read: owner || g.Read, Write: owner || g.Write, SystemAdmin: c.SystemAdmin}, nil
}
func (m *Metadata) Authorize(ctx context.Context, credential, bucket string) (s.Authorization, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.Authorization{}, err
	}
	return m.authorize(credential, bucket)
}
func (m *Metadata) GetChunks(ctx context.Context, keys []s.ChunkKey, consistent bool) (map[s.ChunkKey]s.Chunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := map[s.ChunkKey]s.Chunk{}
	for _, k := range keys {
		if err := k.Validate(); err != nil {
			return nil, err
		}
		if v, ok := m.chunks[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

// Only new canonical destinations require Warm; existing winners remain readable.
func (m *Metadata) validateCanonicalDestination(l s.ChunkLocation) error {
	if err := l.Validate(); err != nil {
		return err
	}
	p, ok := m.packs[l.Pack]
	if !ok {
		return s.ErrNotFound
	}
	if p.State != "Warm" {
		return s.ErrConflict
	}
	return l.Range.Validate(p.Size)
}
func (m *Metadata) PutChunkIfAbsent(ctx context.Context, c s.Chunk, f s.WriterFence) (s.Chunk, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.Chunk{}, false, err
	}
	if err := m.fence(f); err != nil {
		return s.Chunk{}, false, err
	}
	if err := c.Validate(); err != nil {
		return s.Chunk{}, false, err
	}
	if v, ok := m.chunks[c.Key]; ok {
		return v, false, nil
	}
	if c.LocationRevision != 1 {
		return s.Chunk{}, false, s.ErrInvalid
	}
	if err := m.validateCanonicalDestination(c.Location); err != nil {
		return s.Chunk{}, false, err
	}
	m.chunks[c.Key] = c
	return c, true, nil
}
func (m *Metadata) CompareAndSwapChunkLocation(ctx context.Context, key s.ChunkKey, revision uint64, next s.ChunkLocation, f s.WriterFence) (s.Chunk, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.Chunk{}, err
	}
	if err := m.fence(f); err != nil {
		return s.Chunk{}, err
	}
	if err := key.Validate(); err != nil {
		return s.Chunk{}, err
	}
	v, ok := m.chunks[key]
	if !ok {
		return s.Chunk{}, s.ErrNotFound
	}
	if revision != v.LocationRevision || revision == ^uint64(0) {
		return s.Chunk{}, s.ErrConflict
	}
	if err := m.validateCanonicalDestination(next); err != nil {
		return s.Chunk{}, err
	}
	v.Location = next
	v.LocationRevision++
	m.chunks[key] = v
	return v, nil
}
func (m *Metadata) GetPacks(ctx context.Context, refs []s.PackRef) (map[s.PackRef]s.PackInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := map[s.PackRef]s.PackInfo{}
	for _, r := range refs {
		if err := r.Validate(); err != nil {
			return nil, err
		}
		if v, ok := m.packs[r]; ok {
			out[r] = v
		}
	}
	return out, nil
}
func (m *Metadata) WriteManifestPages(ctx context.Context, pages []s.ManifestPage, f s.WriterFence) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.fence(f); err != nil {
		return err
	}
	seen := map[string]map[uint32]s.ManifestPage{}
	for _, p := range pages {
		if err := p.Validate(); err != nil {
			return err
		}
		if _, err := manifest.Encode(manifest.ObjectPage, p); err != nil {
			return err
		}
		if old, ok := m.manifests[p.ManifestID][p.Number]; ok && !reflect.DeepEqual(old, p) {
			return s.ErrConflict
		}
		if _, exists := m.manifests[p.ManifestID][p.Number]; !exists {
			if _, sealed := m.manifestSeals[p.ManifestID]; sealed {
				return s.ErrConflict
			}
		}
		if old, ok := seen[p.ManifestID][p.Number]; ok && !reflect.DeepEqual(old, p) {
			return s.ErrConflict
		}
		if seen[p.ManifestID] == nil {
			seen[p.ManifestID] = map[uint32]s.ManifestPage{}
		}
		seen[p.ManifestID][p.Number] = p
	}
	for _, p := range pages {
		if m.manifests[p.ManifestID] == nil {
			m.manifests[p.ManifestID] = map[uint32]s.ManifestPage{}
		}
		m.manifests[p.ManifestID][p.Number] = copyOf(p)
	}
	return nil
}
func (m *Metadata) GetManifest(ctx context.Context, id string) ([]s.ManifestPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pages, ok := m.manifests[id]
	if !ok {
		if count, sealed := m.manifestSeals[id]; sealed && count == 0 {
			return []s.ManifestPage{}, nil
		}
		return nil, s.ErrNotFound
	}
	out := make([]s.ManifestPage, 0, len(pages))
	for _, p := range pages {
		out = append(out, copyOf(p))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out, nil
}
func (m *Metadata) GetManifestPage(ctx context.Context, id string, number uint32) (s.ManifestPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.ManifestPage{}, err
	}
	page, ok := m.manifests[id][number]
	if !ok {
		return s.ManifestPage{}, s.ErrNotFound
	}
	return copyOf(page), nil
}
func (m *Metadata) GetObject(ctx context.Context, key s.ObjectKey) (s.ObjectHead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.ObjectHead{}, err
	}
	if err := key.Validate(); err != nil {
		return s.ObjectHead{}, err
	}
	b, ok := m.buckets[key.Bucket]
	if !ok || !b.Active || b.ID != key.BucketID {
		return s.ObjectHead{}, s.ErrNotFound
	}
	v, ok := m.heads[key]
	if !ok {
		return s.ObjectHead{}, s.ErrNotFound
	}
	return copyOf(v), nil
}
func (m *Metadata) GetQuota(ctx context.Context, id string) (s.LogicalQuota, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.LogicalQuota{}, err
	}
	v, ok := m.quotas[id]
	if !ok {
		return s.LogicalQuota{}, s.ErrNotFound
	}
	return v, nil
}
func (m *Metadata) manifest(h *s.ObjectHead) error {
	capacity, err := manifest.RefCapacity(h.ManifestID, h.Domain)
	if err != nil {
		return err
	}
	pages, ok := m.manifests[h.ManifestID]
	if !ok {
		if h.Size == 0 && h.PageCount == 0 && h.ChunkCount == 0 && len(h.PageIndex) == 0 {
			return nil
		}
		return s.ErrNotFound
	}
	var offset int64
	var chunks uint64
	var index []uint64
	for n := 0; n < len(pages); n++ {
		p, ok := pages[uint32(n)]
		if !ok || p.Domain != h.Domain || (n < len(pages)-1 && len(p.Chunks) != capacity) {
			return s.ErrInvalid
		}
		index = append(index, uint64(offset))
		for _, r := range p.Chunks {
			chunks++
			c, ok := m.chunks[r.Key]
			if !ok {
				return s.ErrNotFound
			}
			if r.RawBytes != c.RawBytes || r.LogicalOffset != offset || offset > s.MaxObjectBytes-r.RawBytes {
				return s.ErrInvalid
			}
			offset += r.RawBytes
		}
	}
	if offset != h.Size {
		return s.ErrInvalid
	}
	if (h.PageCount != 0 || h.ChunkCount != 0 || len(h.PageIndex) != 0) &&
		(h.PageCount != uint32(len(pages)) || h.ChunkCount != chunks || !reflect.DeepEqual(h.PageIndex, index)) {
		return s.ErrInvalid
	}
	h.PageCount, h.ChunkCount, h.PageIndex = uint32(len(pages)), chunks, index
	return nil
}
func (m *Metadata) CommitObject(ctx context.Context, r s.CommitRequest) (s.CommitResult, error) {
	if r.Head.Deleted {
		return s.CommitResult{}, s.ErrInvalid
	}
	return m.commit(ctx, r)
}
func (m *Metadata) DeleteObject(ctx context.Context, r s.CommitRequest) (s.CommitResult, error) {
	if !r.Head.Deleted {
		return s.CommitResult{}, s.ErrInvalid
	}
	return m.commit(ctx, r)
}
func (m *Metadata) commit(ctx context.Context, r s.CommitRequest) (s.CommitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.CommitResult{}, err
	}
	if err := r.Validate(); err != nil {
		return s.CommitResult{}, err
	}
	if err := m.fence(r.Writer); err != nil {
		return s.CommitResult{}, err
	}
	auth, err := m.authorize(r.CredentialID, r.Head.Object.Bucket)
	if err != nil {
		return s.CommitResult{}, err
	}
	if auth.Bucket.ID != r.Head.Object.BucketID {
		return s.CommitResult{}, s.ErrNotFound
	}
	if auth.UserID != r.UserID || !auth.Write {
		return s.CommitResult{}, s.ErrAccessDenied
	}
	if auth.Bucket.Domain != r.Head.Domain {
		return s.CommitResult{}, s.ErrInvalid
	}
	// Fences may be renewed on retry; the immutable operation input must not change.
	input := r
	input.Writer = s.WriterFence{}
	input.Completion = nil // takeover conditions are not immutable completion input.
	input.ReservationRevision = 0
	data, _ := json.Marshal(input)
	digest := sha256.Sum256(data)
	if done, ok := m.commits[r.Head.OpID]; ok {
		if done.Digest != digest {
			return s.CommitResult{}, s.ErrConflict
		}
		return s.CommitResult{Head: copyOf(done.Head), AlreadyCommitted: true}, nil
	}
	old := m.heads[r.Head.Object]
	if old.Revision != r.ExpectedRevision {
		return s.CommitResult{}, s.ErrConflict
	}
	if _, ok := m.events[r.Event.ID]; ok {
		return s.CommitResult{}, s.ErrConflict
	}
	for _, event := range m.events {
		if event.OpID == r.Event.OpID {
			return s.CommitResult{}, s.ErrConflict
		}
	}
	reservation, ok := m.reservations[r.ReservationID]
	if !ok {
		return s.CommitResult{}, s.ErrNotFound
	}
	if reservation.Object != r.Head.Object || reservation.OwnerID != auth.Bucket.OwnerID || reservation.OpID != r.Head.OpID {
		return s.CommitResult{}, s.ErrInvalid
	}
	if reservation.State != s.ReservationActive || reservation.Revision != r.ReservationRevision || reservation.Revision == ^uint64(0) {
		return s.CommitResult{}, s.ErrConflict
	}
	if reservation.Writer != r.Writer || !m.now().Before(reservation.Deadline) {
		return s.CommitResult{}, s.ErrFenced
	}
	if reservation.StagedBytes != r.Head.Size {
		return s.CommitResult{}, s.ErrInvalid
	}
	q := m.quotas[auth.Bucket.OwnerID]
	used := q.Used - old.Size
	if used < 0 || used > q.Limit || q.Reserved < reservation.Bytes || q.Staged < reservation.StagedBytes {
		return s.CommitResult{}, s.ErrInvalid
	}
	if r.Head.Size > q.Limit-used || q.Reserved-reservation.Bytes > q.Limit-used-r.Head.Size {
		return s.CommitResult{}, s.ErrQuotaExceeded
	}
	h := copyOf(r.Head)
	if !r.Head.Deleted {
		if _, sealed := m.manifestSeals[h.ManifestID]; sealed {
			return s.CommitResult{}, s.ErrConflict
		}
		if err := m.manifest(&h); err != nil {
			return s.CommitResult{}, err
		}
	}
	if r.Receipt != nil {
		current, ok := m.uploads[r.Completion.UploadID]
		if !ok || current != *r.Completion {
			return s.CommitResult{}, s.ErrConflict
		}
		if reservation.UploadID != current.UploadID || q.ActiveUploads < 1 || !r.Receipt.ExpiresAt.After(m.now()) {
			return s.CommitResult{}, s.ErrInvalid
		}
		if _, ok := m.receipts[r.Receipt.UploadID]; ok {
			return s.CommitResult{}, s.ErrConflict
		}
	} else if reservation.UploadID != "" {
		return s.CommitResult{}, s.ErrInvalid
	}
	if m.failBeforeCommit {
		return s.CommitResult{}, fmt.Errorf("%w: injected before atomic publish", s.ErrUnavailable)
	}
	h.Revision = r.ExpectedRevision + 1
	q.Used = used + h.Size
	q.Reserved -= reservation.Bytes
	q.Staged -= reservation.StagedBytes
	if r.Receipt != nil {
		q.ActiveUploads--
		u := *r.Completion
		u.State = "Completed"
		u.Revision++
		m.uploads[u.UploadID] = u
		m.receipts[u.UploadID] = *r.Receipt
	}
	m.quotas[q.OwnerID] = q
	m.heads[h.Object] = h
	if !h.Deleted {
		m.manifestSeals[h.ManifestID] = h.PageCount
	}
	m.events[r.Event.ID] = r.Event
	reservation.State = s.ReservationCommitted
	reservation.Revision++
	m.reservations[r.ReservationID] = reservation
	m.commits[h.OpID] = committed{digest, copyOf(h)}
	if m.failAfterCommit {
		return s.CommitResult{}, fmt.Errorf("%w: injected lost acknowledgement", s.ErrUnavailable)
	}
	return s.CommitResult{Head: copyOf(h)}, nil
}
func (m *Metadata) GetOutbox(ctx context.Context, id string) (s.OutboxEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.OutboxEvent{}, err
	}
	v, ok := m.events[id]
	if !ok {
		return s.OutboxEvent{}, s.ErrNotFound
	}
	return v, nil
}
func (m *Metadata) GetCompletionReceipt(ctx context.Context, id string) (s.CompletedReceipt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.CompletedReceipt{}, err
	}
	v, ok := m.receipts[id]
	if !ok {
		return s.CompletedReceipt{}, s.ErrNotFound
	}
	return v, nil
}
