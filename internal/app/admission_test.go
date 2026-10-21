package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDrainClosesAdmissionBeforeWaiting(t *testing.T) {
	a := new(Admission)
	if a.Wait(context.Background()) == nil {
		t.Fatal("wait without closure")
	}
	release, ok := a.Acquire()
	if !ok {
		t.Fatal("initial admission rejected")
	}
	a.Drain()
	if _, ok := a.Acquire(); ok {
		t.Fatal("draining node admitted new work")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(a.Wait(ctx), context.Canceled) {
		t.Fatal("active work did not block drain")
	}
	release()
	release()
	if err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentAdmissionAndDrain(t *testing.T) {
	a := new(Admission)
	var wg sync.WaitGroup
	for range 100 {
		wg.Go(func() {
			release, ok := a.Acquire()
			if ok {
				release()
			}
		})
	}
	a.Drain()
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.Acquire(); ok {
		t.Fatal("admission reopened")
	}
}
