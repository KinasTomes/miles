package app

import (
	"context"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/storage/s3"
)

type lifecycleTerminalBackend interface {
	VerifyLifecycleTerminal(context.Context, lifecycle.Action) (bool, error)
}

// runtimeLifecycleTerminal joins the physical S3 proof to the same consistent
// DynamoDB observation used by the caller's publication transaction.
type runtimeLifecycleTerminal struct {
	backend lifecycleTerminalBackend
}

func (g runtimeLifecycleTerminal) Terminal(ctx context.Context, action lifecycle.Action, observe dynamodb.Observe) (bool, error) {
	if g.backend == nil {
		return false, jobs.ErrReconciliation
	}
	if err := dynamodb.VerifyLifecycleIsolation(action, observe); err != nil {
		return false, err
	}
	return g.backend.VerifyLifecycleTerminal(ctx, action)
}

var _ dynamodb.LifecycleTerminalGuard = runtimeLifecycleTerminal{}
var _ lifecycleTerminalBackend = (*s3.Store)(nil)
