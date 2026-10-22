package clock

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

// Sequence exhaustion is unreachable in a practical trace, but wrapping it
// would violate the documented total order. Verify failure before insertion.
func TestVirtualSequenceOverflow(t *testing.T) {
	v, err := NewVirtual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatal(err)
	}
	v.sequence = math.MaxUint64 - 1
	fired := 0
	if _, err := v.AfterFunc(0, func() { fired++ }); err != nil {
		t.Fatal(err)
	}
	if _, err := v.AfterFunc(0, func() { fired += 100 }); !errors.Is(err, ErrOverflow) {
		t.Fatal(err)
	}
	if v.Pending() != 1 {
		t.Fatal(v.Pending())
	}
	if err := v.Advance(0); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatal(fired)
	}
}

func TestPendingCancelledSleepersDoesNotMutateHeap(t *testing.T) {
	v, err := NewVirtual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 20)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dispatched []int
	deadlines := []int{0, 10, 1, 11, 12, 2, 3}
	// Register sleeper-shaped events directly so no host goroutine can remove
	// cancelled nodes before Pending observes this exact regression heap.
	for _, due := range deadlines {
		var eventContext context.Context
		if due == 10 || due == 12 {
			eventContext = ctx
		}
		if _, err := v.schedule(time.Duration(due), func() { dispatched = append(dispatched, due) }, eventContext); err != nil {
			t.Fatal(err)
		}
	}
	beforeNodes := append([]*event(nil), v.events...)
	for i, e := range beforeNodes {
		if e.due != int64(deadlines[i]) {
			t.Fatal("regression heap differs", i, e.due)
		}
	}
	before := v.Now()
	beforeSequence := v.sequence
	cancel()
	for repeat := 0; repeat < 10; repeat++ {
		if got := v.Pending(); got != 5 {
			t.Fatalf("Pending=%d, want 5", got)
		}
		v.mu.Lock()
		unchanged := len(v.events) == len(beforeNodes) && v.sequence == beforeSequence
		for i, node := range v.events {
			if i >= len(beforeNodes) || node != beforeNodes[i] || node.index != i || node.callback == nil {
				unchanged = false
			}
		}
		v.mu.Unlock()
		if !unchanged {
			t.Fatal("Pending changed scheduler state")
		}
		if cmp, err := v.Now().Instant.Compare(before.Instant); err != nil || cmp != 0 {
			t.Fatal(cmp, err)
		}
		if !v.Now().WallUTC.Equal(before.WallUTC) {
			t.Fatal("Pending advanced wall time")
		}
	}
	if err := v.Advance(12); err != nil {
		t.Fatal(err)
	}
	if want := []int{0, 1, 2, 3, 11}; !reflect.DeepEqual(dispatched, want) {
		t.Fatal(dispatched)
	}
	if v.Pending() != 0 || len(v.events) != 0 {
		t.Fatal("Advance retained cancelled sleepers")
	}
}

// sleepProbeContext observes registration through Done and can delay the final
// Err observation after a wakeup. This is test-only instrumentation: it never
// changes production Clock hooks or presumes when the host runs the goroutine.
type sleepProbeContext struct {
	context.Context
	registered     chan struct{}
	registeredOnce sync.Once
	holdErr        <-chan struct{}
	releaseErr     <-chan struct{}
	errEntered     chan struct{}
	errOnce        sync.Once
}

func (c *sleepProbeContext) Done() <-chan struct{} {
	c.registeredOnce.Do(func() { close(c.registered) })
	return c.Context.Done()
}

func (c *sleepProbeContext) Err() error {
	select {
	case <-c.holdErr:
		c.errOnce.Do(func() { close(c.errEntered) })
		<-c.releaseErr
	default:
	}
	return c.Context.Err()
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("test synchronization stalled")
	}
}

func TestSleepSameDeadlineOrderingWithDelayedObserver(t *testing.T) {
	for _, wakeFirst := range []bool{false, true} {
		name := "cancel-before-wake"
		if wakeFirst {
			name = "wake-before-cancel"
		}
		t.Run(name, func(t *testing.T) {
			v, err := NewVirtual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), 10)
			if err != nil {
				t.Fatal(err)
			}
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			hold := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			resume := func() { releaseOnce.Do(func() { close(release) }) }
			defer resume()
			ctx := &sleepProbeContext{Context: base, registered: make(chan struct{}), holdErr: hold, releaseErr: release, errEntered: make(chan struct{})}
			var log []string
			registerCancel := func() {
				if _, err := v.AfterFunc(time.Second, func() { log = append(log, "cancel"); cancel() }); err != nil {
					t.Fatal(err)
				}
			}
			if !wakeFirst {
				registerCancel()
			}
			result := make(chan error, 1)
			go func() { result <- v.Sleep(ctx, time.Second) }()
			waitSignal(t, ctx.registered)
			// Registration is complete. Wrap the internal wakeup only to establish
			// a barrier before the goroutine's return-time cancellation observation.
			v.mu.Lock()
			found := false
			for _, node := range v.events {
				if node.ctx == ctx {
					found = true
					original := node.callback
					node.callback = func() {
						log = append(log, "wake")
						if wakeFirst {
							close(hold)
						}
						original()
					}
				}
			}
			v.mu.Unlock()
			if !found {
				t.Fatal("Sleep timer missing after registration")
			}
			if wakeFirst {
				registerCancel()
			}
			if _, err := v.AfterFunc(2*time.Second, func() { log = append(log, "continued") }); err != nil {
				t.Fatal(err)
			}
			if err := v.Advance(2 * time.Second); err != nil {
				t.Fatal(err)
			}
			want := []string{"cancel", "continued"}
			if wakeFirst {
				want = []string{"wake", "cancel", "continued"}
				waitSignal(t, ctx.errEntered)
				select {
				case <-result:
					t.Fatal("Sleep returned before observer release")
				default:
				}
			}
			if !reflect.DeepEqual(log, want) {
				t.Fatal(log, want)
			}
			resume()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("lost Sleep wakeup/cancellation")
			}
			if v.Pending() != 0 || len(v.events) != 0 {
				t.Fatal("Sleep timer leak")
			}
		})
	}
}
