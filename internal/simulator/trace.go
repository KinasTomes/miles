// Package simulator replays explicit accounting observations on the shared Clock.
// It models neither AWS execution nor production R1 permission.
package simulator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/simulator/workloads"
	"github.com/KinasTomes/miles/internal/storage"
)

const MaxTraceBytes = 8 << 20
const MaxEvents = 4096

type Trace struct {
	SchemaVersion  int             `json:"schema_version"`
	Name           string          `json:"name"`
	Kind           string          `json:"kind"` // synthetic or observed-accounting
	Region         string          `json:"region"`
	Profile        string          `json:"profile"`
	Metadata       string          `json:"metadata_backend"`
	Topology       string          `json:"topology"`
	Start          time.Time       `json:"start"`
	DurationNS     int64           `json:"duration_ns"`
	BytesPerGB     uint64          `json:"bytes_per_gb"`
	MonthSeconds   int64           `json:"storage_month_seconds"`
	PolicyRevision uint64          `json:"policy_revision"`
	Policy         json.RawMessage `json:"policy"`
	Omissions      []string        `json:"omissions"`
	Events         []Event         `json:"events"`
}

type Event struct {
	AtNS                        int64                           `json:"at_ns"`
	Snapshot                    *cost.Snapshot                  `json:"snapshot,omitempty"`
	Counters                    cost.WriteCounters              `json:"write_counters"`
	Traffic                     map[cost.Route]uint64           `json:"traffic_bytes"`
	Usage                       []cost.Usage                    `json:"usage"`
	DirectUsage                 []cost.Usage                    `json:"direct_usage,omitempty"`
	StorageBytes                map[storage.StorageClass]uint64 `json:"billable_storage_bytes"`
	Tiering                     *Tiering                        `json:"tiering,omitempty"`
	ColdLogicalDeliveredBytes   uint64                          `json:"cold_logical_delivered_bytes,omitempty"`
	ColdLogicalDeliveredByClass map[storage.StorageClass]uint64 `json:"cold_logical_delivered_by_class,omitempty"`
}

// ClientTrafficBytes returns total outbound traffic delivered to client measured in this event.
// Uses checked addition to prevent uint64 wrap-around on invalid telemetry.
// Returns (traffic, ok) where ok is false if uint64 overflow occurs.
func (e *Event) ClientTrafficBytes() (uint64, bool) {
	if e == nil || e.Traffic == nil {
		return 0, true
	}
	gw := e.Traffic[cost.GatewayClient]
	s3 := e.Traffic[cost.S3Client]
	if s3 > ^uint64(0)-gw {
		return 0, false // overflow detected
	}
	return gw + s3, true
}

// TotalColdDeliveredBytes returns the total claimed cold delivered bytes across all cold classes in this event.
// When class-specific telemetry is provided, it sums the bytes across all classes in the map with checked addition.
// Otherwise, it falls back to the scalar ColdLogicalDeliveredBytes.
// Returns (total, ok) where ok is false if uint64 overflow occurs.
func (e *Event) TotalColdDeliveredBytes() (uint64, bool) {
	if e == nil {
		return 0, true
	}
	if len(e.ColdLogicalDeliveredByClass) > 0 {
		var total uint64
		for _, b := range e.ColdLogicalDeliveredByClass {
			if b > ^uint64(0)-total {
				return 0, false // overflow detected
			}
			total += b
		}
		return total, true
	}
	return e.ColdLogicalDeliveredBytes, true
}

// ColdDeliveredForClass retrieves the proven logical delivered bytes for a specific cold storage class.
// Telemetry precedence and fallback rules:
//  1. Authoritative class-specific telemetry: If ColdLogicalDeliveredByClass is provided (non-empty),
//     it is the sole authoritative source of truth. If the queried class exists in the map with bytes > 0,
//     it returns (bytes, true). If the class is missing from the map or has 0 bytes, it returns (0, false).
//     It NEVER falls back to scalar ColdLogicalDeliveredBytes when class-specific telemetry is present.
//  2. Scalar fallback: If ColdLogicalDeliveredByClass is absent or empty, it falls back to
//     ColdLogicalDeliveredBytes only when ColdLogicalDeliveredBytes > 0.
func (e *Event) ColdDeliveredForClass(class storage.StorageClass) (uint64, bool) {
	if e == nil {
		return 0, false
	}
	if len(e.ColdLogicalDeliveredByClass) > 0 {
		if b, ok := e.ColdLogicalDeliveredByClass[class]; ok && b > 0 {
			return b, true
		}
		return 0, false
	}
	if e.ColdLogicalDeliveredBytes > 0 {
		return e.ColdLogicalDeliveredBytes, true
	}
	return 0, false
}

func (e *Event) UnmarshalJSON(data []byte) error {
	type eventAlias struct {
		AtNS                        int64                           `json:"at_ns"`
		Snapshot                    *cost.Snapshot                  `json:"snapshot,omitempty"`
		Counters                    cost.WriteCounters              `json:"write_counters"`
		Traffic                     map[cost.Route]uint64           `json:"traffic_bytes"`
		Usage                       []cost.Usage                    `json:"usage"`
		DirectUsage                 []cost.Usage                    `json:"direct_usage,omitempty"`
		StorageBytes                map[storage.StorageClass]uint64 `json:"billable_storage_bytes"`
		Tiering                     *Tiering                        `json:"tiering,omitempty"`
		ColdLogicalDeliveredRaw     json.RawMessage                 `json:"cold_logical_delivered_bytes,omitempty"`
		ColdLogicalDeliveredByClass map[storage.StorageClass]uint64 `json:"cold_logical_delivered_by_class,omitempty"`
	}

	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	var raw eventAlias
	if err := d.Decode(&raw); err != nil {
		return err
	}

	e.AtNS = raw.AtNS
	e.Snapshot = raw.Snapshot
	e.Counters = raw.Counters
	e.Traffic = raw.Traffic
	e.Usage = raw.Usage
	e.DirectUsage = raw.DirectUsage
	e.StorageBytes = raw.StorageBytes
	e.Tiering = raw.Tiering
	e.ColdLogicalDeliveredByClass = raw.ColdLogicalDeliveredByClass

	if len(raw.ColdLogicalDeliveredRaw) > 0 && string(raw.ColdLogicalDeliveredRaw) != "null" {
		var num uint64
		if err := json.Unmarshal(raw.ColdLogicalDeliveredRaw, &num); err == nil {
			e.ColdLogicalDeliveredBytes = num
			if len(e.ColdLogicalDeliveredByClass) > 0 {
				if len(e.ColdLogicalDeliveredByClass) == 1 {
					for _, b := range e.ColdLogicalDeliveredByClass {
						if b != num {
							return fmt.Errorf("conflicting cold delivery telemetry: scalar %d != class delivery %d", num, b)
						}
					}
				} else {
					var sum uint64
					for _, b := range e.ColdLogicalDeliveredByClass {
						if b > ^uint64(0)-sum {
							return errors.New("cold delivery telemetry overflow")
						}
						sum += b
					}
					if sum != num {
						return fmt.Errorf("conflicting cold delivery telemetry: scalar %d != sum of class delivery %d", num, sum)
					}
				}
			}
			return nil
		}
		var classMap map[storage.StorageClass]uint64
		if err := json.Unmarshal(raw.ColdLogicalDeliveredRaw, &classMap); err == nil {
			if e.ColdLogicalDeliveredByClass == nil {
				e.ColdLogicalDeliveredByClass = classMap
			} else {
				for k, v := range classMap {
					if existing, ok := e.ColdLogicalDeliveredByClass[k]; ok && existing != v {
						return fmt.Errorf("conflicting cold delivery telemetry for class %s: %d != %d", k, existing, v)
					}
					e.ColdLogicalDeliveredByClass[k] = v
				}
			}
			return nil
		}
		return errors.New("invalid cold_logical_delivered_bytes: must be a number or a map of storage class to bytes")
	}
	return nil
}

// These facts are trace assumptions, never proof of a live operation's safety.
type Tiering struct {
	Pack             storage.PackInfo `json:"pack"`
	LiveBytes        int64            `json:"live_bytes"`
	TotalBytes       int64            `json:"total_bytes"`
	Metadata         policy.Fact      `json:"metadata"`
	Authority        policy.Fact      `json:"authority"`
	SafeToTransition policy.Fact      `json:"safe_to_transition"`
}

func Read(r io.Reader) (Trace, error) {
	var t Trace
	data, err := io.ReadAll(io.LimitReader(r, MaxTraceBytes+1))
	if err != nil || len(data) > MaxTraceBytes {
		return t, errors.New("trace cannot be read or exceeds 8 MiB")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&t); err != nil {
		return Trace{}, fmt.Errorf("trace JSON: %w", err)
	}
	if d.Decode(new(any)) != io.EOF {
		return Trace{}, errors.New("trace must contain one JSON document")
	}
	_, err = t.snapshot()
	if err != nil {
		return Trace{}, err
	}
	return t, nil
}

func (t Trace) snapshot() (config.GlobalSnapshot, error) {
	if t.SchemaVersion != 1 || t.Name == "" || len(t.Name) > 128 || (t.Kind != "synthetic" && t.Kind != "observed-accounting") || t.Region == "" || (t.Profile != "cloud" && t.Profile != "edge") || (t.Metadata != "badger" && t.Metadata != "dynamodb" && t.Metadata != "dynamodb-local" && t.Metadata != "none") || t.Topology == "" || len(t.Topology) > 1024 || t.Start.IsZero() || t.Start.Year() < 1 || t.Start.Year() > 9999 || t.DurationNS < 0 || t.BytesPerGB == 0 || t.BytesPerGB > 1<<40 || t.MonthSeconds < 1 || t.MonthSeconds > 366*86400 || len(t.Omissions) == 0 || len(t.Omissions) > 64 || len(t.Events) == 0 || len(t.Events) > MaxEvents {
		return config.GlobalSnapshot{}, errors.New("invalid trace envelope; explicit backend, topology, units, policy and omissions required")
	}
	for _, omission := range t.Omissions {
		if omission == "" || len(omission) > 1024 {
			return config.GlobalSnapshot{}, errors.New("invalid trace omission")
		}
	}
	last := int64(0)
	for _, event := range t.Events {
		if event.AtNS < last || event.AtNS > t.DurationNS || len(event.Usage) > 16 || len(event.Traffic) > 8 || len(event.StorageBytes) > 4 || len(event.ColdLogicalDeliveredByClass) > 4 {
			return config.GlobalSnapshot{}, errors.New("trace order, timestamp or event bound invalid")
		}
		last = event.AtNS
	}
	v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		return config.GlobalSnapshot{}, err
	}
	return v.DecodeGlobal(storage.PolicySnapshot{Version: storage.ModelVersion, Revision: t.PolicyRevision, Document: t.Policy})
}

// RestoreDuration returns the proven duration that a restored copy of a Glacier pack remains active.
// Precedence:
//  1. Authoritative pack/operation metadata: If event.Tiering has a non-zero Pack.RestoredUntil and
//     t.Start is set, the duration is calculated from the event's timestamp (t.Start + event.AtNS)
//     to RestoredUntil. If RestoredUntil is strictly after event timestamp, it returns (duration, true).
//     If RestoredUntil is at or before event timestamp, it returns (0, false) (already expired).
//  2. Policy auto_restore_days: If policy specifies tiering.auto_restore_days > 0, the duration is
//     auto_restore_days * 24 hours, returning (duration, true).
//  3. If neither pack metadata nor policy evidence provides a valid positive restore duration,
//     it returns (0, false), indicating expiry duration is unproven.
func (t Trace) RestoreDuration(event Event) (time.Duration, bool) {
	if event.Tiering != nil && !event.Tiering.Pack.RestoredUntil.IsZero() && !t.Start.IsZero() {
		eventTime := t.Start.Add(time.Duration(event.AtNS))
		if event.Tiering.Pack.RestoredUntil.After(eventTime) {
			return event.Tiering.Pack.RestoredUntil.Sub(eventTime), true
		}
		return 0, false
	}
	if len(t.Policy) > 0 {
		var p struct {
			Tiering struct {
				AutoRestoreDays int64 `json:"auto_restore_days"`
			} `json:"tiering"`
		}
		if err := json.Unmarshal(t.Policy, &p); err == nil && p.Tiering.AutoRestoreDays > 0 {
			return time.Duration(p.Tiering.AutoRestoreDays) * 24 * time.Hour, true
		}
	}
	return 0, false
}

// EmbeddedWorkload returns the embedded demo workload trace bytes for the given name or workload ID.
func EmbeddedWorkload(name string) []byte {
	return workloads.Get(name)
}
