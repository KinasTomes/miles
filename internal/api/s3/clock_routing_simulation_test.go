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
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
)

// simulatedChronyNode represents a gateway node with simulated clock bounds check
type simulatedChronyNode struct {
	nodeID          string
	isClockTrusted  bool
	probeCalls      int64
	router          Router
	writerCalls     int64
	committedWrites int64
}

func newSimulatedChronyNode(t *testing.T, nodeID string) *simulatedChronyNode {
	t.Helper()

	node := &simulatedChronyNode{
		nodeID:         nodeID,
		isClockTrusted: true, // starts healthy
	}

	fakeWriter := &simulatedObjectWriter{
		node: node,
	}

	world := newBucketWorld(t)
	node.router = world.router
	node.router.NodeID = nodeID
	node.router.Handlers = NewObjectHandlers(fakeWriter, nil, nil, nil)

	return node
}

type simulatedObjectWriter struct {
	fakeObjectWriter
	node *simulatedChronyNode
}

func (w *simulatedObjectWriter) PutObject(ctx context.Context, r object.PutRequest) (object.PutResult, error) {
	atomic.AddInt64(&w.node.writerCalls, 1)
	if !w.node.isClockTrusted {
		return object.PutResult{}, &object.PutStageError{
			Stage: "writer_admission",
			Err:   fmt.Errorf("chrony clock uncertainty exceeded: %w", storage.ErrClockUncertain),
		}
	}
	atomic.AddInt64(&w.node.committedWrites, 1)
	return object.PutResult{
		ETag:              "\"e2fc714c4727ee9395f324cd2e7f331f\"",
		ChecksumValue:     "dummy-checksum",
		ChecksumAlgorithm: "SHA256",
	}, nil
}

// Simulated ALB target group with health checking and round-robin / fail-open behavior
type simulatedALB struct {
	mu                 sync.Mutex
	targets            []*simulatedChronyNode
	targetHealth       map[string]bool
	unhealthyCount     map[string]int
	healthyCount       map[string]int
	rrIndex            int
	unhealthyThreshold int
	healthyThreshold   int
}

func newSimulatedALB(targets []*simulatedChronyNode, unhealthyThreshold, healthyThreshold int) *simulatedALB {
	th := make(map[string]bool)
	uc := make(map[string]int)
	hc := make(map[string]int)
	for _, tg := range targets {
		th[tg.nodeID] = true // initially healthy
		uc[tg.nodeID] = 0
		hc[tg.nodeID] = healthyThreshold
	}
	return &simulatedALB{
		targets:            targets,
		targetHealth:       th,
		unhealthyCount:     uc,
		healthyCount:       hc,
		unhealthyThreshold: unhealthyThreshold,
		healthyThreshold:   healthyThreshold,
	}
}

// PollHealthCheck simulates one ALB health check probe round across all targets
func (alb *simulatedALB) PollHealthCheck(ctx context.Context) {
	alb.mu.Lock()
	defer alb.mu.Unlock()

	for _, node := range alb.targets {
		atomic.AddInt64(&node.probeCalls, 1)
		// /readyz probe reflects clock trust exactly as internal/app/shared.go does
		isHealthy := node.isClockTrusted
		if !isHealthy {
			alb.unhealthyCount[node.nodeID]++
			alb.healthyCount[node.nodeID] = 0
			if alb.unhealthyCount[node.nodeID] >= alb.unhealthyThreshold {
				alb.targetHealth[node.nodeID] = false
			}
		} else {
			alb.healthyCount[node.nodeID]++
			alb.unhealthyCount[node.nodeID] = 0
			if alb.healthyCount[node.nodeID] >= alb.healthyThreshold {
				alb.targetHealth[node.nodeID] = true
			}
		}
	}
}

// ForwardRequest simulates ALB request forwarding
// If all targets are unhealthy, ALB activates fail-open (forwards to all targets round-robin)
func (alb *simulatedALB) ForwardRequest(req *http.Request) (*httptest.ResponseRecorder, string, bool) {
	alb.mu.Lock()
	defer alb.mu.Unlock()

	healthyTargets := make([]*simulatedChronyNode, 0)
	for _, t := range alb.targets {
		if alb.targetHealth[t.nodeID] {
			healthyTargets = append(healthyTargets, t)
		}
	}

	failOpen := false
	routingPool := healthyTargets
	if len(routingPool) == 0 {
		// ALB fail-open: when 100% of targets are unhealthy, forward anyway
		routingPool = alb.targets
		failOpen = true
	}

	target := routingPool[alb.rrIndex%len(routingPool)]
	alb.rrIndex++

	rec := httptest.NewRecorder()
	target.router.ServeHTTP(rec, req)
	return rec, target.nodeID, failOpen
}

func TestALBReadinessLagAndGatewayFailClosed(t *testing.T) {
	nodeA := newSimulatedChronyNode(t, "cloud-a")
	nodeB := newSimulatedChronyNode(t, "cloud-b")

	// ALB with standard threshold = 2
	alb := newSimulatedALB([]*simulatedChronyNode{nodeA, nodeB}, 2, 2)

	// 1. Initial state: Both nodes healthy (< 34s bound)
	alb.PollHealthCheck(context.Background())
	if !alb.targetHealth["cloud-a"] || !alb.targetHealth["cloud-b"] {
		t.Fatal("initial targets must be healthy")
	}

	// 2. Incident injection: cloud-a encounters NTP packet loss / jitter.
	// Clock becomes untrusted (exceeds min(1024, 2*16+2) = 34s bound).
	nodeA.isClockTrusted = false

	// Client sends PUT request immediately (before ALB health check poll fires)
	// Demonstrates that ALB routing lag permits traffic to reach cloud-a!
	reqA := httptest.NewRequest(http.MethodPut, "/demo/test-key-1", strings.NewReader("payload-bytes"))
	sign(reqA, accessKey("owner"), "secret-owner")
	recA, routedNode, failOpen := alb.ForwardRequest(reqA)

	if routedNode != "cloud-a" {
		t.Fatalf("expected round robin to cloud-a, got %s", routedNode)
	}
	if failOpen {
		t.Fatal("fail-open should not be active when cloud-b is healthy")
	}

	// Gateway admission MUST fail-closed: cloud-a rejected write
	if nodeA.committedWrites != 0 {
		t.Fatalf("gateway allowed write under clock uncertainty! committed=%d", nodeA.committedWrites)
	}
	if recA.Code != http.StatusInternalServerError && recA.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 500 or 503 from gateway admission, got %d", recA.Code)
	}

	// Next client request routes to cloud-b (healthy)
	reqB := httptest.NewRequest(http.MethodPut, "/demo/test-key-2", strings.NewReader("payload-bytes"))
	sign(reqB, accessKey("owner"), "secret-owner")
	recB, routedNodeB, _ := alb.ForwardRequest(reqB)

	if routedNodeB != "cloud-b" {
		t.Fatalf("expected round robin to cloud-b, got %s", routedNodeB)
	}
	if recB.Code != http.StatusOK {
		t.Fatalf("cloud-b should succeed, got %d: %s", recB.Code, recB.Body.String())
	}
	if nodeB.committedWrites != 1 {
		t.Fatalf("cloud-b did not commit write: %d", nodeB.committedWrites)
	}

	// 3. ALB Health Check fires 1 time:
	// Unhealthy count becomes 1, but unhealthyThreshold = 2, so ALB STILL marks cloud-a healthy!
	alb.PollHealthCheck(context.Background())
	if !alb.targetHealth["cloud-a"] {
		t.Fatal("ALB should require 2 consecutive failures before deregistering target")
	}

	// 4. ALB Health Check fires 2nd time:
	// Now cloud-a is deregistered from ALB
	alb.PollHealthCheck(context.Background())
	if alb.targetHealth["cloud-a"] {
		t.Fatal("cloud-a should now be marked unhealthy by ALB after 2 consecutive failed probes")
	}

	// 5. With cloud-a deregistered, subsequent requests route only to cloud-b
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/demo/test-key-subseq-%d", i), strings.NewReader("data"))
		sign(req, accessKey("owner"), "secret-owner")
		rec, node, _ := alb.ForwardRequest(req)
		if node != "cloud-b" {
			t.Fatalf("traffic still routed to unhealthy cloud-a: %s", node)
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("cloud-b request failed: %d", rec.Code)
		}
	}

	// 6. Test ALB Fail-Open:
	// If cloud-b ALSO encounters clock uncertainty, ALB targets become 0 healthy.
	nodeB.isClockTrusted = false
	alb.PollHealthCheck(context.Background())
	alb.PollHealthCheck(context.Background())

	if alb.targetHealth["cloud-a"] || alb.targetHealth["cloud-b"] {
		t.Fatal("both targets should be unhealthy")
	}

	// ALB activates fail-open: forwards to targets despite being unhealthy
	reqFailOpen := httptest.NewRequest(http.MethodPut, "/demo/test-fail-open", strings.NewReader("data"))
	sign(reqFailOpen, accessKey("owner"), "secret-owner")
	recFO, nodeFO, isFailOpen := alb.ForwardRequest(reqFailOpen)

	if !isFailOpen {
		t.Fatal("ALB fail-open should be active when 100% of targets are unhealthy")
	}
	// Gateway fail-closed invariant holds even when ALB fails open:
	if recFO.Code == http.StatusOK {
		t.Fatal("gateway must NOT commit writes during fail-open clock uncertainty")
	}
	t.Logf("ALB fail-open routed to %s, gateway safely rejected with HTTP %d", nodeFO, recFO.Code)

	// 7. Test Recovery:
	// cloud-a receives fresh NTP packet: sample age drops to 0.5s
	nodeA.isClockTrusted = true
	alb.PollHealthCheck(context.Background())
	alb.PollHealthCheck(context.Background()) // 2 healthy checks to register

	if !alb.targetHealth["cloud-a"] {
		t.Fatal("cloud-a should recover to healthy after fresh NTP samples")
	}

	reqRecovered := httptest.NewRequest(http.MethodPut, "/demo/test-recovered", strings.NewReader("recovered-data"))
	sign(reqRecovered, accessKey("owner"), "secret-owner")
	recRec, nodeRec, _ := alb.ForwardRequest(reqRecovered)

	if nodeRec != "cloud-a" {
		t.Fatalf("expected cloud-a to handle recovered traffic, got %s", nodeRec)
	}
	if recRec.Code != http.StatusOK {
		t.Fatalf("cloud-a recovered request failed: %d", recRec.Code)
	}
}

func TestClockUncertaintyStructuredDiagnosticsNoLeak(t *testing.T) {
	nodeA := newSimulatedChronyNode(t, "cloud-a")
	nodeA.isClockTrusted = false // untrusted

	var logBuf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logBuf, nil)))
	defer slog.SetDefault(old)

	req := httptest.NewRequest(http.MethodPut, "/demo/private-untrusted-key", strings.NewReader("body"))
	sign(req, accessKey("owner"), "secret-owner")
	rec := httptest.NewRecorder()
	nodeA.router.ServeHTTP(rec, req)

	// Must record structured log with exact stage and failure_class
	var entry map[string]any
	if err := json.Unmarshal(logBuf.Bytes(), &entry); err != nil {
		t.Fatalf("failed to parse log json: %v, raw: %s", err, logBuf.String())
	}

	if entry["stage"] != "writer_admission" {
		t.Fatalf("expected stage writer_admission, got %v", entry["stage"])
	}
	if entry["failure_class"] != "clock_uncertainty_exceeded" {
		t.Fatalf("expected failure_class clock_uncertainty_exceeded, got %v", entry["failure_class"])
	}

	// Secret leak checks
	logStr := logBuf.String()
	for _, sec := range []string{"private-untrusted-key", "secret-owner", "Authorization"} {
		if strings.Contains(logStr, sec) {
			t.Fatalf("log contains sensitive data: %s", sec)
		}
	}
}

// TestDeterministic90PutWorkloadTwoNodesFaultModel models the exact 90-PUT trial sequence
// with 2 gateway nodes under ALB round-robin routing:
//   - PUT #1..#34: both nodes healthy -> 34 successful PUTs
//   - PUT #35..#47: 12.5s jitter window on cloud-a (7 requests routed to cloud-a fail-closed,
//     6 interleaved requests routed to healthy cloud-b succeed)
//   - PUT #48..#90: clock recovers on cloud-a -> 43 consecutive successful PUTs
//   - Expected outcome: exactly 83/90 successful, exactly 7 expected fail-closed, 0 unintended errors.
func TestDeterministic90PutWorkloadTwoNodesFaultModel(t *testing.T) {
	nodeA := newSimulatedChronyNode(t, "cloud-a")
	nodeB := newSimulatedChronyNode(t, "cloud-b")
	alb := newSimulatedALB([]*simulatedChronyNode{nodeA, nodeB}, 2, 2)

	type putResultRecord struct {
		index      int
		routedNode string
		httpStatus int
		failClosed bool
		committed  bool
	}

	results := make([]putResultRecord, 90)
	var expectedFailClosedCount int
	var successCount int

	for i := 1; i <= 90; i++ {
		// Window fault injection: PUT #35 to #47 represents the 12.5s jitter window
		if i >= 35 && i <= 47 {
			nodeA.isClockTrusted = false // NTP jitter on cloud-a
		} else {
			nodeA.isClockTrusted = true
		}
		nodeB.isClockTrusted = true // cloud-b remains healthy throughout

		req := httptest.NewRequest(http.MethodPut, fmt.Sprintf("/demo/object-%03d.bin", i), strings.NewReader("sample-payload"))
		sign(req, accessKey("owner"), "secret-owner")
		rec, routedNode, _ := alb.ForwardRequest(req)

		recCommitted := (rec.Code == http.StatusOK)
		isFailClosed := (rec.Code != http.StatusOK)

		results[i-1] = putResultRecord{
			index:      i,
			routedNode: routedNode,
			httpStatus: rec.Code,
			failClosed: isFailClosed,
			committed:  recCommitted,
		}

		if recCommitted {
			successCount++
		} else {
			expectedFailClosedCount++
		}
	}

	// 1. Verify exact breakdown matches Cloud C1 Trial 1
	if successCount != 83 {
		t.Fatalf("expected 83 successful PUTs, got %d", successCount)
	}
	if expectedFailClosedCount != 7 {
		t.Fatalf("expected exactly 7 fail-closed PUTs, got %d", expectedFailClosedCount)
	}

	// 2. Verify all 7 failures occurred exclusively on cloud-a during the jitter window
	expectedFailIndices := map[int]bool{35: true, 37: true, 39: true, 41: true, 43: true, 45: true, 47: true}
	for _, r := range results {
		if expectedFailIndices[r.index] {
			if r.routedNode != "cloud-a" {
				t.Fatalf("PUT #%d should have routed to cloud-a, got %s", r.index, r.routedNode)
			}
			if r.httpStatus == http.StatusOK {
				t.Fatalf("PUT #%d on clock-untrusted cloud-a must not succeed", r.index)
			}
			if r.committed {
				t.Fatalf("PUT #%d committed data despite clock uncertainty!", r.index)
			}
		} else {
			if r.httpStatus != http.StatusOK {
				t.Fatalf("PUT #%d unexpectedly failed: status=%d, node=%s", r.index, r.httpStatus, r.routedNode)
			}
		}
	}

	// 3. Verify exactly 43 consecutive successes after the jitter window
	for i := 48; i <= 90; i++ {
		r := results[i-1]
		if r.httpStatus != http.StatusOK {
			t.Fatalf("PUT #%d after recovery failed: status=%d", i, r.httpStatus)
		}
	}

	t.Logf("Deterministic 90-PUT simulation verified: 34 initial + 6 interleaved + 43 recovered = 83/90 success, 7 expected fail-closed faults on cloud-a, 0 corrupted writes")
}
