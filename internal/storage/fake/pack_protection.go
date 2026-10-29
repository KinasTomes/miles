package fake

import (
	"context"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	s "github.com/KinasTomes/miles/internal/storage"
)

// The reference fake models the atomic max/binding predicates only. It is not
// producer persistence or distributed R1/R2 evidence; native suites cover that.
func (m *Metadata) ExtendPackProtection(ctx context.Context, r s.PackProtectionUpdate) (s.PackInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.PackInfo{}, err
	}
	if err := m.fence(r.Writer); err != nil {
		return s.PackInfo{}, err
	}
	row, ok := m.chunks[r.Chunk.Key]
	if !ok {
		return s.PackInfo{}, s.ErrNotFound
	}
	p, ok := m.packs[r.Chunk.Location.Pack]
	if !ok {
		return s.PackInfo{}, s.ErrNotFound
	}
	out, err := operations.NextProtectedPack(row, p, r, m.now())
	if err != nil {
		return s.PackInfo{}, err
	}
	if m.failBeforeCommit {
		return s.PackInfo{}, s.ErrUnavailable
	}
	m.packs[out.Ref] = out
	if m.failAfterCommit {
		return s.PackInfo{}, s.ErrUnavailable
	}
	return out, nil
}
