package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/faults"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

func TestWiredS3FaultInjection(t *testing.T) {
	c := config.DefaultStartup()
	c.Mode = "dev"
	c.EnableFaultInjection = true
	d, err := app.Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	if d.Faults == nil {
		t.Fatal("expected non-nil d.Faults when enable_fault_injection is true")
	}

	a := new(app.Admission)
	nodeID := "dev-node"
	d.NodeID = nodeID
	server := httptest.NewServer(s3Handler(c.Region, nodeID, d, a))
	defer server.Close()

	cred := d.DevCredentials[0]
	signAndDo := func(method, path string) (int, string) {
		req, _ := http.NewRequest(method, server.URL+path, nil)
		hash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		req.Header.Set("X-Amz-Content-Sha256", hash)
		if err := v4.NewSigner().SignHTTP(context.Background(), aws.Credentials{AccessKeyID: cred.AccessKeyID, SecretAccessKey: cred.Secret}, req, hash, "s3", c.Region, time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	// 1. Initial healthy call -> 200 OK
	code, body := signAndDo("GET", "/")
	if code != http.StatusOK {
		t.Fatalf("expected 200 OK initially, got %d: %s", code, body)
	}

	// 2. Inject s3_5xx fault
	rule, err := d.Faults.Add(time.Now(), faults.FaultRule{
		Type:     faults.FaultS35xx,
		Scope:    faults.ScopeNode,
		NodeID:   nodeID,
		Rate:     1.0,
		Duration: 60,
	})
	if err != nil {
		t.Fatalf("failed to add s3_5xx fault: %v", err)
	}

	// Request should fail with 500 InternalError
	code, body = signAndDo("GET", "/")
	if code != http.StatusInternalServerError {
		t.Fatalf("expected 500 InternalServerError with s3_5xx injected, got %d: %s", code, body)
	}
	if !strings.Contains(body, "InternalError") {
		t.Fatalf("expected S3 InternalError in body, got: %s", body)
	}

	// Delete s3_5xx rule -> restores normal service
	if err := d.Faults.Delete(rule.ID); err != nil {
		t.Fatalf("failed to delete fault rule: %v", err)
	}
	code, body = signAndDo("GET", "/")
	if code != http.StatusOK {
		t.Fatalf("expected 200 OK after deleting fault, got %d: %s", code, body)
	}

	// 3. Inject kill_node fault
	_, err = d.Faults.Add(time.Now(), faults.FaultRule{
		Type:     faults.FaultKillNode,
		Scope:    faults.ScopeNode,
		NodeID:   nodeID,
		Rate:     1.0,
		Duration: 60,
	})
	if err != nil {
		t.Fatalf("failed to add kill_node fault: %v", err)
	}

	// Request should trigger admission drain and return 503
	code, body = signAndDo("GET", "/")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 ServiceUnavailable with kill_node injected, got %d: %s", code, body)
	}
	if !a.Draining() {
		t.Fatal("expected admission gate to be draining after kill_node")
	}

	// Subsequent request also rejected because admission is draining
	code, body = signAndDo("GET", "/")
	if code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 ServiceUnavailable on drained node, got %d: %s", code, body)
	}
}
