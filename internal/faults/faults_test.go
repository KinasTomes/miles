package faults

import (
	"errors"
	"testing"
	"time"
)

func TestFaultRegistryOptInAndLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

	// Disabled by default
	reg := NewRegistry(false)
	if reg.Enabled() {
		t.Fatal("expected disabled by default")
	}

	_, err := reg.Add(now, FaultRule{
		Type:     FaultS35xx,
		Scope:    ScopeCluster,
		Rate:     1.0,
		Duration: 60,
	})
	if !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled, got %v", err)
	}

	// Enable
	reg.SetEnabled(true)
	rule, err := reg.Add(now, FaultRule{
		Type:     FaultS35xx,
		Scope:    ScopeCluster,
		Rate:     1.0,
		Duration: 60,
	})
	if err != nil {
		t.Fatalf("failed to add rule: %v", err)
	}
	if rule.ID == "" {
		t.Fatal("expected generated fault ID")
	}

	// List
	list, err := reg.List(now)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 rule, got %v, err=%v", list, err)
	}

	// ShouldInject
	if !reg.ShouldInject(now, FaultS35xx, "node-1") {
		t.Fatal("expected injection for cluster scope")
	}
	if reg.ShouldInject(now, FaultDDBThrottle, "node-1") {
		t.Fatal("unexpected injection for different fault type")
	}

	// Node scope
	_, err = reg.Add(now, FaultRule{
		Type:     FaultKillNode,
		Scope:    ScopeNode,
		NodeID:   "node-2",
		Rate:     1.0,
		Duration: 60,
	})
	if err != nil {
		t.Fatalf("failed to add node-scoped rule: %v", err)
	}
	if reg.ShouldInject(now, FaultKillNode, "node-1") {
		t.Fatal("should not inject for node-1")
	}
	if !reg.ShouldInject(now, FaultKillNode, "node-2") {
		t.Fatal("should inject for node-2")
	}

	// Expiration
	future := now.Add(61 * time.Second)
	listAfter, err := reg.List(future)
	if err != nil || len(listAfter) != 0 {
		t.Fatalf("expected rules to expire, got %d rules", len(listAfter))
	}

	// Delete
	reg.Add(now, FaultRule{
		ID:       "flt-explicit",
		Type:     FaultDDBThrottle,
		Scope:    ScopeCluster,
		Rate:     1.0,
		Duration: 60,
	})
	if err := reg.Delete("flt-explicit"); err != nil {
		t.Fatalf("failed to delete rule: %v", err)
	}
	if err := reg.Delete("flt-explicit"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second delete, got %v", err)
	}
}
