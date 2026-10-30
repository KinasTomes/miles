package jobs

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNodeClusterResultAndRecordBounds(t *testing.T) {
	r := Result{Scope: "cluster", Nodes: []NodeResult{{NodeID: "cloud-1", State: "succeeded", Result: json.RawMessage(`{"removed":3}`)}, {NodeID: "edge-1", State: "unavailable", ErrorCode: "heartbeat_stale"}}}
	if e := r.Validate(); e != nil {
		t.Fatal(e)
	}
	b, e := json.Marshal(r)
	if e != nil || !strings.Contains(string(b), `"state":"unavailable"`) {
		t.Fatal(string(b), e)
	}
	r.Nodes = append(r.Nodes, r.Nodes[0])
	if r.Validate() == nil {
		t.Fatal("duplicate member allowed")
	}
	r.Scope = "node"
	r.Nodes = r.Nodes[:2]
	if r.Validate() == nil {
		t.Fatal("node result accepted multiple nodes")
	}
	j := Job{ID: "job-fixture", Type: "scrub", State: Queued, Revision: 1, PolicyRevision: 1, Params: json.RawMessage(`{}`), CreatedAt: time.Now().UTC()}
	if j.Validate() != nil {
		t.Fatal("valid job rejected")
	}
	j.Params = json.RawMessage(`null`)
	if j.Validate() == nil {
		t.Fatal("null params accepted")
	}
	j.Params = json.RawMessage(`{"large":"` + strings.Repeat("x", 8192) + `"}`)
	if j.Validate() == nil {
		t.Fatal("unbounded params accepted")
	}
}
