package faults

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

type dummyHTTPClient struct {
	called bool
}

func (d *dummyHTTPClient) Do(r *http.Request) (*http.Response, error) {
	d.called = true
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil))}, nil
}

func TestFaultRegistryOptInAndLifecycle(t *testing.T) {
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

	// Nil registry is safe
	var nilReg *Registry
	if nilReg.Enabled() {
		t.Fatal("nil registry should not be enabled")
	}
	if _, err := nilReg.Add(now, FaultRule{Type: FaultS35xx, Scope: ScopeCluster, Rate: 1.0, Duration: 60}); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled for nil registry, got %v", err)
	}
	if _, err := nilReg.List(now); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled for nil registry, got %v", err)
	}
	if err := nilReg.Delete("abc"); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled for nil registry, got %v", err)
	}
	if nilReg.ShouldInject(now, FaultS35xx, "node-1") {
		t.Fatal("nil registry should never inject")
	}

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
	if !reg.Enabled() {
		t.Fatal("expected enabled after SetEnabled(true)")
	}

	// Invalid rules
	if _, err := reg.Add(now, FaultRule{Type: "invalid_type", Scope: ScopeCluster, Rate: 1.0, Duration: 60}); err == nil {
		t.Fatal("expected error for invalid type")
	}
	if _, err := reg.Add(now, FaultRule{Type: FaultS35xx, Scope: ScopeNode, NodeID: "", Rate: 1.0, Duration: 60}); err == nil {
		t.Fatal("expected error for node scope without node_id")
	}
	if _, err := reg.Add(now, FaultRule{Type: FaultS35xx, Scope: ScopeCluster, Rate: 0.0, Duration: 60}); err == nil {
		t.Fatal("expected error for rate 0.0")
	}
	if _, err := reg.Add(now, FaultRule{Type: FaultS35xx, Scope: ScopeCluster, Rate: 1.5, Duration: 60}); err == nil {
		t.Fatal("expected error for rate > 1.0")
	}
	if _, err := reg.Add(now, FaultRule{Type: FaultS35xx, Scope: ScopeCluster, Rate: 1.0, Duration: 0}); err == nil {
		t.Fatal("expected error for duration <= 0")
	}

	// Valid rule
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
	if reg.ShouldInject(future, FaultS35xx, "node-1") {
		t.Fatal("should not inject after expiration")
	}

	// Delete
	_, err = reg.Add(now, FaultRule{
		ID:       "flt-explicit",
		Type:     FaultDDBThrottle,
		Scope:    ScopeCluster,
		Rate:     1.0,
		Duration: 60,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Delete("flt-explicit"); err != nil {
		t.Fatalf("failed to delete rule: %v", err)
	}
	if err := reg.Delete("flt-explicit"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on second delete, got %v", err)
	}

	// Disable clears rules
	reg.SetEnabled(false)
	if reg.Enabled() {
		t.Fatal("expected disabled")
	}
	if _, err := reg.List(now); !errors.Is(err, ErrDisabled) {
		t.Fatalf("expected ErrDisabled after SetEnabled(false), got %v", err)
	}
}

func TestWrapHTTPClientDDBThrottle(t *testing.T) {
	reg := NewRegistry(true)
	rule := FaultRule{
		Type:     FaultDDBThrottle,
		Scope:    ScopeCluster,
		Rate:     1.0,
		Duration: 60,
	}
	if _, err := reg.Add(time.Now(), rule); err != nil {
		t.Fatal(err)
	}

	dummy := &dummyHTTPClient{}
	wrapped := WrapHTTPClient(dummy, reg, "node-1")

	req, _ := http.NewRequest("POST", "https://dynamodb.ap-southeast-1.amazonaws.com/", bytes.NewReader([]byte("{}")))
	resp, err := wrapped.Do(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected status 400 for DDB throttle, got %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Amzn-Errortype") != "ProvisionedThroughputExceededException" {
		t.Fatalf("expected header ProvisionedThroughputExceededException, got %v", resp.Header.Get("X-Amzn-Errortype"))
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("ProvisionedThroughputExceededException")) {
		t.Fatalf("unexpected body: %s", string(body))
	}
	if dummy.called {
		t.Fatal("underlying client should not have been called when throttled")
	}
}

func TestWrapPackStoreCorruptPack(t *testing.T) {
	ctx := context.Background()
	reg := NewRegistry(true)
	rule := FaultRule{
		Type:     FaultCorruptPack,
		Scope:    ScopeCluster,
		Rate:     1.0,
		Duration: 60,
	}
	if _, err := reg.Add(time.Now(), rule); err != nil {
		t.Fatal(err)
	}

	rawPacks := fake.NewPacks(nil)
	ref := storage.PackRef{ID: "pack-test", Generation: "gen-1"}
	originalData := []byte("hello world 1234567890")
	info := storage.PackInfo{Version: storage.ModelVersion, Ref: ref, Size: int64(len(originalData)), Class: storage.Standard, State: "Warm"}
	if err := rawPacks.PutPack(ctx, info, bytes.NewReader(originalData)); err != nil {
		t.Fatal(err)
	}

	wrapped := WrapPackStore(rawPacks, reg, "node-1")

	rc, err := wrapped.RangeGet(ctx, ref, storage.ByteRange{Offset: 0, Length: int64(len(originalData))})
	if err != nil {
		t.Fatalf("RangeGet failed: %v", err)
	}
	defer rc.Close()

	readData, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}

	if bytes.Equal(readData, originalData) {
		t.Fatal("expected corrupted data, got identical bytes")
	}
	if readData[0] != (originalData[0] ^ 0xFF) {
		t.Fatalf("expected first byte inverted, got %x vs %x", readData[0], originalData[0]^0xFF)
	}
}
