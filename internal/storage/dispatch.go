package storage

import "context"

type physicalGuardKey struct{}

// WithPhysicalDispatch binds trusted permission checks to the final SDK send,
// after adapter reads/buffering. It is not populated by request headers.
func WithPhysicalDispatch(ctx context.Context, guard func(context.Context) error) context.Context {
	return context.WithValue(ctx, physicalGuardKey{}, guard)
}
func CheckPhysicalDispatch(ctx context.Context) error {
	if e := ctx.Err(); e != nil {
		return e
	}
	if guard, ok := ctx.Value(physicalGuardKey{}).(func(context.Context) error); ok {
		return guard(ctx)
	}
	return nil
}

// RequirePhysicalDispatch is for maintenance operations that must never run
// without an explicit dispatch authority.
func RequirePhysicalDispatch(ctx context.Context) error {
	if guard, ok := ctx.Value(physicalGuardKey{}).(func(context.Context) error); !ok || guard == nil {
		return ErrFenced
	}
	return CheckPhysicalDispatch(ctx)
}
