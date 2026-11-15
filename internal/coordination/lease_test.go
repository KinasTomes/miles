package coordination

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestKeepAliveRetainsFirstRenewalCauseAndJoins(t *testing.T) {
	for _, failure := range []error{storage.ErrClockUncertain, storage.ErrFenced, storage.ErrUnavailable} {
		t.Run(failure.Error(), func(t *testing.T) {
			c, err := clock.NewVirtual(time.Now(), 100)
			if err != nil {
				t.Fatal(err)
			}
			timer := signaledClock{Virtual: c, registered: make(chan struct{}, 1)}
			parent, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			live, stop, err := keepAlive(parent, timer, time.Second, func(context.Context) error { return failure })
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
			<-timer.registered
			if err := c.Advance(time.Second); err != nil {
				t.Fatal(err)
			}
			select {
			case <-live.Done():
			case <-time.After(time.Second):
				t.Fatal("renewal failure did not cancel work")
			}
			cancel(errors.New("later parent cancellation"))
			stop()
			if !errors.Is(context.Cause(live), failure) || live.Err() != context.Canceled {
				t.Fatal("lost first renewal failure", context.Cause(live))
			}
		})
	}
}

func TestKeepAliveStopAndParentCancellation(t *testing.T) {
	for _, parentFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(parentFirst), func(t *testing.T) {
			parent, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			live, stop, err := keepAlive(parent, clock.NewReal(), time.Hour, func(context.Context) error {
				t.Error("unexpected renewal")
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			want := error(context.Canceled)
			if parentFirst {
				want = context.DeadlineExceeded
				cancel(want)
			}
			stop()
			stop()
			if !errors.Is(context.Cause(live), want) {
				t.Fatal("changed stop/parent cause", context.Cause(live))
			}
		})
	}
}
