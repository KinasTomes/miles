package clock_test

import (
	"context"
	"errors"
	"math"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	c "github.com/KinasTomes/miles/internal/clock"
)

func schedule(t *testing.T, v *c.Virtual, d time.Duration, fn func()) *c.Event {
	t.Helper()
	e, err := v.AfterFunc(d, fn)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The host timer is only a deadlock watchdog, never virtual time or event order.
func waitPending(t *testing.T, v *c.Virtual, n int) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for v.Pending() != n {
		select {
		case <-timer.C:
			t.Fatalf("pending=%d, want %d", v.Pending(), n)
		default:
			runtime.Gosched()
		}
	}
}

func await(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("sleep did not return")
		return nil
	}
}

func TestVirtualWallJumps(t *testing.T) {
	v := virtual(t, 20)
	before := v.Now()
	fired := 0
	schedule(t, v, 2*time.Second, func() { fired++ })
	for _, jump := range []time.Duration{time.Hour, -2 * time.Hour} {
		if err := v.SetWall(v.Now().WallUTC.Add(jump)); err != nil {
			t.Fatal(err)
		}
		if delta, err := v.Now().Instant.Sub(before.Instant); err != nil || delta != 0 {
			t.Fatal(delta, err)
		}
	}
	if err := v.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatal("wall jump fired elapsed timer")
	}
	if err := v.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	if fired != 1 {
		t.Fatal(fired)
	}
	if want := before.WallUTC.Add(-time.Hour + 2*time.Second); !v.Now().WallUTC.Equal(want) {
		t.Fatal(v.Now(), want)
	}
}

func TestVirtualDeterministicOrdering(t *testing.T) {
	for run := 0; run < 100; run++ {
		v := virtual(t, 20)
		start := v.Now().Instant
		var log []string
		record := func(s string, at time.Duration) func() {
			return func() {
				if elapsed, err := v.Now().Instant.Sub(start); err != nil || elapsed != at {
					t.Fatalf("%s at %v, %v", s, elapsed, err)
				}
				log = append(log, s)
			}
		}
		schedule(t, v, 3*time.Second, record("late", 3*time.Second))
		schedule(t, v, time.Second, func() { record("first", time.Second)(); schedule(t, v, 0, record("nested", time.Second)) })
		schedule(t, v, time.Second, record("second", time.Second))
		schedule(t, v, 2*time.Second, record("middle", 2*time.Second))
		if err := v.Advance(5 * time.Second); err != nil {
			t.Fatal(err)
		}
		if want := []string{"first", "second", "nested", "middle", "late"}; !reflect.DeepEqual(log, want) {
			t.Fatal(log)
		}
		if delta, err := v.Now().Instant.Sub(start); err != nil || delta != 5*time.Second {
			t.Fatal(delta, err)
		}
		if v.Pending() != 0 {
			t.Fatal(v.Pending())
		}
	}
}

func TestVirtualEventCancellation(t *testing.T) {
	v := virtual(t, 20)
	fired := 0
	e := schedule(t, v, time.Second, func() { fired++ })
	copyOfHandle := *e
	if !copyOfHandle.Cancel() || e.Cancel() || v.Pending() != 0 {
		t.Fatal("cancel did not remove event")
	}
	var self *c.Event
	self = schedule(t, v, time.Second, func() {
		if self.Cancel() {
			t.Error("running callback cancelled")
		}
		fired++
	})
	other := schedule(t, v, 2*time.Second, func() { fired += 100 })
	schedule(t, v, time.Second, func() {
		if !other.Cancel() {
			t.Error("pending event not cancelled")
		}
	})
	if err := v.Advance(3 * time.Second); err != nil {
		t.Fatal(err)
	}
	if fired != 1 || v.Pending() != 0 {
		t.Fatal(fired, v.Pending())
	}
}

func TestVirtualWallOverflowDuringCallback(t *testing.T) {
	v := virtual(t, 10)
	start := v.Now()
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC)
	schedule(t, v, time.Second, func() {
		if err := v.SetWall(last); err != nil {
			t.Fatal(err)
		}
	})
	fired := false
	schedule(t, v, 2*time.Second, func() { fired = true })
	if err := v.Advance(3 * time.Second); !errors.Is(err, c.ErrOverflow) {
		t.Fatal(err)
	}
	if elapsed, err := v.Now().Instant.Sub(start.Instant); err != nil || elapsed != time.Second {
		t.Fatal(elapsed, err)
	}
	if fired || v.Pending() != 1 {
		t.Fatal(fired, v.Pending())
	}
	if err := v.SetWall(start.WallUTC); err != nil {
		t.Fatal(err)
	}
	if err := v.Advance(time.Second); err != nil {
		t.Fatal(err)
	}
	if !fired || v.Pending() != 0 {
		t.Fatal(fired, v.Pending())
	}
}

func TestVirtualZeroAndExactBudget(t *testing.T) {
	v := virtual(t, 2)
	fired := 0
	schedule(t, v, 0, func() { fired++ })
	schedule(t, v, 0, func() { fired++ })
	if err := v.Sleep(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if fired != 0 {
		t.Fatal("zero Sleep dispatched events")
	}
	if err := v.Advance(0); err != nil {
		t.Fatal(err)
	}
	if fired != 2 || v.Pending() != 0 {
		t.Fatal(fired, v.Pending())
	}
	if (*c.Event)(nil).Cancel() || new(c.Event).Cancel() {
		t.Fatal("inactive event cancelled")
	}
}

func TestVirtualSleepCancellationAndWake(t *testing.T) {
	for _, cancelAt := range []time.Duration{time.Second, 2 * time.Second, 3 * time.Second} {
		t.Run(cancelAt.String(), func(t *testing.T) {
			v := virtual(t, 20)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if cancelAt <= 2*time.Second {
				schedule(t, v, cancelAt, cancel)
			}
			result := make(chan error, 1)
			go func() { result <- v.Sleep(ctx, 2*time.Second) }()
			wantPending := 1
			if cancelAt <= 2*time.Second {
				wantPending++
			}
			waitPending(t, v, wantPending)
			if err := v.Advance(2 * time.Second); err != nil {
				t.Fatal(err)
			}
			err := await(t, result)
			if cancelAt <= 2*time.Second {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if v.Pending() != 0 {
				t.Fatal("sleep retained pending timer", v.Pending())
			}
		})
	}
	// Cancel without advancing; return must eagerly remove the queued timer.
	v := virtual(t, 10)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- v.Sleep(ctx, time.Hour) }()
	waitPending(t, v, 1)
	cancel()
	if err := await(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if v.Pending() != 0 {
		t.Fatal(v.Pending())
	}
	if err := v.Advance(time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestVirtualValidationAndOverflow(t *testing.T) {
	if _, err := c.NewVirtual(time.Now(), 0); !errors.Is(err, c.ErrBudget) {
		t.Fatal(err)
	}
	if _, err := c.NewVirtual(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), 1); !errors.Is(err, c.ErrOverflow) {
		t.Fatal(err)
	}
	v := virtual(t, 10)
	before := v.Now()
	if err := v.Advance(-1); !errors.Is(err, c.ErrDuration) {
		t.Fatal(err)
	}
	if _, err := v.AfterFunc(-1, func() {}); !errors.Is(err, c.ErrDuration) {
		t.Fatal(err)
	}
	if _, err := v.AfterFunc(0, nil); !errors.Is(err, c.ErrCallback) {
		t.Fatal(err)
	}
	if err := v.SetWall(time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)); !errors.Is(err, c.ErrOverflow) {
		t.Fatal(err)
	}
	if !v.Now().WallUTC.Equal(before.WallUTC) {
		t.Fatal("invalid operation changed wall")
	}
	if err := v.Advance(time.Duration(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	before = v.Now()
	if err := v.Advance(1); !errors.Is(err, c.ErrOverflow) {
		t.Fatal(err)
	}
	if _, err := v.AfterFunc(1, func() {}); !errors.Is(err, c.ErrOverflow) {
		t.Fatal(err)
	}
	if err := v.Sleep(context.Background(), 1); !errors.Is(err, c.ErrOverflow) {
		t.Fatal(err)
	}
	if cmp, err := before.Instant.Compare(v.Now().Instant); err != nil || cmp != 0 {
		t.Fatal(cmp, err)
	}
	wallLimit, _ := c.NewVirtual(time.Date(9999, 12, 31, 23, 59, 59, 999999999, time.UTC), 10)
	if err := wallLimit.Advance(1); !errors.Is(err, c.ErrOverflow) {
		t.Fatal(err)
	}
}

func TestVirtualGuardAndReentrancy(t *testing.T) {
	v := virtual(t, 3)
	calls := 0
	var repeat func()
	repeat = func() { calls++; schedule(t, v, 0, repeat) }
	schedule(t, v, 0, repeat)
	if err := v.Advance(time.Second); !errors.Is(err, c.ErrEventLimit) {
		t.Fatal(err)
	}
	if calls != 3 || v.Pending() != 1 {
		t.Fatal(calls, v.Pending())
	}
	if err := v.Advance(0); !errors.Is(err, c.ErrEventLimit) {
		t.Fatal(err)
	}
	if calls != 6 {
		t.Fatal(calls)
	}
	v = virtual(t, 3)
	schedule(t, v, 0, func() {
		if err := v.Advance(0); !errors.Is(err, c.ErrAdvancing) {
			t.Error(err)
		}
	})
	if err := v.Advance(0); err != nil {
		t.Fatal(err)
	}
}

func TestCancelledSleepDoesNotConsumeBudget(t *testing.T) {
	for run := 0; run < 100; run++ {
		v := virtual(t, 2)
		ctx, cancel := context.WithCancel(context.Background())
		schedule(t, v, time.Second, cancel)
		fired := false
		schedule(t, v, 3*time.Second, func() { fired = true })
		result := make(chan error, 1)
		go func() { result <- v.Sleep(ctx, 2*time.Second) }()
		waitPending(t, v, 3)
		// Exactly two callbacks regardless of when Sleep's goroutine cleans up.
		if err := v.Advance(4 * time.Second); err != nil {
			t.Fatal(err)
		}
		if err := await(t, result); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if !fired || v.Pending() != 0 {
			t.Fatal(fired, v.Pending())
		}
	}
}

func TestVirtualConcurrentAdvanceAndCancel(t *testing.T) {
	v := virtual(t, 100)
	entered := make(chan struct{})
	release := make(chan struct{})
	result := make(chan error, 1)
	schedule(t, v, 0, func() { close(entered); <-release })
	go func() { result <- v.Advance(time.Second) }()
	<-entered
	if err := v.Advance(0); !errors.Is(err, c.ErrAdvancing) {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for n := 0; n < 20; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, err := v.AfterFunc(2*time.Second, func() { t.Error("cancelled event fired") })
			if err != nil {
				t.Error(err)
				return
			}
			e.Cancel()
			_ = v.Now()
		}()
	}
	wg.Wait()
	close(release)
	if err := await(t, result); err != nil {
		t.Fatal(err)
	}
	if v.Pending() != 0 {
		t.Fatal(v.Pending())
	}
}

func TestVirtualPanicReleasesDriver(t *testing.T) {
	v := virtual(t, 10)
	schedule(t, v, 0, func() { panic("injected") })
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		_ = v.Advance(0)
	}()
	if err := v.Advance(0); err != nil {
		t.Fatal(err)
	}
	if v.Pending() != 0 {
		t.Fatal(v.Pending())
	}
}

func TestCancelWhileAdvanceQueuedOrSelected(t *testing.T) {
	for _, selected := range []bool{false, true} {
		name := "queued"
		if selected {
			name = "selected"
		}
		t.Run(name, func(t *testing.T) {
			v := virtual(t, 10)
			entered := make(chan struct{})
			release := make(chan struct{})
			var once sync.Once
			resume := func() { once.Do(func() { close(release) }) }
			defer resume()
			var calls atomic.Int32
			if !selected {
				schedule(t, v, 0, func() { close(entered); <-release })
			}
			target := schedule(t, v, time.Second, func() {
				if selected {
					close(entered)
					<-release
				}
				calls.Add(1)
			})
			result := make(chan error, 1)
			go func() { result <- v.Advance(time.Second) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("Advance did not reach barrier")
			}
			if got := target.Cancel(); got == selected {
				t.Fatalf("Cancel=%v, selected=%v", got, selected)
			}
			if target.Cancel() {
				t.Fatal("second Cancel succeeded")
			}
			if v.Pending() != 0 {
				t.Fatal("cancelled/selected event still pending")
			}
			resume()
			if err := await(t, result); err != nil {
				t.Fatal(err)
			}
			if err := v.Advance(time.Second); err != nil {
				t.Fatal(err)
			}
			want := int32(0)
			if selected {
				want = 1
			}
			if got := calls.Load(); got != want {
				t.Fatal(got, want)
			}
			if v.Pending() != 0 {
				t.Fatal("event leak")
			}
		})
	}
}

func TestCancelDispatchContention(t *testing.T) {
	for run := 0; run < 100; run++ {
		v := virtual(t, 10)
		var calls atomic.Int32
		e := schedule(t, v, time.Second, func() { calls.Add(1) })
		start := make(chan struct{})
		advanced := make(chan error, 1)
		cancelled := make(chan bool, 1)
		go func() { <-start; advanced <- v.Advance(time.Second) }()
		go func() { <-start; cancelled <- e.Cancel() }()
		close(start)
		if err := await(t, advanced); err != nil {
			t.Fatal(err)
		}
		var won bool
		select {
		case won = <-cancelled:
		case <-time.After(5 * time.Second):
			t.Fatal("Cancel stalled")
		}
		want := int32(1)
		if won {
			want = 0
		}
		if got := calls.Load(); got != want {
			t.Fatalf("cancel=%v calls=%d", won, got)
		}
		if e.Cancel() {
			t.Fatal("terminal event cancelled again")
		}
		if err := v.Advance(0); err != nil {
			t.Fatal(err)
		}
		if calls.Load() != want || v.Pending() != 0 {
			t.Fatal("duplicate callback or timer leak")
		}
	}
}
