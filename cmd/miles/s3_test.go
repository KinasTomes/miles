package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func TestRequestGateDeadlineDrainAndQuiescence(t *testing.T) {
	a := new(app.Admission)
	entered := make(chan context.Context, 1)
	finish := make(chan struct{})
	done := make(chan struct{})
	handler := requestGate(a, 40*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- r.Context()
		<-r.Context().Done()
		<-finish // Cancellation is not quiescence.
	}), s3Unavailable)
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	}()
	ctx := <-entered
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 40*time.Millisecond {
		t.Fatal("missing hard deadline")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("deadline was not enforced")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal(ctx.Err())
	}
	a.Drain()
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	if w.Code != 503 {
		t.Fatal("drain admitted request", w.Code)
	}
	wait, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if a.Wait(wait) == nil {
		t.Fatal("cancel released admission before handler quiescence")
	}
	close(finish)
	<-done
	if err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDeadlineInterruptsBlockedRequestBody(t *testing.T) {
	a := new(app.Admission)
	done := make(chan struct{})
	server := httptest.NewServer(requestGate(a, 60*time.Millisecond, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		_, _ = io.Copy(io.Discard, r.Body)
	}), s3Unavailable))
	defer server.Close()
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	request, _ := http.NewRequest("POST", server.URL, reader)
	request.ContentLength = 100
	result := make(chan error, 1)
	go func() {
		resp, err := server.Client().Do(request)
		if resp != nil {
			resp.Body.Close()
		}
		result <- err
	}()
	if _, err := writer.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("context deadline alone left body read blocked")
	}
	writer.Close()
	<-result
	a.Drain()
	if err := a.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestWiredS3BucketSignedRequestsAndDrain(t *testing.T) {
	c := config.DefaultStartup()
	c.Mode = "dev"
	d, err := app.Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	a := new(app.Admission)
	server := httptest.NewServer(s3Handler(c.Region, c.RuntimeNodeID, d, a))
	defer server.Close()
	cred := d.DevCredentials[0]
	request := func(method, path string, expected int) {
		t.Helper()
		req, _ := http.NewRequest(method, server.URL+path, nil)
		hash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		req.Header.Set("X-Amz-Content-Sha256", hash)
		if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: cred.AccessKeyID, SecretAccessKey: cred.Secret}, req, hash, "s3", c.Region, time.Now()); err != nil {
			t.Fatal(err)
		}
		response, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != expected {
			t.Fatalf("%s %s returned %d: %s", method, path, response.StatusCode, body)
		}
		if expected == 503 && !strings.Contains(string(body), "ServiceUnavailable") {
			t.Fatal("drain did not use S3 error")
		}
	}
	request("PUT", "/new-bucket", 200)
	request("HEAD", "/new-bucket", 200)
	request("GET", "/", 200)
	request("GET", "/new-bucket/object", 501)
	a.Drain()
	request("PUT", "/another-bucket", 503)
}

func TestS3UnavailableLogsStructuredDiagnostics(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	defer slog.SetDefault(old)

	handler := s3UnavailableNode("test-node-cloud-1")
	req := httptest.NewRequest("PUT", "/bucket/key", nil)
	w := httptest.NewRecorder()
	handler(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
	reqID := w.Header().Get("X-Amz-Request-Id")
	if reqID == "" {
		t.Fatal("missing X-Amz-Request-Id header")
	}

	var entry map[string]any
	if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse json log: %v (%q)", err, logs.String())
	}
	if entry["msg"] != "s3 service unavailable" {
		t.Fatalf("msg: got %v want s3 service unavailable", entry["msg"])
	}
	if entry["request_id"] != reqID {
		t.Fatalf("request_id: got %v want %v", entry["request_id"], reqID)
	}
	if entry["node_id"] != "test-node-cloud-1" {
		t.Fatalf("node_id: got %v want test-node-cloud-1", entry["node_id"])
	}
	if entry["stage"] != "admission_gate" {
		t.Fatalf("stage: got %v want admission_gate", entry["stage"])
	}
	if entry["failure_class"] != "draining" {
		t.Fatalf("failure_class: got %v want draining", entry["failure_class"])
	}
}
