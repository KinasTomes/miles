package observability

import "context"

type requestIDKey struct{}

// WithRequestID carries a server-generated ID into backend diagnostics. It must
// not be populated from signed URLs, credentials or client-controlled headers.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}
