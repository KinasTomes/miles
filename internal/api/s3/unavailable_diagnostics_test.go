package s3

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestUnavailableDiagnosticsKeepResponseAndLogsPrivate(t *testing.T) {
	for _, tc := range []struct {
		name, method, class string
		cause               error
	}{
		{"r1-busy-put", http.MethodPut, "r1_gate_busy", storage.ErrR1Busy},
		{"fenced-put", http.MethodPut, "fenced", storage.ErrFenced},
		{"backend-put", http.MethodPut, "backend_unavailable", storage.ErrUnavailable},
		{"backend-get", http.MethodGet, "backend_unavailable", storage.ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := newBucketWorld(t)
			cause := fmt.Errorf("private_backend_details=DO_NOT_LOG: %w", tc.cause)
			if tc.method == http.MethodPut {
				world.router.Handlers = NewObjectHandlers(&fakeObjectWriter{err: cause}, &fakeObjectReader{}, nil, nil)
			} else {
				world.router.Handlers = NewObjectHandlers(&fakeObjectWriter{}, &fakeObjectReader{getErr: cause}, nil, nil)
			}
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(previous)

			var body *strings.Reader
			if tc.method == http.MethodPut {
				body = strings.NewReader("body")
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tc.method, "/demo/private-object-name", body)
			sign(req, accessKey("owner"), "secret-owner")
			recorder := httptest.NewRecorder()
			world.router.ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable || checkErrorBody(t, recorder) != ErrServiceUnavailable {
				t.Fatalf("S3 response changed: status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("structured S3 availability log missing: %v", err)
			}
			if entry["msg"] != "s3 service unavailable" || entry["failure_class"] != tc.class ||
				entry["request_id"] != recorder.Header().Get("X-Amz-Request-Id") || entry["method"] != tc.method {
				t.Fatalf("bad safe provenance log: %v", entry)
			}
			if strings.Contains(logs.String(), "DO_NOT_LOG") || strings.Contains(logs.String(), "private-object-name") ||
				strings.Contains(logs.String(), "secret-owner") || strings.Contains(recorder.Body.String(), tc.class) {
				t.Fatal("internal cause, object key, credential or failure class leaked")
			}
		})
	}
}

func TestMetadataWriteUnavailableClassification(t *testing.T) {
	for _, tc := range []struct {
		cause error
		class string
	}{
		{storage.ErrR1Busy, "r1_gate_busy"},
		{storage.ErrFenced, "fenced"},
		{storage.ErrUnavailable, "backend_unavailable"},
	} {
		got := MetadataWriteError(fmt.Errorf("private: %w", tc.cause))
		if got.Code != ErrServiceUnavailable || got.failureClass != tc.class || got.Error() != "ServiceUnavailable: Service is unable to handle request." {
			t.Fatalf("wrong metadata 503 classification: %#v", got)
		}
	}
}
