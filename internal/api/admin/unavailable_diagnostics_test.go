package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestAdminUnavailableDiagnosticsPreserveResponseAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, class string
		cause       error
	}{
		{"r1-busy", "r1_gate_busy", storage.ErrR1Busy},
		{"fenced", "fenced", storage.ErrFenced},
		{"backend", "backend_unavailable", storage.ErrUnavailable},
		{"deadline", "deadline_exceeded", context.DeadlineExceeded},
		{"canceled", "canceled", context.Canceled},
		{"unclassified-api", "unknown", &apiError{http.StatusServiceUnavailable, "unavailable", "unavailable"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			svc := &server{Options: Options{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}}
			req := httptest.NewRequest(http.MethodPost, "/admin/v1/users/private-name?token=PRIVATE_QUERY", nil)
			req.Pattern = "POST /admin/v1/users"
			recorder := httptest.NewRecorder()
			call := &call{w: recorder, r: req, id: "fixed-request-id"}
			svc.fail(call, fmt.Errorf("private_backend_details=DO_NOT_LOG: %w", tc.cause))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d", recorder.Code)
			}
			var response struct {
				Error struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					RequestID string `json:"request_id"`
				}
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Code != "unavailable" || response.Error.RequestID != call.id {
				t.Fatalf("error mapping or request id changed: %+v", response.Error)
			}
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("structured admin log missing: %v", err)
			}
			if entry["msg"] != "admin service unavailable" || entry["failure_class"] != tc.class ||
				entry["request_id"] != call.id || entry["action"] != req.Pattern {
				t.Fatalf("bad diagnostic log: %v", entry)
			}
			if strings.Contains(logs.String(), "DO_NOT_LOG") || strings.Contains(logs.String(), "private-name") ||
				strings.Contains(logs.String(), "PRIVATE_QUERY") || strings.Contains(recorder.Body.String(), tc.class) {
				t.Fatal("internal error, path/query or class leaked outside expected fields")
			}
		})
	}
}
