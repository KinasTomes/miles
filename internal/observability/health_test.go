package observability

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthReadinessAndCache(t *testing.T) {
	now := time.Now()
	calls := 0
	broken := false
	draining := false
	h := NewHealth(func(context.Context) error {
		calls++
		if broken {
			return errors.New("secret backend detail")
		}
		return nil
	}, func() bool { return draining }, true)
	h.now = func() time.Time { return now }
	handler := h.Handler()
	get := func(path string, want int) {
		t.Helper()
		r := httptest.NewRecorder()
		handler.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != want {
			t.Fatalf("%s=%d want %d", path, r.Code, want)
		}
		if r.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
	}
	get("/healthz", 200)
	get("/readyz", 503)
	if calls != 0 {
		t.Fatal("request invoked probe")
	}
	h.refresh(context.Background())
	for range 10 {
		get("/healthz", 200)
		get("/readyz", 200)
	}
	if calls != 1 {
		t.Fatal("probes not cached")
	}
	now = now.Add(maxProbeAge)
	get("/readyz", 503)
	get("/healthz", 200)
	h.refresh(context.Background())
	get("/readyz", 200)
	draining = true
	get("/readyz", 503)
	draining = false
	broken = true
	h.refresh(context.Background())
	get("/readyz", 503)
	get("/healthz", 200)
	r := httptest.NewRecorder()
	handler.ServeHTTP(r, httptest.NewRequest("POST", "/healthz", nil))
	if r.Code != 405 {
		t.Fatal(r.Code)
	}
	if NewHealth(nil, nil, false).Ready() {
		t.Fatal("missing proof must fail closed")
	}
}
func TestCanceledProbeCannotPublishReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	h := NewHealth(func(context.Context) error { cancel(); return nil }, nil, true)
	h.refresh(ctx)
	if h.Ready() {
		t.Fatal("canceled observation published readiness")
	}
}
