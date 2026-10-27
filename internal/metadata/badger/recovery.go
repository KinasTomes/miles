package badger

import (
	"context"
	"errors"
	"reflect"

	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

var _ s.OutboxExportStore = (*Store)(nil)

func (m *Store) ListOutbox(ctx context.Context, q s.OutboxQuery) (s.OutboxPage, error) {
	var out s.OutboxPage
	if err := q.Validate(); err != nil {
		return out, err
	}
	err := m.view(ctx, func(t *db.Txn) error {
		it := t.NewIterator(db.DefaultIteratorOptions)
		defer it.Close()
		prefix := []byte("events/")
		start := prefix
		if q.After != "" {
			start = []byte(key("events", q.After))
		}
		scanned, last := 0, ""
		for it.Seek(start); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			k := string(it.Item().KeyCopy(nil))
			if q.After != "" && k <= key("events", q.After) {
				continue
			}
			if scanned == q.Limit {
				out.Next = last
				break
			}
			e, err := get[s.OutboxEvent](t, k)
			if err != nil {
				return err
			}
			if e.Validate() != nil || key("events", e.ID) != k {
				return s.ErrInvalid
			}
			scanned++
			last = e.ID
			if e.HistoryID != q.HistoryID || e.Epoch != q.Epoch {
				continue
			}
			out.Events = append(out.Events, e)
		}
		return nil
	})
	return out, err
}

func (m *Store) MarkOutboxExported(ctx context.Context, p s.OutboxExport) error {
	if err := p.Validate(); err != nil {
		return err
	}
	return m.update(ctx, func(t *db.Txn) error {
		e, err := get[s.OutboxEvent](t, key("events", p.Event.ID))
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(e, p.Event) {
			return s.ErrConflict
		}
		old, err := get[s.OutboxExport](t, key("exports", e.ID))
		if err == nil {
			if reflect.DeepEqual(old, p) {
				return nil
			}
			return s.ErrConflict
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		return put(t, key("exports", e.ID), p)
	})
}
func (m *Store) GetOutboxExport(ctx context.Context, id string) (s.OutboxExport, error) {
	return read[s.OutboxExport](m, ctx, key("exports", id))
}
