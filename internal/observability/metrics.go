package observability

import (
	"fmt"
	"net/http"
	"runtime"
)

// metrics exports fixed process gauges and probe observations, never backend I/O
// or per-object/user labels. Mode makes simulated dev readiness explicit.
func (h *Health) metrics(w http.ResponseWriter, r *http.Request) {
	h.mu.RLock()
	checked, failures := h.checkedAt, h.failures
	h.mu.RUnlock()
	age := -1.0
	if !checked.IsZero() {
		age = max(0, h.now().Sub(checked).Seconds())
	}
	ready := 0
	if h.Ready() {
		ready = 1
	}
	draining := 0
	if h.draining != nil && h.draining() {
		draining = 1
	}
	mode := "runtime"
	if h.development {
		mode = "dev"
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `# HELP miles_process_start_time_seconds Process start time in Unix seconds.
# TYPE miles_process_start_time_seconds gauge
miles_process_start_time_seconds %d
# HELP miles_goroutines Current Go goroutine count.
# TYPE miles_goroutines gauge
miles_goroutines %d
# HELP miles_ready Cached readiness including independent drain state.
# TYPE miles_ready gauge
miles_ready{mode="%s"} %d
# HELP miles_draining Process has closed admission for shutdown.
# TYPE miles_draining gauge
miles_draining %d
# HELP miles_dependency_probe_age_seconds Last observation age; -1 means never checked.
# TYPE miles_dependency_probe_age_seconds gauge
miles_dependency_probe_age_seconds %g
# HELP miles_dependency_probe_failures_total Failed dependency or clock probes.
# TYPE miles_dependency_probe_failures_total counter
miles_dependency_probe_failures_total %d
`, h.started.Unix(), runtime.NumGoroutine(), mode, ready, draining, age, failures)
}
