package observability

import (
	"context"
	"net/http"
	"sync"
	"time"
)

const maxProbeAge = 5 * time.Second

// Health caches dependency/clock observations; HTTP requests never run probes.
// Draining is an independent process gate, not a replacement for R1 admission.
type Health struct {
	started     time.Time
	development bool
	failures    uint64
	mu          sync.RWMutex
	checkedAt   time.Time
	usable      bool
	probe       func(context.Context) error
	draining    func() bool
	now         func() time.Time
}

func NewHealth(probe func(context.Context) error, draining func() bool, development bool) *Health {
	return &Health{probe: probe, draining: draining, now: time.Now, started: time.Now(), development: development}
}
func (h *Health) Ready() bool {
	if h.draining != nil && h.draining() {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	age := h.now().Sub(h.checkedAt)
	return h.usable && !h.checkedAt.IsZero() && age >= 0 && age < maxProbeAge
}
func (h *Health) refresh(ctx context.Context) {
	start := h.now()
	check, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	usable := h.probe != nil && h.probe(check) == nil && check.Err() == nil
	h.mu.Lock()
	h.checkedAt = start
	h.usable = usable
	if !usable {
		h.failures++
	}
	h.mu.Unlock()
}

// Run performs bounded probes outside request handlers. Probe must honor ctx.
func (h *Health) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		h.refresh(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (h *Health) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { status(w, http.StatusOK, "ok\n") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !h.Ready() {
			status(w, http.StatusServiceUnavailable, "not ready\n")
			return
		}
		status(w, http.StatusOK, "ok\n")
	})
	mux.HandleFunc("GET /metrics", h.metrics)
	return mux
}
func status(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(body))
}
