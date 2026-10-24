package fake

import (
	"context"

	s "github.com/KinasTomes/miles/internal/storage"
)

func (m *Metadata) FailReconcile(fail bool) { m.mu.Lock(); defer m.mu.Unlock(); m.failReconcile = fail }
func (m *Metadata) GetReservation(ctx context.Context, id string) (s.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.Reservation{}, err
	}
	r, ok := m.reservations[id]
	if !ok {
		return s.Reservation{}, s.ErrNotFound
	}
	return r, nil
}

func (m *Metadata) ReserveLogicalQuota(ctx context.Context, r s.Reservation) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.fence(r.Writer); err != nil {
		return err
	}
	if err := r.Object.Validate(); err != nil {
		return err
	}
	if r.Version != s.ModelVersion || r.ID == "" || r.OpID == "" || r.Revision != 1 || r.State != s.ReservationActive || r.BaseSize < 0 || r.BaseSize > s.MaxObjectBytes || !m.now().Before(r.Deadline) || r.Bytes < 0 || r.Bytes > s.MaxObjectBytes || r.StagedBytes < 0 || r.StagedBytes > s.MaxObjectBytes {
		return s.ErrInvalid
	}
	if r.StagedBytes > r.BaseSize && r.Bytes < r.StagedBytes-r.BaseSize {
		return s.ErrInvalid
	}
	if v, ok := m.reservations[r.ID]; ok {
		if v == r {
			return nil
		}
		return s.ErrConflict
	}
	for _, old := range m.reservations {
		if old.OpID == r.OpID {
			return s.ErrConflict
		}
	}
	b, ok := m.buckets[r.Object.Bucket]
	if !ok || !b.Active || b.ID != r.Object.BucketID {
		return s.ErrNotFound
	}
	if b.OwnerID != r.OwnerID {
		return s.ErrInvalid
	}
	base := m.heads[r.Object]
	if base.Revision != r.BaseRevision || base.Size != r.BaseSize {
		return s.ErrConflict
	}
	q, ok := m.quotas[r.OwnerID]
	if !ok {
		return s.ErrNotFound
	}
	if q.Limit < 0 || q.Used < 0 || q.Reserved < 0 || q.Staged < 0 || q.Used > q.Limit || q.Reserved > q.Limit-q.Used {
		return s.ErrInvalid
	}
	if r.Bytes > q.Limit-q.Used-q.Reserved {
		return s.ErrQuotaExceeded
	}
	if q.Staged >= q.Limit && (q.Staged-q.Limit > s.MaxObjectBytes || r.StagedBytes > s.MaxObjectBytes-(q.Staged-q.Limit)) {
		return s.ErrQuotaExceeded
	}
	if q.Staged > int64(^uint64(0)>>1)-r.StagedBytes {
		return s.ErrQuotaExceeded
	}
	q.Reserved += r.Bytes
	q.Staged += r.StagedBytes
	m.quotas[r.OwnerID] = q
	m.reservations[r.ID] = r
	return nil
}

func (m *Metadata) AdjustLogicalQuota(ctx context.Context, a s.ReservationAdjustment) (s.Reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.Reservation{}, err
	}
	if err := m.fence(a.Writer); err != nil {
		return s.Reservation{}, err
	}
	if a.ID == "" || a.OpID == "" || a.ExpectedRevision == 0 {
		return s.Reservation{}, s.ErrInvalid
	}
	r, ok := m.reservations[a.ID]
	if !ok {
		return s.Reservation{}, s.ErrNotFound
	}
	if r.OpID != a.OpID || r.State != s.ReservationActive {
		return s.Reservation{}, s.ErrConflict
	}
	if r.Writer != a.Writer || !m.now().Before(r.Deadline) {
		return s.Reservation{}, s.ErrFenced
	}
	if r.Revision != a.ExpectedRevision {
		if a.ExpectedRevision != ^uint64(0) && r.Revision == a.ExpectedRevision+1 && r.Bytes == a.Bytes && r.StagedBytes == a.StagedBytes {
			return r, nil
		}
		return s.Reservation{}, s.ErrConflict
	}
	if r.Revision == ^uint64(0) {
		return s.Reservation{}, s.ErrConflict
	}
	q, ok := m.quotas[r.OwnerID]
	if !ok {
		return s.Reservation{}, s.ErrNotFound
	}
	q, err := s.AdjustReservationQuota(q, r, a.Bytes, a.StagedBytes)
	if err != nil {
		return s.Reservation{}, err
	}
	r.Bytes, r.StagedBytes, r.Revision = a.Bytes, a.StagedBytes, r.Revision+1
	m.quotas[r.OwnerID] = q
	m.reservations[r.ID] = r
	return r, nil
}

func (m *Metadata) GetCommitOutcome(ctx context.Context, opID string) (s.CommitResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.CommitResult{}, err
	}
	if opID == "" {
		return s.CommitResult{}, s.ErrInvalid
	}
	if m.failReconcile {
		return s.CommitResult{}, s.ErrUnavailable
	}
	if done, ok := m.commits[opID]; ok {
		return s.CommitResult{Head: copyOf(done.Head), AlreadyCommitted: true}, nil
	}
	for _, event := range m.events {
		if event.OpID == opID {
			return s.CommitResult{}, s.ErrUnavailable
		}
	}
	return s.CommitResult{}, s.ErrNotFound
}

// outcome is an authoritative read within the same transaction as claim/release.
// A failed/missing outcome proof never implies that quota can be released.
func (m *Metadata) outcome(r s.Reservation) (*s.CommitResult, error) {
	if m.failReconcile {
		return nil, s.ErrUnavailable
	}
	if done, ok := m.commits[r.OpID]; ok {
		if r.State != s.ReservationCommitted {
			return nil, s.ErrUnavailable
		}
		return &s.CommitResult{Head: copyOf(done.Head), AlreadyCommitted: true}, nil
	}
	if r.State == s.ReservationCommitted {
		return nil, s.ErrUnavailable
	}
	for _, event := range m.events {
		if event.OpID == r.OpID {
			return nil, s.ErrUnavailable
		}
	}
	return nil, nil
}
func (m *Metadata) ClaimReservation(ctx context.Context, request s.ReservationClaim) (s.ReservationResolution, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.ReservationResolution{}, err
	}
	if err := m.fence(request.Writer); err != nil {
		return s.ReservationResolution{}, err
	}
	if request.ID == "" || request.OpID == "" || request.ExpectedRevision == 0 {
		return s.ReservationResolution{}, s.ErrInvalid
	}
	r, ok := m.reservations[request.ID]
	if !ok {
		return s.ReservationResolution{}, s.ErrNotFound
	}
	if r.OpID != request.OpID {
		return s.ReservationResolution{}, s.ErrConflict
	}
	done, err := m.outcome(r)
	if err != nil {
		return s.ReservationResolution{}, err
	}
	if done != nil {
		return s.ReservationResolution{Reservation: r, Committed: done}, nil
	}
	if r.State != s.ReservationActive || r.Revision != request.ExpectedRevision || r.Revision == ^uint64(0) {
		return s.ReservationResolution{}, s.ErrConflict
	}
	if request.Writer.ServiceIncarnation != r.Writer.ServiceIncarnation {
		return s.ReservationResolution{}, s.ErrFenced
	}
	if r.Writer == request.Writer {
		return s.ReservationResolution{Reservation: r}, nil
	}
	if m.fence(r.Writer) == nil {
		return s.ReservationResolution{}, s.ErrConflict
	}
	r.Writer = request.Writer
	r.Revision++
	m.reservations[r.ID] = r
	return s.ReservationResolution{Reservation: r}, nil
}
func (m *Metadata) ReleaseLogicalQuota(ctx context.Context, request s.ReservationRelease) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.fence(request.Writer); err != nil {
		return err
	}
	if request.ID == "" || request.OpID == "" || request.ExpectedRevision == 0 {
		return s.ErrInvalid
	}
	r, ok := m.reservations[request.ID]
	if !ok {
		return s.ErrNotFound
	}
	if r.OpID != request.OpID {
		return s.ErrConflict
	}
	if r.Writer != request.Writer {
		return s.ErrFenced
	}
	done, err := m.outcome(r)
	if err != nil {
		return err
	}
	if done != nil {
		return s.ErrConflict
	}
	if r.State == s.ReservationReleased {
		return nil
	}
	if r.State != s.ReservationActive || r.Revision != request.ExpectedRevision || r.Revision == ^uint64(0) {
		return s.ErrConflict
	}
	q := m.quotas[r.OwnerID]
	if q.Reserved < r.Bytes || q.Staged < r.StagedBytes {
		return s.ErrInvalid
	}
	q.Reserved -= r.Bytes
	q.Staged -= r.StagedBytes
	m.quotas[r.OwnerID] = q
	r.State = s.ReservationReleased
	r.Revision++
	m.reservations[r.ID] = r
	return nil
}
