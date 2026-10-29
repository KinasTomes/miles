package app

import (
	"context"
	"errors"
	"sync"
)

// Admission tracks process-local work during shutdown. Each real request must
// separately enforce authorization, dependency checks and shared R1 admission.
type Admission struct {
	mu       sync.Mutex
	draining bool
	active   int
	idle     chan struct{}
}

func (a *Admission) Acquire() (release func(), ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.draining {
		return nil, false
	}
	if a.active == 0 {
		a.idle = make(chan struct{})
	}
	a.active++
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			defer a.mu.Unlock()
			a.active--
			if a.active == 0 {
				close(a.idle)
			}
		})
	}, true
}
func (a *Admission) Drain()         { a.mu.Lock(); a.draining = true; a.mu.Unlock() }
func (a *Admission) Draining() bool { a.mu.Lock(); defer a.mu.Unlock(); return a.draining }
func (a *Admission) Status() (int64, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return int64(a.active), a.draining
}
func (a *Admission) Wait(ctx context.Context) error {
	a.mu.Lock()
	if !a.draining {
		a.mu.Unlock()
		return errors.New("admission must be closed before waiting")
	}
	if a.active == 0 {
		a.mu.Unlock()
		return nil
	}
	idle := a.idle
	a.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
