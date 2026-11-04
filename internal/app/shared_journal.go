package app

import (
	"context"
	"errors"
	"fmt"
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

func (j *journalRuntime) once(ctx context.Context, flush bool) (err error) {
	id := "recovery_supervisor"
	if flush {
		suffix, e := storage.NewBucketID()
		if e != nil {
			return e
		}
		id = "shutdown_" + suffix
	}
	f, err := j.authority.Acquire(ctx, id, "recovery")
	if err != nil {
		return fmt.Errorf("acquire recovery authority: %w", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		e := j.authority.Close(cleanup, f)
		if e != nil {
			e = fmt.Errorf("release recovery authority: %w", e)
		}
		err = errors.Join(err, e)
	}()
	w := recovery.Worker{Store: j.store, Payloads: j.store, Objects: j.objects, Catalog: j.catalog, Clock: j.clock, Fence: f}
	if flush {
		return w.FlushSupervised(ctx)
	}
	return w.RunCuts(ctx)
}

func (j *journalRuntime) run(ctx context.Context) error {
	for ctx.Err() == nil {
		_ = j.once(ctx, false)
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
		err := j.once(ctx, true)
		if !errors.Is(err, storage.ErrR1Busy) {
			return err
		}
		// A peer may still own a closed epoch's seal. Keep its authority intact
		// and retry discovery within this shutdown's original flush deadline.
		if err := j.clock.Sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}
