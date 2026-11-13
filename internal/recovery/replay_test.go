package recovery

import (
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/metadata/operations"
	"testing"
)

func TestVerifyReplayUsesSealedEventsAndRejectsDamagedArtifacts(t *testing.T) {
	w, _ := checkpointFixture(t)
	validator, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	global, err := validator.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := global.StorageSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	effect, err := operations.Projection("SystemConfig", "global-policy", 1, snapshot, false)
	if err != nil {
		t.Fatal(err)
	}
	seal := fixtureSeal(t, w, 0, w.Catalog.Genesis.SHA256, []Effect{effect})
	a, b, count, err := VerifyReplay(t.Context(), w.Objects, w.Catalog, 0)
	if err != nil || a != b || count < 1 {
		t.Fatal(a, b, count, err)
	}
	// A hash/key/state field alone cannot turn truncated sealed bytes into proof.
	w.Objects.(*memoryObjects).objects[seal.Key] = []byte("corrupt")
	if _, _, _, err := VerifyReplay(t.Context(), w.Objects, w.Catalog, 0); err == nil {
		t.Fatal("corrupt seal accepted")
	}
}

func TestVerifyReplayRejectsUnsealedEpoch(t *testing.T) {
	w, _ := checkpointFixture(t)
	if _, _, _, err := VerifyReplay(t.Context(), w.Objects, w.Catalog, 0); err == nil {
		t.Fatal("unsealed epoch accepted")
	}
}
