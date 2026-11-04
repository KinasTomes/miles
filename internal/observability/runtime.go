package observability

import (
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/readpath"
)

// Runtime owns fixed-cardinality process observations, never authority or billing.
// Gauges come from bounded background reads; Snapshot performs no network I/O.
type Runtime struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]float64
	observed time.Time
	Warm     readpath.WarmMetrics
}

var runtimeCounters = []string{
	"miles_ddb_calls_total", "miles_ddb_transactions_total", "miles_ddb_call_seconds_total",
	"miles_ddb_consumed_units_total", "miles_ddb_read_units_total", "miles_ddb_write_units_total", "miles_ddb_capacity_responses_total",
	"miles_ddb_lease_write_attempts_total", "miles_ddb_throttles_total", "miles_ddb_retries_total",
	"miles_metadata_request_body_bytes_total", "miles_metadata_response_body_bytes_total",
	"miles_gateway_s3_request_body_bytes_total", "miles_gateway_s3_response_body_bytes_total",
	"miles_client_gateway_body_bytes_total", "miles_gateway_client_body_bytes_total",
	"miles_runtime_observation_failures_total",
}
var runtimeGauges = []string{
	"miles_recovery_cut_age_seconds", "miles_r1_gate_closed_seconds",
	"miles_stuck_completing_uploads", "miles_journal_export_lag_seconds",
}

func (m *Runtime) add(name string, n float64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counters == nil {
		m.counters = map[string]float64{}
	}
	m.counters[name] += n
}
func (m *Runtime) Retry()              { m.add("miles_ddb_retries_total", 1) }
func (m *Runtime) ObservationFailure() { m.add("miles_runtime_observation_failures_total", 1) }
func (m *Runtime) Observe(at time.Time, cutAge, gateDuration, exportLag float64, stuck int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observed = at
	m.gauges = map[string]float64{
		"miles_recovery_cut_age_seconds":   max(0, cutAge),
		"miles_r1_gate_closed_seconds":     max(0, gateDuration),
		"miles_stuck_completing_uploads":   float64(stuck),
		"miles_journal_export_lag_seconds": max(0, exportLag),
	}
}
func (m *Runtime) Snapshot(at time.Time) []Metric {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Metric, 0, len(runtimeCounters)+len(runtimeGauges)+6)
	for _, name := range runtimeCounters {
		out = append(out, Metric{Name: name, Value: m.counters[name], Counter: true})
	}
	age := -1.0
	if !m.observed.IsZero() {
		age = max(0, at.Sub(m.observed).Seconds())
	}
	out = append(out, Metric{Name: "miles_runtime_observation_age_seconds", Value: age})
	if age >= 0 && age < 60 {
		for _, name := range runtimeGauges {
			value := m.gauges[name]
			out = append(out, Metric{Name: name, Value: value})
		}
	}
	warm := m.Warm.Snapshot()
	for _, v := range []struct {
		name string
		n    uint64
	}{
		{"requests", warm.Requests}, {"aborted", warm.Aborted}, {"integrity_failures", warm.IntegrityFailures},
		{"cache_hits", warm.CacheHits}, {"cache_misses", warm.CacheMisses},
	} {
		out = append(out, Metric{Name: "miles_warm_read_" + v.name + "_total", Value: float64(v.n), Counter: true})
	}
	return out
}
func validRuntimeMetric(m Metric) bool {
	if len(m.Labels) != 0 {
		return false
	}
	if m.Name == "miles_runtime_observation_age_seconds" {
		return !m.Counter && m.Value >= -1
	}
	for _, name := range runtimeCounters {
		if name == m.Name {
			return m.Counter && m.Value >= 0
		}
	}
	for _, name := range runtimeGauges {
		if name == m.Name {
			return !m.Counter && m.Value >= 0
		}
	}
	for _, suffix := range []string{"requests", "aborted", "integrity_failures", "cache_hits", "cache_misses"} {
		if m.Name == "miles_warm_read_"+suffix+"_total" {
			return m.Counter && m.Value >= 0
		}
	}
	return false
}
