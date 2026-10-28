package readpath

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// WarmMetrics has fixed-cardinality counters; it never exposes object/user IDs.
// Mount this handler on the runtime metrics mux. A mid-stream error is observed
// here and returned to the HTTP consumer, which must terminate delivery.
type WarmMetrics struct{ requests, aborted, integrity, hits, misses, emitted, cacheErrors, accessStatsErrors, backendBytes atomic.Uint64 }
type WarmCounters struct{ Requests, Aborted, IntegrityFailures, CacheHits, CacheMisses, EmittedBytes, CacheErrors, AccessStatsErrors, BackendBytes uint64 }

func (m *WarmMetrics) Snapshot() WarmCounters {
	return WarmCounters{m.requests.Load(), m.aborted.Load(), m.integrity.Load(), m.hits.Load(), m.misses.Load(), m.emitted.Load(), m.cacheErrors.Load(), m.accessStatsErrors.Load(), m.backendBytes.Load()}
}
func (m *WarmMetrics) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	s := m.Snapshot()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	for _, c := range []struct {
		name  string
		value uint64
	}{{"requests", s.Requests}, {"aborted", s.Aborted}, {"integrity_failures", s.IntegrityFailures}, {"cache_hits", s.CacheHits}, {"cache_misses", s.CacheMisses}, {"emitted_bytes", s.EmittedBytes}, {"cache_errors", s.CacheErrors}, {"access_stats_errors", s.AccessStatsErrors}, {"backend_bytes", s.BackendBytes}} {
		fmt.Fprintf(w, "# TYPE miles_warm_read_%s_total counter\nmiles_warm_read_%s_total %d\n", c.name, c.name, c.value)
	}
}
