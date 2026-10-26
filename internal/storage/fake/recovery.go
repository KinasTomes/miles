package fake

import (
	"context"
	s "github.com/KinasTomes/miles/internal/storage"
	"reflect"
	"sort"
)

var _ s.OutboxExportStore = (*Metadata)(nil)

func (m *Metadata) ListOutbox(ctx context.Context, q s.OutboxQuery) (s.OutboxPage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out s.OutboxPage
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if err := q.Validate(); err != nil {
		return out, err
	}
	var ids []string
	for id := range m.events {
		if id > q.After {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for i, id := range ids {
		if i == q.Limit {
			out.Next = ids[i-1]
			break
		}
		e := m.events[id]
		if e.HistoryID == q.HistoryID && e.Epoch == q.Epoch {
			out.Events = append(out.Events, copyOf(e))
		}
	}
	return out, nil
}
func (m *Metadata) MarkOutboxExported(ctx context.Context, p s.OutboxExport) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	e, ok := m.events[p.Event.ID]
	if !ok {
		return s.ErrNotFound
	}
	if !reflect.DeepEqual(e, p.Event) {
		return s.ErrConflict
	}
	if old, ok := m.exports[e.ID]; ok {
		if reflect.DeepEqual(old, p) {
			return nil
		}
		return s.ErrConflict
	}
	if m.exports == nil {
		m.exports = make(map[string]s.OutboxExport)
	}
	m.exports[e.ID] = copyOf(p)
	return nil
}
func (m *Metadata) GetOutboxExport(ctx context.Context, id string) (s.OutboxExport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return s.OutboxExport{}, err
	}
	p, ok := m.exports[id]
	if !ok {
		return s.OutboxExport{}, s.ErrNotFound
	}
	return copyOf(p), nil
}
