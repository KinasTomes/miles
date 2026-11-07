// Package faults provides opt-in, administrative fault injection for testing and resilience.
// Fault injection is disabled by default and can only be enabled via explicit configuration.
//
// In this MVP, fault injection rules are strictly node-scoped (scope=node) and process-local.
// The Admin API binds the gateway's trusted local NodeID and rejects rules targeting any
// other node_id. Listings (GET), creations (POST), and deletions (DELETE) operate purely on
// the directly addressed gateway. Cluster-scope distributed fault coordination (cross-node
// fan-out, membership tracking, distributed TTL) is deferred to a future task.
package faults

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrDisabled    = errors.New("fault injection is disabled on this gateway")
	ErrNotFound    = errors.New("fault rule not found")
	ErrInvalidRule = errors.New("invalid fault rule")
)

type FaultType string

const (
	FaultS35xx       FaultType = "s3_5xx"
	FaultDDBThrottle FaultType = "ddb_throttle"
	// FaultKillNode simulates catastrophic node death by immediately draining admission
	// and failing health probes. Draining is intentionally permanent on the process; recovery
	// requires a gateway restart or supervisor reboot, mirroring real node failure.
	FaultKillNode    FaultType = "kill_node"
	FaultCorruptPack FaultType = "corrupt_pack"
)

func (t FaultType) Valid() bool {
	switch t {
	case FaultS35xx, FaultDDBThrottle, FaultKillNode, FaultCorruptPack:
		return true
	}
	return false
}

type FaultScope string

const (
	ScopeNode    FaultScope = "node"
	ScopeCluster FaultScope = "cluster" // cluster scope is rejected in MVP and deferred to multi-node task
)

func (s FaultScope) Valid() bool {
	return s == ScopeNode
}

type FaultRule struct {
	ID        string     `json:"fault_id"`
	Type      FaultType  `json:"type"`
	Scope     FaultScope `json:"scope"`
	NodeID    string     `json:"node_id,omitempty"`
	Rate      float64    `json:"rate"` // 0.0 to 1.0 probability
	Duration  int64      `json:"duration_seconds"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt time.Time  `json:"expires_at"`
}

func (r FaultRule) Validate() error {
	if !r.Type.Valid() {
		return fmt.Errorf("%w: invalid fault type", ErrInvalidRule)
	}
	if r.Scope == ScopeCluster {
		return fmt.Errorf("%w: cluster scope is not supported in single-node fault MVP; specify scope 'node' with node_id", ErrInvalidRule)
	}
	if r.Scope != ScopeNode {
		return fmt.Errorf("%w: invalid fault scope; must be 'node'", ErrInvalidRule)
	}
	if r.NodeID == "" {
		return fmt.Errorf("%w: node scope requires node_id", ErrInvalidRule)
	}
	if r.Rate <= 0.0 || r.Rate > 1.0 {
		return fmt.Errorf("%w: rate must be in (0.0, 1.0]", ErrInvalidRule)
	}
	if r.Duration <= 0 || r.Duration > 86400 {
		return fmt.Errorf("%w: duration must be between 1s and 86400s", ErrInvalidRule)
	}
	return nil
}

type Registry struct {
	mu      sync.RWMutex
	enabled bool
	rules   map[string]FaultRule
}

func NewRegistry(enabled bool) *Registry {
	return &Registry{
		enabled: enabled,
		rules:   make(map[string]FaultRule),
	}
}

func (reg *Registry) Enabled() bool {
	if reg == nil {
		return false
	}
	reg.mu.RLock()
	defer reg.mu.RUnlock()
	return reg.enabled
}

func (reg *Registry) SetEnabled(enabled bool) {
	if reg == nil {
		return
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	reg.enabled = enabled
	if !enabled {
		reg.rules = make(map[string]FaultRule)
	}
}

func newFaultID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return "flt-" + hex.EncodeToString(b)
}

func (reg *Registry) Add(now time.Time, rule FaultRule) (FaultRule, error) {
	if reg == nil {
		return FaultRule{}, ErrDisabled
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if !reg.enabled {
		return FaultRule{}, ErrDisabled
	}
	if err := rule.Validate(); err != nil {
		return FaultRule{}, err
	}
	if rule.ID == "" {
		rule.ID = newFaultID()
	}
	rule.CreatedAt = now.UTC()
	rule.ExpiresAt = now.Add(time.Duration(rule.Duration) * time.Second).UTC()
	reg.rules[rule.ID] = rule
	return rule, nil
}

func (reg *Registry) List(now time.Time) ([]FaultRule, error) {
	if reg == nil {
		return nil, ErrDisabled
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if !reg.enabled {
		return nil, ErrDisabled
	}
	// Expire old rules
	var active []FaultRule
	for id, r := range reg.rules {
		if now.After(r.ExpiresAt) {
			delete(reg.rules, id)
		} else {
			active = append(active, r)
		}
	}
	return active, nil
}

func (reg *Registry) Delete(id string) error {
	if reg == nil {
		return ErrDisabled
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if !reg.enabled {
		return ErrDisabled
	}
	if _, ok := reg.rules[id]; !ok {
		return ErrNotFound
	}
	delete(reg.rules, id)
	return nil
}

func (reg *Registry) ShouldInject(now time.Time, t FaultType, nodeID string) bool {
	if reg == nil {
		return false
	}
	reg.mu.Lock()
	defer reg.mu.Unlock()
	if !reg.enabled {
		return false
	}
	for id, r := range reg.rules {
		if now.After(r.ExpiresAt) {
			delete(reg.rules, id)
			continue
		}
		if r.Type == t && r.Scope == ScopeNode && r.NodeID == nodeID {
			if r.Rate >= 1.0 {
				return true
			}
			// Sample pseudo-random
			b := make([]byte, 2)
			rand.Read(b)
			val := float64(uint16(b[0])<<8|uint16(b[1])) / 65535.0
			return val < r.Rate
		}
	}
	return false
}
