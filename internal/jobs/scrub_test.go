package jobs

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/KinasTomes/miles/internal/config"
)

func TestScrubWorkerDryRunAndCancel(t *testing.T) {
	worker := &ScrubWorker{}
	exec := worker.Executor()

	step, err := exec.Step(context.Background(), Job{
		DryRun: true,
	}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !step.Done || !step.Settled {
		t.Fatalf("expected done and settled, got %+v", step)
	}
	var res ScrubResult
	if err := json.Unmarshal(step.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.SkipReasons["dry_run_or_cancelled"] != 1 {
		t.Fatalf("expected dry_run_or_cancelled, got %+v", res)
	}
}

func TestScrubWorkerValidateParams(t *testing.T) {
	worker := &ScrubWorker{}
	exec := worker.Executor()

	if err := exec.Validate(json.RawMessage(`{"limit": 50}`)); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	if err := exec.Validate(json.RawMessage(`{"limit": -1}`)); err == nil {
		t.Fatal("negative limit should be rejected")
	}
	if err := exec.Validate(json.RawMessage(`{"limit": 1001}`)); err == nil {
		t.Fatal("limit > 1000 should be rejected")
	}
}

func TestScrubWorkerMissingStoresHandledGracefully(t *testing.T) {
	worker := &ScrubWorker{Store: nil, Packs: nil}
	exec := worker.Executor()

	step, err := exec.Step(context.Background(), Job{}, config.GlobalSnapshot{})
	if err != nil {
		t.Fatal(err)
	}
	if !step.Done || !step.Settled {
		t.Fatalf("expected step to finish, got %+v", step)
	}
	var res ScrubResult
	if err := json.Unmarshal(step.Result, &res); err != nil {
		t.Fatal(err)
	}
	if res.SkipReasons["missing_store_or_packs"] != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
}
