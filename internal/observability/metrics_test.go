package observability

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsDoNotProbeAndExposeFixedLabels(t *testing.T) {
	calls := 0
	broken := false
	draining := false
	h := NewHealth(func(context.Context) error {
		calls++
		if broken {
			return errors.New("private-secret-object-key")
		}
		return nil
	}, func() bool { return draining }, true)
	now := time.Now()
	h.now = func() time.Time { return now }
	scrape := func() string {
		t.Helper()
		r := httptest.NewRecorder()
		h.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/metrics?object=private-secret-object-key", nil))
		if r.Code != 200 {
			t.Fatal(r.Code)
		}
		if r.Header().Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
			t.Fatal("invalid exposition type")
		}
		return r.Body.String()
	}
	if body := scrape(); !strings.Contains(body, "miles_dependency_probe_age_seconds -1") {
		t.Fatal(body)
	}
	if calls != 0 {
		t.Fatal("scrape called backend")
	}
	h.refresh(context.Background())
	for range 10 {
		body := scrape()
		if !strings.Contains(body, `miles_ready{mode="dev"} 1`) || strings.Contains(body, "private-secret") {
			t.Fatal(body)
		}
	}
	if calls != 1 {
		t.Fatal("metrics caused extra probes")
	}
	draining = true
	if !strings.Contains(scrape(), "miles_draining 1") {
		t.Fatal("missing drain")
	}
	draining = false
	now = now.Add(maxProbeAge)
	if !strings.Contains(scrape(), `miles_ready{mode="dev"} 0`) {
		t.Fatal("stale success exported ready")
	}
	broken = true
	h.refresh(context.Background())
	if !strings.Contains(scrape(), "miles_dependency_probe_failures_total 1") {
		t.Fatal("missing failure")
	}
}
