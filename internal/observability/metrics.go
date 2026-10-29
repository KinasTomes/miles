package observability

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
)

var metricHelp = map[string]string{
	"miles_process_start_time_seconds":      "Process start time in Unix seconds.",
	"miles_goroutines":                      "Current Go goroutine count.",
	"miles_ready":                           "Cached readiness including independent drain state.",
	"miles_draining":                        "Process has closed admission for shutdown.",
	"miles_dependency_probe_age_seconds":    "Last observation age; -1 means never checked.",
	"miles_dependency_probe_failures_total": "Failed dependency or clock probes.",
	"miles_active_requests":                 "Currently admitted requests on this node.",
	"miles_job_updates_total":               "Acknowledged job record updates on this process, not cluster job counts.",
}

// Snapshot reads cached probes and bounded local counters. Both Prometheus and
// CloudWatch use it, so a scrape never performs backend/API operations.
func (h *Health) Snapshot() []Metric {
	h.mu.RLock()
	checked, failures, ops, active := h.checkedAt, h.failures, h.operations, h.active
	h.mu.RUnlock()
	age := -1.0
	if !checked.IsZero() {
		age = max(0, h.now().Sub(checked).Seconds())
	}
	ready, draining := 0.0, 0.0
	if h.Ready() {
		ready = 1
	}
	if h.draining != nil && h.draining() {
		draining = 1
	}
	mode := "runtime"
	if h.development {
		mode = "dev"
	}
	out := []Metric{
		{Name: "miles_process_start_time_seconds", Value: float64(h.started.Unix())},
		{Name: "miles_goroutines", Value: float64(runtime.NumGoroutine())},
		{Name: "miles_ready", Value: ready, Labels: map[string]string{"mode": mode}},
		{Name: "miles_draining", Value: draining},
		{Name: "miles_dependency_probe_age_seconds", Value: age},
		{Name: "miles_dependency_probe_failures_total", Value: float64(failures), Counter: true},
	}
	if active != nil {
		out = append(out, Metric{Name: "miles_active_requests", Value: float64(active())})
	}
	return append(out, ops.Snapshot()...)
}
func (h *Health) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	seen := map[string]bool{}
	for _, m := range h.Snapshot() {
		if !validMetric(m) {
			continue
		}
		if !seen[m.Name] {
			kind := "gauge"
			if m.Counter {
				kind = "counter"
			}
			fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n", m.Name, metricHelp[m.Name], m.Name, kind)
			seen[m.Name] = true
		}
		labels := make([]string, 0, len(m.Labels))
		for k, v := range m.Labels {
			labels = append(labels, fmt.Sprintf("%s=%q", k, v))
		}
		sort.Strings(labels)
		suffix := ""
		if len(labels) > 0 {
			suffix = "{" + strings.Join(labels, ",") + "}"
		}
		fmt.Fprintf(w, "%s%s %g\n", m.Name, suffix, m.Value)
	}
}
