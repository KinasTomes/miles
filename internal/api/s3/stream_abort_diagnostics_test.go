package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

type faultInjectingReader struct {
	failBeforeHeaders bool
	failDuringStream  bool
	errToInject       error
	bytesEmitted      int
}

func (f *faultInjectingReader) GetObject(ctx context.Context, r object.GetRequest) (object.GetResult, error) {
	if f.failBeforeHeaders {
		return object.GetResult{}, f.errToInject
	}
	info := object.Info{
		Size: 8 * 1024 * 1024,
		ETag: `"sample-etag"`,
	}
	result := object.GetResult{Info: info}
	if r.Ready != nil {
		if err := r.Ready(result); err != nil {
			return result, err
		}
	}
	if f.failDuringStream {
		chunk := bytes.Repeat([]byte("A"), 1024*1024) // 1MB chunk
		if r.Emit != nil {
			if err := r.Emit(ctx, chunk); err != nil {
				return result, err
			}
			f.bytesEmitted += len(chunk)
		}
		return result, f.errToInject
	}
	if r.Emit != nil {
		chunk := bytes.Repeat([]byte("B"), 1024)
		if err := r.Emit(ctx, chunk); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (f *faultInjectingReader) Stat(ctx context.Context, r object.GetRequest) (object.GetResult, error) {
	return object.GetResult{Info: object.Info{Size: 8 * 1024 * 1024, ETag: `"sample-etag"`}}, nil
}

func TestGetObjectStreamAbortDiagnostics(t *testing.T) {
	tests := []struct {
		name              string
		failBeforeHeaders bool
		failDuringStream  bool
		err               error
		wantStage         string
		wantClass         string
		expectPanic       bool
	}{
		{
			name:             "renewal cause survives transport cancellation without leaking raw error",
			failDuringStream: true,
			err:              errors.Join(context.Canceled, fmt.Errorf("private-backend-message: %w", storage.ErrClockUncertain)),
			wantStage:        "object_get_stream",
			wantClass:        "clock_uncertainty_exceeded",
			expectPanic:      true,
		},
		{
			name:              "clock uncertainty during streaming after headers committed",
			failBeforeHeaders: false,
			failDuringStream:  true,
			err:               storage.ErrClockUncertain,
			wantStage:         "object_get_stream",
			wantClass:         "clock_uncertainty_exceeded",
			expectPanic:       true,
		},
		{
			name:              "fence lost midstream",
			failBeforeHeaders: false,
			failDuringStream:  true,
			err:               storage.ErrFenced,
			wantStage:         "object_get_stream",
			wantClass:         "fenced",
			expectPanic:       true,
		},
		{
			name:              "context canceled midstream",
			failBeforeHeaders: false,
			failDuringStream:  true,
			err:               context.Canceled,
			wantStage:         "object_get_stream",
			wantClass:         "canceled",
			expectPanic:       true,
		},
		{
			name:              "deadline exceeded midstream",
			failBeforeHeaders: false,
			failDuringStream:  true,
			err:               context.DeadlineExceeded,
			wantStage:         "object_get_stream",
			wantClass:         "deadline_exceeded",
			expectPanic:       true,
		},
		{
			name:              "clock uncertainty before headers",
			failBeforeHeaders: true,
			failDuringStream:  false,
			err:               storage.ErrClockUncertain,
			wantStage:         "object_get",
			wantClass:         "clock_uncertainty_exceeded",
			expectPanic:       false,
		},
		{
			name:              "backend unavailable before headers",
			failBeforeHeaders: true,
			failDuringStream:  false,
			err:               storage.ErrUnavailable,
			wantStage:         "object_get",
			wantClass:         "backend_unavailable",
			expectPanic:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			world := newBucketWorld(t)
			nodeID := "gateway-node-alpha"
			world.router.NodeID = nodeID

			reader := &faultInjectingReader{
				failBeforeHeaders: tc.failBeforeHeaders,
				failDuringStream:  tc.failDuringStream,
				errToInject:       tc.err,
			}
			world.router.Handlers = NewObjectHandlers(nil, reader, nil, nil)

			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)

			req := httptest.NewRequest(http.MethodGet, "/demo/secret-object-path", nil)
			sign(req, accessKey("owner"), "secret-owner")
			resp := httptest.NewRecorder()

			panicked := false
			var panicVal any
			func() {
				defer func() {
					if rec := recover(); rec != nil {
						panicked = true
						panicVal = rec
					}
				}()
				world.router.ServeHTTP(resp, req)
			}()

			reqID := resp.Header().Get("X-Amz-Request-Id")
			if reqID == "" {
				t.Fatal("X-Amz-Request-Id header missing from response")
			}

			if tc.expectPanic {
				if !panicked {
					t.Fatalf("expected panic(http.ErrAbortHandler) for streaming abort, but handler returned normally")
				}
				if panicVal != http.ErrAbortHandler {
					t.Fatalf("expected panic value http.ErrAbortHandler, got %v", panicVal)
				}
				// Verify partial body was emitted without trailing XML error
				if resp.Body.Len() == 0 {
					t.Fatalf("expected partial body bytes to be emitted prior to stream abort")
				}
				if strings.Contains(resp.Body.String(), "<Error>") {
					t.Fatalf("illegal XML error written to streaming response body: %s", resp.Body.String())
				}
			} else {
				if panicked {
					t.Fatalf("unexpected panic: %v", panicVal)
				}
				if resp.Code != http.StatusInternalServerError && resp.Code != http.StatusServiceUnavailable {
					t.Fatalf("unexpected status code for pre-header failure: %d", resp.Code)
				}
			}

			// Verify structured diagnostic log
			if strings.Contains(logs.String(), "private-backend-message") {
				t.Fatal("raw renewal failure leaked into diagnostics")
			}
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatalf("failed to parse structured JSON log: %v (raw log: %q)", err, logs.String())
			}

			wantMsg := "s3 server failure"
			if !tc.expectPanic && (tc.wantClass == "backend_unavailable" || tc.wantClass == "clock_uncertainty_exceeded") {
				wantMsg = "s3 service unavailable"
			}
			if entry["msg"] != wantMsg {
				t.Fatalf("expected log msg %q, got %v", wantMsg, entry["msg"])
			}
			if entry["request_id"] != reqID {
				t.Fatalf("expected request_id=%q, got %v", reqID, entry["request_id"])
			}
			if entry["node_id"] != nodeID {
				t.Fatalf("expected node_id=%q, got %v", nodeID, entry["node_id"])
			}
			if entry["operation"] != "GetObject" {
				t.Fatalf("expected operation=GetObject, got %v", entry["operation"])
			}
			if entry["stage"] != tc.wantStage {
				t.Fatalf("expected stage=%q, got %v", tc.wantStage, entry["stage"])
			}
			if entry["failure_class"] != tc.wantClass {
				t.Fatalf("expected failure_class=%q, got %v", tc.wantClass, entry["failure_class"])
			}

			// Verify privacy: no raw secret or object key in logs
			for _, secret := range []string{"secret-object-path", "secret-owner", "Authorization"} {
				if strings.Contains(logs.String(), secret) {
					t.Fatalf("sensitive information leaked into log: %s", secret)
				}
			}
		})
	}
}

type shortWriter struct {
	http.ResponseWriter
	limit int
	wrote int
}

func (s *shortWriter) Write(b []byte) (int, error) {
	if s.wrote >= s.limit {
		return 0, io.ErrClosedPipe
	}
	remaining := s.limit - s.wrote
	if len(b) > remaining {
		s.wrote += remaining
		return remaining, io.ErrClosedPipe
	}
	n, err := s.ResponseWriter.Write(b)
	s.wrote += n
	return n, err
}

func TestGetObjectBrokenPipeStreamAbort(t *testing.T) {
	world := newBucketWorld(t)
	nodeID := "gateway-node-pipe-test"
	world.router.NodeID = nodeID

	reader := &faultInjectingReader{
		failBeforeHeaders: false,
		failDuringStream:  false, // reader emits normally, writer cuts off
	}
	world.router.Handlers = NewObjectHandlers(nil, reader, nil, nil)

	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)

	req := httptest.NewRequest(http.MethodGet, "/demo/test-broken-pipe", nil)
	sign(req, accessKey("owner"), "secret-owner")
	rec := httptest.NewRecorder()
	sw := &shortWriter{ResponseWriter: rec, limit: 10}

	panicked := false
	var panicVal any
	func() {
		defer func() {
			if rec := recover(); rec != nil {
				panicked = true
				panicVal = rec
			}
		}()
		world.router.ServeHTTP(sw, req)
	}()

	if !panicked || panicVal != http.ErrAbortHandler {
		t.Fatalf("expected panic(http.ErrAbortHandler) on broken pipe, got panicked=%v, val=%v", panicked, panicVal)
	}

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log: %v (%q)", err, logs.String())
	}
	if entry["stage"] != "object_get_stream" {
		t.Fatalf("expected stage=object_get_stream, got %v", entry["stage"])
	}
	if entry["request_id"] != rec.Header().Get("X-Amz-Request-Id") {
		t.Fatalf("request_id mismatch")
	}
}
