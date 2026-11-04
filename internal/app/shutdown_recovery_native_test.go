package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/storage"
)

type releasePeerRecoveryClock struct {
	clock.Clock
	release func() error
}

type failedRecoveryRelease struct {
	jobs.Authority
	failure error
	fence   storage.WriterFence
	calls   int
}

func (a *failedRecoveryRelease) Close(ctx context.Context, f storage.WriterFence) error {
	a.calls++
	if a.calls == 1 {
		a.fence = f
		return a.failure
	}
	return a.Authority.Close(ctx, f)
}

func (c releasePeerRecoveryClock) Sleep(ctx context.Context, d time.Duration) error {
	if d == time.Second {
		if err := c.release(); err != nil {
			return err
		}
	}
	return c.Clock.Sleep(ctx, d)
}

func nativeShutdownDuringPeerRecovery(t *testing.T, ctx context.Context, nodes []Dependencies) {
	t.Helper()
	peer, closing := nodes[0].Journal, nodes[2].Journal
	fence, err := peer.authority.Acquire(ctx, "peer-sealing-at-shutdown", "recovery")
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			if err := peer.authority.Close(context.Background(), fence); err != nil {
				t.Error(err)
			}
		}
	}()
	gate, err := peer.store.GetR1Gate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request := storage.RecoveryCutRequest{HistoryID: gate.HistoryID, Epoch: gate.Epoch, CloseID: strings.Repeat("f", 32), Worker: fence}
	epoch, err := peer.store.BeginRecoveryCut(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for step := 0; epoch.State == "Closing"; step++ {
		if step > 100 {
			t.Fatal("empty epoch did not close")
		}
		epoch, err = peer.store.StepRecoveryCut(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
	}
	if epoch.State != "Closed" {
		t.Fatal("unexpected epoch state", epoch.State)
	}
	if workErr, cleanupErr := closing.once(ctx, true); !errors.Is(workErr, storage.ErrR1Busy) || cleanupErr != nil {
		t.Fatal("shutdown did not encounter the peer's retained recovery authority", workErr, cleanupErr)
	}
	original := closing.clock
	defer func() { closing.clock = original }()
	closing.clock = releasePeerRecoveryClock{Clock: original, release: func() error {
		if released {
			return nil
		}
		if err := peer.authority.Close(ctx, fence); err != nil {
			return err
		}
		released = true
		return nil
	}}
	// Flush sees a busy peer while its own lease release also fails. The
	// release error must survive even though another attempt could succeed.
	authority := closing.authority
	failure := errors.New("injected recovery lease release failure")
	fault := &failedRecoveryRelease{Authority: authority, failure: failure}
	closing.authority = fault
	defer func() {
		closing.authority = authority
		if fault.fence.LeaseID != "" {
			if err := authority.Close(context.Background(), fault.fence); err != nil {
				t.Error(err)
			}
		}
	}()
	err = closing.flush(ctx)
	closing.authority = authority
	if !errors.Is(err, storage.ErrR1Busy) || !errors.Is(err, failure) || fault.calls != 1 || released {
		t.Fatal("shutdown retried after failed lease cleanup", err, fault.calls, released)
	}
	lease, err := closing.store.GetR1Lease(ctx, fault.fence.LeaseID)
	if err != nil || lease.State != "Active" {
		t.Fatal("injected cleanup failure did not retain the original lease", lease, err)
	}
	if err := authority.Close(ctx, fault.fence); err != nil {
		t.Fatal("fixture lease cleanup", err)
	}
	fault.fence = storage.WriterFence{}
	if err := closing.flush(ctx); err != nil {
		t.Fatal("shutdown failed instead of waiting for peer recovery", err)
	}
	if !released {
		t.Fatal("shutdown never retried the busy recovery worker")
	}
	state, err := closing.store.GetRecoveryState(ctx, gate.HistoryID)
	if err != nil || state.NextToSeal != gate.Epoch+1 {
		t.Fatal("shutdown did not seal the closed epoch", state, err)
	}
}
