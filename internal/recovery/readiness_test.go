package recovery

import (
	"context"
	"errors"
	s "github.com/KinasTomes/miles/internal/storage"
	"strings"
	"testing"
	"time"
)

func TestInstallReadinessRechecksClassAfterStaging(t *testing.T) {
	w, root, packs := liveCheckpointFixture(t)
	retention := Retention{Checkpointer: w, Store: &memoryCatalog{}, Packs: packs}
	if e := retention.Publish(t.Context(), root); e != nil {
		t.Fatal(e)
	}
	changed := false
	installer := &memoryInstaller{stageHook: func() {
		if !changed {
			changed = true
			if e := packs.ChangeStorageClass(t.Context(), s.PackRef{ID: "pack", Generation: "generation"}, s.GlacierIR); e != nil {
				t.Fatal(e)
			}
		}
	}}
	now := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
	_, e := (Recoverer{Retention: retention, Installer: installer}).Recover(t.Context(), RecoverRequest{Target: now, Bounds: s.R1Bounds{Lower: now, Upper: now, Horizon: now.Add(time.Hour)}, History: strings.Repeat("8", 32), Incarnation: strings.Repeat("9", 32)})
	if !errors.Is(e, s.ErrConflict) || !changed || len(installer.records) == 0 || installer.finished {
		t.Fatal("class drift reached readiness", e)
	}
}

type slowReadinessPacks struct{ s.PackStore }

func (p slowReadinessPacks) HeadPack(ctx context.Context, ref s.PackRef) (s.PackInfo, error) {
	timer := time.NewTimer(20 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return s.PackInfo{}, ctx.Err()
	case <-timer.C:
	}
	return p.PackStore.HeadPack(ctx, ref)
}
func TestInstallReadinessCannotUseExpiredTimeSample(t *testing.T) {
	w, root, packs := liveCheckpointFixture(t)
	index, e := w.Rebuild(t.Context(), root)
	if e != nil {
		t.Fatal(e)
	}
	ref := s.PackRef{ID: "pack", Generation: "generation"}
	expiry := time.Date(2026, 11, 5, 0, 0, 0, 0, time.UTC)
	if e = packs.ChangeStorageClass(t.Context(), ref, s.Glacier); e != nil {
		t.Fatal(e)
	}
	if e = packs.Restore(t.Context(), ref, s.RestoreRequest{Days: 1, Tier: "Standard"}); e != nil {
		t.Fatal(e)
	}
	if e = packs.CompleteRestore(ref, expiry); e != nil {
		t.Fatal(e)
	}
	p := index.Packs[ref]
	p.Class = s.Glacier
	p.State = "Restored"
	p.RestoredUntil = expiry
	index.Packs[ref] = p
	before := expiry.Add(-time.Millisecond)
	recoverer := Recoverer{Retention: Retention{Packs: slowReadinessPacks{packs}}}
	e = recoverer.checkFrozenReadiness(t.Context(), index, s.R1Bounds{Lower: before, Upper: before, Horizon: expiry.Add(time.Hour)})
	if !errors.Is(e, s.ErrCold) {
		t.Fatal("expired during HEAD verification", e)
	}
}
