package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

type releasePeerRecoveryClock struct {
	clock.Clock
	release func() error
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
	if err := closing.once(ctx, true); !errors.Is(err, storage.ErrR1Busy) {
		t.Fatal("shutdown did not encounter the peer's retained recovery authority", err)
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
