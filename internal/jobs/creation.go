package jobs

import (
	"context"
	"github.com/KinasTomes/miles/internal/storage"
)

type creationFenceKey struct{}

// WithCreationFence binds trusted scheduler/parent authority to enqueueing.
// This is separate from a namespace producer context: Jobs are not replayed.
// HTTP clients never supply a creator fence; adapters check it in the write CAS.
func WithCreationFence(ctx context.Context, f storage.WriterFence) context.Context {
	return context.WithValue(ctx, creationFenceKey{}, f)
}
func CreationFence(ctx context.Context) (storage.WriterFence, bool) {
	f, ok := ctx.Value(creationFenceKey{}).(storage.WriterFence)
	return f, ok
}
