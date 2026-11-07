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
	var err error
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}

	nonAdminToken := w.mintToken("user-writer", false)

	// 1. Unauthorized / Non-admin checks
	w.must(w.do("GET", "/admin/v1/faults", "", ""), 401)
	w.must(w.do("GET", "/admin/v1/faults", nonAdminToken, ""), 403)
	w.must(w.do("POST", "/admin/v1/faults", "", `{"type":"s3_5xx","scope":"cluster","rate":1.0,"duration_seconds":60}`), 401)
	w.must(w.do("POST", "/admin/v1/faults", nonAdminToken, `{"type":"s3_5xx","scope":"cluster","rate":1.0,"duration_seconds":60}`), 403)
	w.must(w.do("DELETE", "/admin/v1/faults/flt-123", "", ""), 401)
	w.must(w.do("DELETE", "/admin/v1/faults/flt-123", nonAdminToken, ""), 403)

	// 2. Disabled fault injection
	rec := w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"cluster","rate":1.0,"duration_seconds":60}`), 400)
	if code(t, rec) != "invalid_request" {
		t.Fatalf("expected invalid_request code when disabled, got %s", code(t, rec))
	}

	// 3. Enable fault injection
	reg.SetEnabled(true)

	// Invalid POST body / parameters
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{invalid json`), 400)
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"invalid_type","scope":"cluster","rate":1.0,"duration_seconds":60}`), 400)
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"node","rate":1.0,"duration_seconds":60}`), 400)    // node scope requires node_id
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"cluster","rate":0.0,"duration_seconds":60}`), 400) // rate <= 0
	w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"cluster","rate":1.0,"duration_seconds":0}`), 400)  // duration <= 0

	// Valid POST
	createRec := w.must(w.do("POST", "/admin/v1/faults", w.root, `{"type":"s3_5xx","scope":"cluster","rate":1.0,"duration_seconds":120}`), 201)
	var created faults.FaultRule
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.ID == "" || created.Type != faults.FaultS35xx || created.Scope != faults.ScopeCluster {
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
