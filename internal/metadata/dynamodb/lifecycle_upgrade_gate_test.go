package dynamodb

import (
	"testing"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
)

func TestLegacyLifecycleUpgradeGateBlocksUnknownWithR1Protection(t *testing.T) {
	a := lifecycle.Action{ID: "job-1", State: "Unknown"}
	j := jobs.Job{ID: "job-1", State: jobs.Running, PendingStep: "step-1"}
	l := s.R1Lease{State: "Active", Pending: []string{"lifecycle/job-1"}, PinCount: 1}
	f := classifyLegacyLifecycle(a, j, l)
	if !f.Blocked || !f.Pending || f.Reason == "" {
		t.Fatalf("expected fail-closed finding, got %#v", f)
	}
}

func TestLegacyLifecycleUpgradeGateAllowsQuiescentTerminalHistory(t *testing.T) {
	a := lifecycle.Action{ID: "job-1", State: "Done"}
	j := jobs.Job{ID: "job-1", State: jobs.Succeeded}
	l := s.R1Lease{State: "Released"}
	f := classifyLegacyLifecycle(a, j, l)
	if f.Blocked || f.Pending {
		t.Fatalf("expected quiescent terminal history to pass, got %#v", f)
	}
}
