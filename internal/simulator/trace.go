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
	AtNS         int64                           `json:"at_ns"`
	Snapshot     *cost.Snapshot                  `json:"snapshot,omitempty"`
	Counters     cost.WriteCounters              `json:"write_counters"`
	Traffic      map[cost.Route]uint64           `json:"traffic_bytes"`
	Usage        []cost.Usage                    `json:"usage"`
	StorageBytes map[storage.StorageClass]uint64 `json:"billable_storage_bytes"`
	Tiering      *Tiering                        `json:"tiering,omitempty"`
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
		if event.AtNS < last || event.AtNS > t.DurationNS || len(event.Usage) > 16 || len(event.Traffic) > 8 || len(event.StorageBytes) > 4 {
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
