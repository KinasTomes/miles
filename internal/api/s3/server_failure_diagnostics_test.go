package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

type failedAuthorization struct{}

func (failedAuthorization) Authorize(context.Context, string, string) (storage.Authorization, error) {
	return storage.Authorization{}, fmt.Errorf("private authorization backend: %w", storage.ErrUnavailable)
}

func TestServerFailureDiagnosticsTrackStageWithoutSecrets(t *testing.T) {
	tests := []struct {
		name, stage, class string
		status             int
		change             func(*Router)
	}{
		{"credential backend", "credential_lookup", "backend_unavailable", http.StatusInternalServerError,
			func(r *Router) { r.Credentials = unavailableCredentials{} }},
		{"authorization backend", "authorize", "backend_unavailable", http.StatusInternalServerError,
			func(r *Router) { r.Access = failedAuthorization{} }},
		{"object preflight", "object_lookup", "unknown", http.StatusInternalServerError,
			func(r *Router) {
				r.Handlers = NewObjectHandlers(&fakeObjectWriter{err: &object.PutStageError{Stage: "object_lookup", Err: errors.New("private object preflight marker")}}, nil, nil, nil)
			}},
		{"writer admission", "writer_admission", "backend_unavailable", http.StatusServiceUnavailable,
			func(r *Router) {
				r.Handlers = NewObjectHandlers(&fakeObjectWriter{err: &object.PutStageError{Stage: "writer_admission", Err: fmt.Errorf("private fenced call: %w", storage.ErrUnavailable)}}, nil, nil, nil)
			}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			world := newBucketWorld(t)
			world.router.NodeID = "cloud-test-node-a"
			tc.change(&world.router)
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)
			req := httptest.NewRequest(http.MethodPut, "/demo/private-object-name", strings.NewReader("body"))
			sign(req, accessKey("owner"), "secret-owner")
			resp := httptest.NewRecorder()
			world.router.ServeHTTP(resp, req)
			if resp.Code != tc.status {
				t.Fatalf("response=%d want=%d", resp.Code, tc.status)
			}
			var fields map[string]any
			if err := json.Unmarshal(logs.Bytes(), &fields); err != nil {
				t.Fatalf("missing diagnostic: %v (%q)", err, logs.String())
			}
			wantMessage := "s3 server failure"
			if tc.status == http.StatusServiceUnavailable {
				wantMessage = "s3 service unavailable"
			}
			if fields["msg"] != wantMessage || fields["stage"] != tc.stage || fields["failure_class"] != tc.class ||
				fields["request_id"] != resp.Header().Get("X-Amz-Request-Id") || fields["node_id"] != "cloud-test-node-a" ||
				fields["operation"] != string(PutObject) {
				t.Fatalf("incorrect bounded diagnostic: %v", fields)
			}
			for _, secret := range []string{"private-object-name", "secret-owner", "private object preflight marker", "private fenced call", "private authorization backend", "Authorization", "private-session"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("sensitive detail leaked into log: %s", secret)
				}
			}
			if strings.Contains(resp.Body.String(), "backend_unavailable") || strings.Contains(resp.Body.String(), "private object preflight marker") {
				t.Fatal("server diagnostic leaked into public response")
			}
		})
	}
}

func TestSafeFailureClassPreservesErrorsIs(t *testing.T) {
	if got := safeFailureClass(fmt.Errorf("private: %w", storage.ErrUnavailable)); got != "backend_unavailable" {
		t.Fatal(got)
	}
	wrapped := &object.PutStageError{Stage: "object_lookup", Err: storage.ErrUnavailable}
	if !errors.Is(wrapped, storage.ErrUnavailable) || safeFailureClass(wrapped) != "backend_unavailable" {
		t.Fatal(wrapped)
	}
}
