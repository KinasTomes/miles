package object

import (
	"context"
	"github.com/KinasTomes/miles/internal/storage"
	"strings"
)

// MetadataEvents is the production envelope builder for strict producer stores.
// Complete effects and immutable dependencies are prepared by the metadata
// transaction, not fabricated in HTTP handlers or queued after publication.
type MetadataEvents struct{}

func (MetadataEvents) ObjectCommit(ctx context.Context, r CommitEvent) (storage.OutboxEvent, error) {
	c, ok := storage.MutationFromContext(ctx)
	if !ok || c.Validate() != nil || c.Object != r.Object || c.MutationID != r.OpID || c.LogicalOpID != r.OpID {
		return storage.OutboxEvent{}, storage.ErrFenced
	}
	id, err := storage.EventID(c.HistoryID, c.MutationID, "Objects", "object_commit")
	if err != nil {
		return storage.OutboxEvent{}, err
	}
	return storage.OutboxEvent{Version: 1, ID: id, HistoryID: c.HistoryID, Epoch: c.Epoch, Entity: "Objects", Action: "object_commit", Object: r.Object, Revision: r.Revision, OpID: r.OpID, PayloadPointer: "backend/prepared", PayloadSHA256: strings.Repeat("0", 64)}, nil
}

var _ EventBuilder = MetadataEvents{}
