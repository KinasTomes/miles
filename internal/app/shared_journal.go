package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/storage"
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

func (j *journalRuntime) once(ctx context.Context, flush bool) (workErr, cleanupErr error) {
	id := "recovery_supervisor"
	if flush {
		suffix, e := storage.NewBucketID()
		if e != nil {
			return e, nil
		}
		id = "shutdown_" + suffix
	}
	f, err := j.authority.Acquire(ctx, id, "recovery")
	if err != nil {
		return fmt.Errorf("acquire recovery authority: %w", err), nil
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cleanupErr = j.authority.Close(cleanup, f)
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("release recovery authority: %w", cleanupErr)
		}
	}()
	w := recovery.Worker{Store: j.store, Payloads: j.store, Objects: j.objects, Catalog: j.catalog, Clock: j.clock, Fence: f}
	if flush {
		return w.FlushSupervised(ctx), nil
	}
	return w.RunCuts(ctx), nil
}

func (j *journalRuntime) run(ctx context.Context) error {
	if os.Getenv("MILES_DISABLE_PERIODIC") == "1" {
		<-ctx.Done()
		return ctx.Err()
	}
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
	for {
		workErr, cleanupErr := j.once(ctx, true)
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
