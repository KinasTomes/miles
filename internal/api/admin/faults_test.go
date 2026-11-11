package admin

import (
	"encoding/json"
	"testing"

	"github.com/KinasTomes/miles/internal/faults"
	"github.com/KinasTomes/miles/internal/metadata/badger"
)

func TestAdminFaultAPIRBACAndLifecycle(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	reg := faults.NewRegistry(false)
	w.opts.Faults = reg
	w.opts.NodeID = "node-1"
	var err error
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}

	nonAdminToken := w.mintToken("user-writer", false)

	// 1. Unauthorized / Non-admin checks
	w.must(w.do("GET", "/admin/v1/faults", "", ""), 401)
	w.must(w.do("GET", "/admin/v1/faults", nonAdminToken, ""), 403)
	w.must(w.do("POST", "/admin/v1/faults", "", `{"type":"s3_5xx","scope":"node","node_id":"node-1","rate":1.0,"duration_seconds":60}`), 401)
	w.must(w.do("POST", "/admin/v1/faults", nonAdminToken, `{"type":"s3_5xx","scope":"node","node_id":"node-1","rate":1.0,"duration_seconds":60}`), 403)
	w.must(w.do("DELETE", "/admin/v1/faults/flt-123", "", ""), 401)
	w.must(w.do("DELETE", "/admin/v1/faults/flt-123", nonAdminToken, ""), 403)

	// 2. Disabled fault injection
	rec := w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"node","node_id":"node-1","rate":1.0,"duration_seconds":60}`), 400)
	if code(t, rec) != "invalid_request" {
		t.Fatalf("expected invalid_request code when disabled, got %s", code(t, rec))
	}

	// 3. Enable fault injection
	reg.SetEnabled(true)

	// Invalid POST body / parameters
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{invalid json`), 400)
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"invalid_type","scope":"node","node_id":"node-1","rate":1.0,"duration_seconds":60}`), 400)
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"cluster","rate":1.0,"duration_seconds":60}`), 400)                 // cluster scope rejected in MVP
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"node","rate":1.0,"duration_seconds":60}`), 400)                    // node scope requires node_id
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"node","node_id":"node-2","rate":1.0,"duration_seconds":60}`), 400) // node_id mismatch rejected
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"node","node_id":"node-1","rate":0.0,"duration_seconds":60}`), 400) // rate <= 0
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"node","node_id":"node-1","rate":1.0,"duration_seconds":0}`), 400)  // duration <= 0

	// Valid POST
	createRec := w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"node","node_id":"node-1","rate":1.0,"duration_seconds":120}`), 201)
	var created faults.FaultRule
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Type != faults.FaultS35xx || created.Scope != faults.ScopeNode || created.NodeID != "node-1" {
		t.Fatalf("unexpected created rule: %+v", created)
	}

	// List
	listRec := w.must(w.do("GET", "/admin/v1/faults", w.root, ""), 200)
	var list []faults.FaultRule
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("unexpected list output: %+v", list)
	}

	// DELETE
	w.must(w.do("DELETE", "/admin/v1/faults/"+created.ID, w.root, ""), 204)
	w.must(w.do("DELETE", "/admin/v1/faults/"+created.ID, w.root, ""), 404) // second delete 404
}

func TestAdminFaultNodeTargetMismatch(t *testing.T) {
	// Gateway A
	wA := newWorld(t, badger.Bootstrap{})
	regA := faults.NewRegistry(true)
	wA.opts.Faults = regA
	wA.opts.NodeID = "node-a"
	var err error
	wA.h, err = NewServer(wA.opts)
	if err != nil {
		t.Fatal(err)
	}

	// Gateway B
	wB := newWorld(t, badger.Bootstrap{})
	regB := faults.NewRegistry(true)
	wB.opts.Faults = regB
	wB.opts.NodeID = "node-b"
	wB.h, err = NewServer(wB.opts)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Gateway A accepts node-a (matching) with 201 Created
	recA := wA.must(wA.do("POST", "/admin/v1/faults", wA.root, `{"type":"s3_5xx","scope":"node","node_id":"node-a","rate":1.0,"duration_seconds":60}`), 201)
	var createdA faults.FaultRule
	if err := json.Unmarshal(recA.Body.Bytes(), &createdA); err != nil {
		t.Fatal(err)
	}
	if createdA.NodeID != "node-a" {
		t.Fatalf("expected node-a, got %s", createdA.NodeID)
	}

	// 2. Gateway A rejects node-b (mismatch) with 400 Bad Request
	recMismatchedA := wA.must(wA.do("POST", "/admin/v1/faults", wA.root, `{"type":"s3_5xx","scope":"node","node_id":"node-b","rate":1.0,"duration_seconds":60}`), 400)
	if code(t, recMismatchedA) != "invalid_request" {
		t.Fatalf("expected invalid_request on mismatched node, got %s", code(t, recMismatchedA))
	}

	// 3. Gateway B accepts node-b (matching) with 201 Created
	recB := wB.must(wB.do("POST", "/admin/v1/faults", wB.root, `{"type":"s3_5xx","scope":"node","node_id":"node-b","rate":1.0,"duration_seconds":60}`), 201)
	var createdB faults.FaultRule
	if err := json.Unmarshal(recB.Body.Bytes(), &createdB); err != nil {
		t.Fatal(err)
	}
	if createdB.NodeID != "node-b" {
		t.Fatalf("expected node-b, got %s", createdB.NodeID)
	}

	// 4. Gateway B rejects node-a (mismatch) with 400 Bad Request
	recMismatchedB := wB.must(wB.do("POST", "/admin/v1/faults", wB.root, `{"type":"s3_5xx","scope":"node","node_id":"node-a","rate":1.0,"duration_seconds":60}`), 400)
	if code(t, recMismatchedB) != "invalid_request" {
		t.Fatalf("expected invalid_request on mismatched node, got %s", code(t, recMismatchedB))
	}
}
