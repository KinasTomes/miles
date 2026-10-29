package observability

import (
	"math"
	"sort"
	"sync"

	"github.com/KinasTomes/miles/internal/jobs"
)

type Metric struct {
	Name    string
	Value   float64
	Counter bool
	Labels  map[string]string
}
type Operations struct {
	mu      sync.Mutex
	updates map[string]uint64
}

func (o *Operations) JobUpdate(t jobs.Type, s jobs.State) {
	if o == nil || !t.Valid() || !s.Valid() {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.updates == nil {
		o.updates = map[string]uint64{}
	}
	k := string(t) + "/" + string(s)
	o.updates[k]++
}
func (o *Operations) Snapshot() []Metric {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	out := []Metric{}
	for _, t := range jobs.Types() {
		for _, s := range jobs.States() {
			k := string(t) + "/" + string(s)
			if n := o.updates[k]; n > 0 {
				out = append(out, Metric{Name: "miles_job_updates_total", Value: float64(n), Counter: true, Labels: map[string]string{"job_type": string(t), "state": string(s)}})
			}
		}
	}
	return out
}
func metricKey(m Metric) string {
	keys := make([]string, 0, len(m.Labels))
	for k := range m.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := m.Name
	for _, k := range keys {
		out += "/" + k + "=" + m.Labels[k]
	}
	return out
}

// Only fixed names and finite enum labels are eligible for either export path.
func validMetric(m Metric) bool {
	if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) || math.Abs(m.Value) > math.Exp2(360) {
		return false
	}
	switch m.Name {
	case "miles_process_start_time_seconds", "miles_goroutines", "miles_draining", "miles_dependency_probe_age_seconds", "miles_active_requests":
		return !m.Counter && len(m.Labels) == 0
	case "miles_dependency_probe_failures_total":
		return m.Counter && m.Value >= 0 && len(m.Labels) == 0
	case "miles_ready":
		return !m.Counter && len(m.Labels) == 1 && (m.Labels["mode"] == "dev" || m.Labels["mode"] == "runtime")
	case "miles_job_updates_total":
		return m.Counter && m.Value >= 0 && len(m.Labels) == 2 && jobs.Type(m.Labels["job_type"]).Valid() && jobs.State(m.Labels["state"]).Valid()
	}
	return false
}
