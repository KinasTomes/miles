package dynamodb

import (
	suite "github.com/KinasTomes/miles/internal/metadata/contracttest"
	"github.com/KinasTomes/miles/internal/storage"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	"testing"
	"time"
)

func TestLocalObservationContract(t *testing.T) {
	suite.RunObservation(t, func(t *testing.T, f shared.Fixture) suite.ObservingStore { m, _ := fixtureStore(t, f); return m })
}

func TestLocalObservationCAS(t *testing.T) {
	suite.RunObservationCAS(t, func(t *testing.T, f shared.Fixture) suite.ObservingStore { m, _ := fixtureStore(t, f); return m })
}

func TestLocalExportLagUsesAtomicEventObservationAndExportProgress(t *testing.T) {
	f := newRecoveryFixture(t)
	request, writer := f.mutation(t, "pilot-lag")
	before := f.utc.Now()
	if _, err := f.m.MutateControl(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	oldest, err := f.m.ObserveExportLag(t.Context(), f.catalog.History)
	if err != nil || !oldest.Equal(before) {
		t.Fatal("missing publication timestamp", oldest, err)
	}
	f.utc.Advance(10 * time.Second)
	if _, err = f.peer.MutateControl(t.Context(), request); err != nil {
		t.Fatal("idempotent mutation", err)
	}
	again, err := f.m.ObserveExportLag(t.Context(), f.catalog.History)
	if err != nil || !again.Equal(oldest) {
		t.Fatal("retry reset event lag", again, err)
	}
	f.release(t, writer)
	if err = f.service(f.m, f.worker).Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if oldest, err = f.m.ObserveExportLag(t.Context(), f.catalog.History); err != nil || !oldest.IsZero() {
		t.Fatal("exported event still pending", oldest, err)
	}
}

func TestLocalRuntimeObservationUsesSealedCut(t *testing.T) {
	f := newRecoveryFixture(t)
	initial, err := f.m.ObserveRuntime(t.Context(), f.catalog.History, f.utc.Now())
	if err != nil || initial.GateClosed || !initial.CutAt.Equal(f.utc.Now()) {
		t.Fatal("initial cut schedule", initial, err)
	}
	if _, err = f.m.ObserveRuntime(t.Context(), "foreign-history", f.utc.Now()); err != storage.ErrFenced {
		t.Fatal("mixed history observation", err)
	}
	f.utc.Advance(10 * time.Second)
	if _, err = f.service(f.m, f.worker).CloseAndSeal(t.Context(), f.request(0)); err != nil {
		t.Fatal(err)
	}
	epoch, err := f.m.GetRecoveryEpoch(t.Context(), f.catalog.History, 0)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := f.m.ObserveRuntime(t.Context(), f.catalog.History, f.utc.Now())
	if err != nil || epoch.State != "Sealed" || !sample.CutAt.Equal(epoch.CutAt) || sample.CutAt.Equal(initial.CutAt) {
		t.Fatal("cut observation does not follow sealed publication", sample, epoch, err)
	}
}

func TestLocalRuntimeObservationDoesNotResetAgeAtUnsealedClose(t *testing.T) {
	f := newRecoveryFixture(t)
	f.utc.Advance(10 * time.Second)
	epoch := closeRecovery(t, f.m, f.request(0))
	if epoch.State != "Closed" {
		t.Fatal(epoch.State)
	}
	if _, err := f.m.ObserveRuntime(t.Context(), f.catalog.History, f.utc.Now()); err != storage.ErrUnavailable {
		t.Fatal("advanced cadence was treated as a sealed cut", err)
	}
}
