package fake

import (
	"context"
	"errors"
)

type Point string

const (
	Registration Point = "registration"
	S3Write      Point = "s3_write"
	Publication  Point = "commit"
	EpochClose   Point = "close_epoch"
	SealPublish  Point = "publish_seal"
	PinAttach    Point = "pin_attach"
	ReadRelease  Point = "read_release"
)

type Phase string

const (
	Before Phase = "before"
	After  Phase = "after"
)

var ErrUnknown = errors.New("fault harness: acknowledgement lost; reconcile outcome")

// Faults is an explicit test driver, never installed globally. Hook must be
// concurrency-safe if used by concurrent drivers. A channel barrier can pause
// before/after an operation LP without holding the coordinator's mutex.
// After-error preserves the effect and hides its acknowledgement. The caller
// MUST reconcile before using a handle or dropping protection. This wrapper
// does not implement durable S3, transactions, recovery serialization or pins.
type Faults struct {
	Hook func(context.Context, Point, Phase) error
}

func (f Faults) Run(ctx context.Context, point Point, effect func() error) error {
	if ctx == nil || effect == nil {
		return ErrInvalid
	}
	switch point {
	case Registration, S3Write, Publication, EpochClose, SealPublish, PinAttach, ReadRelease:
	default:
		return ErrInvalid
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if f.Hook != nil {
		if e := f.Hook(ctx, point, Before); e != nil {
			return e
		}
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if e := effect(); e != nil {
		return e
	}
	if f.Hook != nil {
		if e := f.Hook(ctx, point, After); e != nil {
			return errors.Join(ErrUnknown, e)
		}
	}
	if e := ctx.Err(); e != nil {
		return errors.Join(ErrUnknown, e)
	}
	return nil
}
