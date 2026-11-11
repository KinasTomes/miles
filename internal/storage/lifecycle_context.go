package storage

import "context"

type lifecycleActionContextKey struct{}

// WithLifecycleAction binds an external lifecycle request to its durable
// write-ahead action. Physical providers may use the ID as an idempotency
// marker when the underlying service supports one.
func WithLifecycleAction(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, lifecycleActionContextKey{}, id)
}

func LifecycleActionFromContext(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(lifecycleActionContextKey{}).(string)
	return id, ok && id != ""
}
