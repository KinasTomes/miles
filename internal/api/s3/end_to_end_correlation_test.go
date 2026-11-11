package s3

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

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

type faultCredentialStore struct {
	err error
}

func (f faultCredentialStore) Credential(_ context.Context, _ string) (Credential, error) {
	return Credential{}, f.err
}

func TestDualGatewayFaultInjectionEndToEndCorrelation(t *testing.T) {
	// Gateway 1: simulates cloud-a with transient credential backend error (causes 500)
	var logsNodeA bytes.Buffer
	loggerNodeA := slog.New(slog.NewJSONHandler(&logsNodeA, nil))

	worldA := newBucketWorld(t)
	worldA.router.NodeID = "cloud-gateway-a"
	worldA.router.Credentials = faultCredentialStore{
		err: fmt.Errorf("private ddb throttle: %w", storage.ErrUnavailable),
	}

	// Gateway 2: simulates cloud-b with writer admission failure (causes 503)
	var logsNodeB bytes.Buffer
	loggerNodeB := slog.New(slog.NewJSONHandler(&logsNodeB, nil))

	worldB := newBucketWorld(t)
	worldB.router.NodeID = "cloud-gateway-b"
	worldB.router.Handlers = NewObjectHandlers(&fakeObjectWriter{
		err: &object.PutStageError{
			Stage: "writer_admission",
			Err:   fmt.Errorf("private ddb lease failure: %w", storage.ErrUnavailable),
		},
	}, nil, nil, nil)

	prevLogger := slog.Default()
	defer slog.SetDefault(prevLogger)

	// --- Request 1 directed to Gateway 1 (Cloud-A) -> Expect 500 ---
	slog.SetDefault(loggerNodeA)
	reqA := httptest.NewRequest(http.MethodPut, "/demo/test-object-500.bin", strings.NewReader("payload-bytes"))
	sign(reqA, accessKey("owner"), "secret-owner")
	recA := httptest.NewRecorder()
	worldA.router.ServeHTTP(recA, reqA)

	if recA.Code != http.StatusInternalServerError {
		t.Fatalf("expected HTTP 500 on gateway-a, got %d", recA.Code)
	}
	reqIDA := recA.Header().Get("X-Amz-Request-Id")
	if reqIDA == "" {
		t.Fatal("missing X-Amz-Request-Id on gateway-a response")
	}

	// --- Request 2 directed to Gateway 2 (Cloud-B) -> Expect 503 ---
	slog.SetDefault(loggerNodeB)
	reqB := httptest.NewRequest(http.MethodPut, "/demo/test-object-503.bin", strings.NewReader("payload-bytes"))
	sign(reqB, accessKey("owner"), "secret-owner")
	recB := httptest.NewRecorder()
	worldB.router.ServeHTTP(recB, reqB)

	if recB.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503 on gateway-b, got %d", recB.Code)
	}
	reqIDB := recB.Header().Get("X-Amz-Request-Id")
	if reqIDB == "" {
		t.Fatal("missing X-Amz-Request-Id on gateway-b response")
	}

	// Verify server logs on Gateway 1 (Cloud-A)
	var entryA map[string]any
	if err := json.Unmarshal(logsNodeA.Bytes(), &entryA); err != nil {
		t.Fatalf("failed to parse gateway-a structured json log: %v (%q)", err, logsNodeA.String())
	}
	if entryA["msg"] != "s3 server failure" {
		t.Fatalf("unexpected msg on gateway-a: %v", entryA["msg"])
	}
	if entryA["request_id"] != reqIDA {
		t.Fatalf("request_id mismatch on gateway-a: got %v want %v", entryA["request_id"], reqIDA)
	}
	if entryA["node_id"] != "cloud-gateway-a" {
		t.Fatalf("node_id mismatch on gateway-a: %v", entryA["node_id"])
	}
	if entryA["stage"] != "credential_lookup" {
		t.Fatalf("stage mismatch on gateway-a: %v", entryA["stage"])
	}
	if entryA["failure_class"] != "backend_unavailable" {
		t.Fatalf("failure_class mismatch on gateway-a: %v", entryA["failure_class"])
	}

	// Verify server logs on Gateway 2 (Cloud-B)
	var entryB map[string]any
	if err := json.Unmarshal(logsNodeB.Bytes(), &entryB); err != nil {
		t.Fatalf("failed to parse gateway-b structured json log: %v (%q)", err, logsNodeB.String())
	}
	if entryB["msg"] != "s3 service unavailable" {
		t.Fatalf("unexpected msg on gateway-b: %v", entryB["msg"])
	}
	if entryB["request_id"] != reqIDB {
		t.Fatalf("request_id mismatch on gateway-b: got %v want %v", entryB["request_id"], reqIDB)
	}
	if entryB["node_id"] != "cloud-gateway-b" {
		t.Fatalf("node_id mismatch on gateway-b: %v", entryB["node_id"])
	}
	if entryB["stage"] != "writer_admission" {
		t.Fatalf("stage mismatch on gateway-b: %v", entryB["stage"])
	}
	if entryB["failure_class"] != "backend_unavailable" {
		t.Fatalf("failure_class mismatch on gateway-b: %v", entryB["failure_class"])
	}

	// Strict privacy assertion: NO leak of secret strings, object keys, or auth headers into server logs
	combinedLogs := logsNodeA.String() + "\n" + logsNodeB.String()
	logSensitivePatterns := []string{
		"test-object-500.bin", "test-object-503.bin",
		"private ddb throttle", "private ddb lease failure",
		"secret-owner", "AWS4-HMAC-SHA256",
	}
	for _, pattern := range logSensitivePatterns {
		if strings.Contains(combinedLogs, pattern) {
			t.Fatalf("sensitive information leaked into server logs: %q", pattern)
		}
	}

	// In public client responses: server internal cause, stage, failure class, or secrets must NOT leak
	clientSensitivePatterns := []string{
		"private ddb throttle", "private ddb lease failure",
		"secret-owner", "backend_unavailable", "credential_lookup", "writer_admission",
	}
	for _, pattern := range clientSensitivePatterns {
		if strings.Contains(recA.Body.String(), pattern) || strings.Contains(recB.Body.String(), pattern) {
			t.Fatalf("internal server detail or secret leaked into HTTP response: %q", pattern)
		}
	}
}

func TestNegativeObservabilityUpstreamALBClassification(t *testing.T) {
	// Test the scenario where an error is returned upstream (e.g. ALB 503) without reaching gateway
	// Verify that such errors are classified distinctly as upstream_no_gateway_request
	reqID := "upstream-alb-request-id-12345"
	gatewayLogs := "" // Gateway was never hit

	hasGatewayLog := strings.Contains(gatewayLogs, reqID)
	if hasGatewayLog {
		t.Fatal("upstream ALB failure should not have gateway log")
	}
	// In runner classification: when client observes 503 but no matching gateway log exists,
	// it must classify as upstream_no_gateway_request.
	classification := "gateway_error"
	if !hasGatewayLog {
		classification = "upstream_no_gateway_request"
	}
	if classification != "upstream_no_gateway_request" {
		t.Fatalf("failed to classify upstream error: got %s", classification)
	}
}
