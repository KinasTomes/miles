package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/smithy-go"
)

// One supervisor per gateway; immutable writes and shared CAS arbitrate peers.
// A new hard lease never resets the durable hourly epoch deadline.
type journalRuntime struct {
	store     *dynamodb.Store
	objects   *recovery.S3Objects
	catalog   recovery.Catalog
	clock     clock.Clock
	authority jobs.Authority
}

type journalAttemptKey struct{}

func journalFailure(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, storage.ErrR1Busy):
		return "r1_gate_busy"
	case errors.Is(err, storage.ErrFenced):
		return "fenced"
	case errors.Is(err, storage.ErrClockUncertain):
		return "clock_uncertainty_exceeded"
	case errors.Is(err, storage.ErrUnavailable):
		var service smithy.APIError
		if errors.As(err, &service) {
			switch service.ErrorCode() {
			case "ThrottlingException", "ProvisionedThroughputExceededException", "RequestLimitExceeded":
				return "backend_throttled"
			case "InternalServerError":
				return "backend_internal_error"
			case "TransactionConflictException":
				return "backend_transaction_conflict"
			}
		}
		return "backend_unavailable"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, storage.ErrConflict):
		return "metadata_conflict"
	case errors.Is(err, storage.ErrInvalid):
		return "invalid_state"
	default:
		return "unknown"
	}
}

func (j *journalRuntime) once(ctx context.Context, flush bool) (workErr, cleanupErr error) {
	started := time.Now()
	stage := "acquire_authority"
	attempt, _ := ctx.Value(journalAttemptKey{}).(int)
	defer func() {
		if !flush {
			return
		}
		remaining := int64(0)
		if deadline, ok := ctx.Deadline(); ok {
			remaining = max(0, time.Until(deadline).Milliseconds())
		}
		slog.InfoContext(ctx, "shutdown journal attempt", "attempt", attempt,
			"stage", stage, "work_failure", journalFailure(workErr), "cleanup_failure", journalFailure(cleanupErr),
			"elapsed_ms", time.Since(started).Milliseconds(), "remaining_ms", remaining)
	}()
	observe := func(next string) {
		if flush {
			if next != "close_authority" {
				stage = next
			}
			slog.InfoContext(ctx, "shutdown journal progress", "stage", next, "attempt", attempt)
		}
	}
	id := "recovery_supervisor"
	if flush {
		suffix, e := storage.NewBucketID()
		if e != nil {
			return e, nil
		}
		id = "shutdown_" + suffix
	}
	observe("acquire_authority")
	f, err := j.authority.Acquire(ctx, id, "recovery")
	if err != nil {
		return fmt.Errorf("acquire recovery authority: %w", err), nil
	}
	defer func() {
		observe("close_authority")
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cleanupErr = j.authority.Close(cleanup, f)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("release recovery authority: %w", cleanupErr)
		}
	}()
	w := recovery.Worker{Store: j.store, Payloads: j.store, Objects: j.objects, Catalog: j.catalog, Clock: j.clock, Fence: f, Observe: observe}
	if flush {
		return w.FlushSupervised(ctx), nil
	}
	return w.RunCuts(ctx), nil
}

func (j *journalRuntime) run(ctx context.Context) error {
	for ctx.Err() == nil {
		_, _ = j.once(ctx, false)
		if err := j.clock.Sleep(ctx, time.Second); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (j *journalRuntime) flush(parent context.Context) error {
	ctx, cancel := context.WithTimeout(parent, 8*time.Minute)
	defer cancel()
	for attempt := 1; ; attempt++ {
		workErr, cleanupErr := j.once(context.WithValue(ctx, journalAttemptKey{}, attempt), true)
		retryable := errors.Is(workErr, storage.ErrR1Busy) || errors.Is(workErr, storage.ErrUnavailable)
		if cleanupErr != nil || !retryable {
			return errors.Join(workErr, cleanupErr)
		}
		// Peer ownership and transient backend failures may clear. Retry only
		// after successful lease cleanup, within the original flush deadline.
		if err := j.clock.Sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}
