package recovery

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	s "github.com/KinasTomes/miles/internal/storage"
)

type flushClock struct {
	clock.Clock
	sleep chan time.Duration
	wake  chan struct{}
}

func (c flushClock) Sleep(ctx context.Context, d time.Duration) error {
	select {
	case c.sleep <- d:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-c.wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type blockedFlush struct {
	s.RecoveryStore
	catalog Catalog
	started chan struct{}
	release chan struct{}
	renewed chan struct{}
	failure error
}

func (m blockedFlush) GetR1Gate(context.Context) (s.R1Gate, error) {
	return s.R1Gate{HistoryID: m.catalog.History, Incarnation: m.catalog.Incarnation, Epoch: 1}, nil
}
func (m blockedFlush) GetRecoveryState(context.Context, string) (s.RecoveryState, error) {
	return s.RecoveryState{NextToSeal: 1}, nil
}
func (m blockedFlush) CheckRecoveryWorker(context.Context, s.WriterFence) error { return nil }
func (m blockedFlush) ListOutbox(ctx context.Context, _ s.OutboxQuery) (s.OutboxPage, error) {
	close(m.started)
	select {
	case <-m.release:
		return s.OutboxPage{}, nil
	case <-ctx.Done():
		return s.OutboxPage{}, ctx.Err()
	}
}
func (m blockedFlush) RenewR1(context.Context, s.WriterFence) (s.R1Lease, error) {
	m.renewed <- struct{}{}
	return s.R1Lease{}, m.failure
}

func TestFlushSupervisedRenewsAndJoins(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
	}{{"long flush", nil}, {"fenced", s.ErrFenced}} {
		t.Run(test.name, func(t *testing.T) {
			failure := test.failure
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			catalog := Catalog{Version: 1, History: strings.Repeat("1", 32), Incarnation: strings.Repeat("2", 32), Genesis: ref([]byte("{}"))}
			store := blockedFlush{catalog: catalog, started: make(chan struct{}), release: make(chan struct{}), renewed: make(chan struct{}, 1), failure: failure}
			timer := flushClock{Clock: clock.NewReal(), sleep: make(chan time.Duration, 1), wake: make(chan struct{}, 1)}
			worker := Worker{Store: store, Payloads: memoryInputs{}, Objects: &memoryObjects{}, Catalog: catalog, Clock: timer, Fence: s.WriterFence{ServiceIncarnation: catalog.Incarnation, LeaseID: "flush", Epoch: 1}}
			done := make(chan error, 1)
			go func() { done <- worker.FlushSupervised(ctx) }()
			select {
			case <-store.started:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			for i := 0; i < 5; i++ {
				select {
				case d := <-timer.sleep:
					if d != 15*time.Second {
						t.Fatal("unexpected lease renewal cadence", d)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				timer.wake <- struct{}{}
				<-store.renewed
				if failure != nil {
					break
				}
			}
			if failure == nil {
				close(store.release)
			}
			if err := <-done; !errors.Is(err, failure) {
				t.Fatal("flush did not retain its renewal outcome", err)
			}
			select {
			case <-store.renewed:
				t.Fatal("renewal outlived the supervised flush")
			default:
			}
		})
	}
}

type blockedCut struct{ blockedFlush }

func (m blockedCut) BeginRecoveryCut(ctx context.Context, _ s.RecoveryCutRequest) (s.RecoveryEpoch, error) {
	close(m.started)
	<-ctx.Done()
	return s.RecoveryEpoch{}, ctx.Err()
}

func TestCloseAndSealSupervisedStopsWhenNativeLeaseIsFenced(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	catalog := Catalog{Version: 1, History: strings.Repeat("1", 32), Incarnation: strings.Repeat("2", 32), Genesis: ref([]byte("{}"))}
	store := blockedCut{blockedFlush{catalog: catalog, started: make(chan struct{}), renewed: make(chan struct{}, 1), failure: s.ErrFenced}}
	timer := flushClock{Clock: clock.NewReal(), sleep: make(chan time.Duration, 1), wake: make(chan struct{}, 1)}
	fence := s.WriterFence{ServiceIncarnation: catalog.Incarnation, LeaseID: "operator-cut", Epoch: 1}
	worker := Worker{Store: store, Payloads: memoryInputs{}, Objects: &memoryObjects{}, Catalog: catalog, Clock: timer, Fence: fence}
	done := make(chan error, 1)
	go func() {
		_, err := worker.CloseAndSealSupervised(ctx, s.RecoveryCutRequest{HistoryID: catalog.History, CloseID: strings.Repeat("3", 32), Worker: fence})
		done <- err
	}()
	select {
	case <-store.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-timer.sleep:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	timer.wake <- struct{}{}
	select {
	case err := <-done:
		if !errors.Is(err, s.ErrFenced) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
