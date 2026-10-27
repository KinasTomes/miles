package storage

import "context"

// OutboxExportStore extends the shared metadata boundary. Enumeration is bounded
// and ordered by ID. Limit bounds scanned base records before filtering; empty
// pages may have a Next cursor. An open scan is discovery, never closure proof.
// MarkOutboxExported compares the entire immutable source event. Progress is
// separate from the event and never deletes it or releases recovery roots.
type OutboxExportStore interface {
	GetOutbox(context.Context, string) (OutboxEvent, error)
	ListOutbox(context.Context, OutboxQuery) (OutboxPage, error)
	MarkOutboxExported(context.Context, OutboxExport) error
	GetOutboxExport(context.Context, string) (OutboxExport, error)
}

type OutboxQuery struct {
	HistoryID string
	Epoch     uint64
	After     string
	Limit     int
}
type OutboxPage struct {
	Events []OutboxEvent
	Next   string
}
type OutboxExport struct {
	Event                           OutboxEvent
	DescriptorKey, DescriptorSHA256 string
}

func (q OutboxQuery) Validate() error {
	if !hex128(q.HistoryID) || q.Epoch > 1<<53-1 || q.Limit < 1 || q.Limit > 256 || (q.After != "" && !sha256Hex.MatchString(q.After)) {
		return ErrInvalid
	}
	return nil
}
func (p OutboxExport) Validate() error {
	if p.Event.Validate() != nil || p.DescriptorKey == "" || !sha256Hex.MatchString(p.DescriptorSHA256) {
		return ErrInvalid
	}
	return nil
}
