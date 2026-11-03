package jobs

import (
	"context"
	"github.com/KinasTomes/miles/internal/storage"
)

type creationFenceKey struct{}

type adminActorKey struct{}

// AdminActor binds an authenticated HTTP mutation to its admission fence.
// Adapters recheck the token and user inside each job/node write transaction.
type AdminActor struct {
	TokenID string
	Writer  storage.WriterFence
}

func WithAdminActor(ctx context.Context, actor AdminActor) context.Context {
	return context.WithValue(ctx, adminActorKey{}, actor)
}

func AdminActorFromContext(ctx context.Context) (AdminActor, bool) {
	a, ok := ctx.Value(adminActorKey{}).(AdminActor)
	return a, ok
}

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
