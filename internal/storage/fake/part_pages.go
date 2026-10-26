package fake

import (
	"context"
	"reflect"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
)

var _ s.PartPageStore = (*Metadata)(nil)

func (m *Metadata) WritePartPages(ctx context.Context, pages []s.ManifestPage, writer s.WriterFence) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.fence(writer); err != nil {
		return err
	}
	for _, page := range pages {
		if _, err := manifest.Encode(manifest.PartPage, page); err != nil {
			return err
		}
		if old, ok := m.partPages[page.ManifestID][page.Number]; ok && !reflect.DeepEqual(old, page) {
			return s.ErrConflict
		}
		if m.partPages == nil {
			m.partPages = map[string]map[uint32]s.ManifestPage{}
		}
		if m.partPages[page.ManifestID] == nil {
			m.partPages[page.ManifestID] = map[uint32]s.ManifestPage{}
		}
		m.partPages[page.ManifestID][page.Number] = copyOf(page)
	}
	return nil
}
func (m *Metadata) GetPartPage(ctx context.Context, owner string, number uint32) (s.ManifestPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.ManifestPage{}, err
	}
	p, ok := m.partPages[owner][number]
	if !ok {
		return s.ManifestPage{}, s.ErrNotFound
	}
	return copyOf(p), nil
}
