package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/storage"
)

func nativeProvisionedCheckpoint(t *testing.T, ctx context.Context, node Dependencies) {
	t.Helper()
	journal := node.Journal
	state, e := journal.store.GetRecoveryState(ctx, journal.catalog.History)
	if e != nil || state.NextToSeal == 0 {
		t.Fatal(state, e)
	}
	fence, e := journal.authority.Acquire(ctx, "provisioned-checkpoint", "recovery")
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := journal.authority.Close(context.Background(), fence); e != nil {
			t.Error(e)
		}
	}()
	executor, ok := node.Operations.Cluster.Runner.Executors["recovery_checkpoint"]
	if !ok {
		t.Fatal("typed bootstrap did not expose checkpoint capability")
	}
	params, _ := json.Marshal(map[string]uint64{"target_epoch": state.NextToSeal - 1})
	step, e := executor.Step(ctx, jobs.Job{Worker: fence, Params: params}, config.GlobalSnapshot{})
	if e != nil || !step.Done {
		t.Fatal(step, e)
	}
	head, e := journal.store.GetRecoveryCatalog(ctx, journal.catalog.History)
	if e != nil || head.Revision == 0 {
		t.Fatal(head, e)
	}
}

func nativeClusterOperations(t *testing.T, ctx context.Context, nodes []Dependencies, token string) {
	t.Helper()
	gates := []*Admission{}
	handlers := []http.Handler{}
	for _, d := range nodes {
		gate := new(Admission)
		gates = append(gates, gate)
		d.Operations.Cluster.Gate = gate
		if e := d.Operations.Registry.Tick(ctx, gate, jobs.NodeStatus{Ready: true, AppliedPolicyRevision: 1}); e != nil {
			t.Fatal(e)
		}
		handler, e := admin.NewServer(*d.Admin)
		if e != nil {
			t.Fatal(e)
		}
		handlers = append(handlers, handler)
		t.Cleanup(func() {
			if e := d.Operations.Cluster.Runner.Close(context.Background()); e != nil {
				t.Error(e)
			}
		})
	}
	call := func(node int, method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handlers[node].ServeHTTP(rec, r)
		return rec
	}
	create := func(node int, body string) jobs.Detail {
		rec := call(node, "POST", "/admin/v1/jobs", body)
		if rec.Code != 202 {
			t.Fatal(rec.Code, rec.Body)
		}
		var j jobs.Detail
		if e := json.Unmarshal(rec.Body.Bytes(), &j); e != nil {
			t.Fatal(e)
		}
		return j
	}
	purge := func(node int, body string) jobs.Detail {
		rec := call(node, "POST", "/admin/v1/cache/purge", body)
		var j jobs.Detail
		if rec.Code != 202 || json.Unmarshal(rec.Body.Bytes(), &j) != nil {
			t.Fatal(rec.Code, rec.Body)
		}
		return j
	}
	wait := func(id string) jobs.Job {
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			for i, d := range nodes {
				if e := d.Operations.Registry.Tick(ctx, gates[i], jobs.NodeStatus{Ready: !gates[i].Draining(), AppliedPolicyRevision: 1}); e != nil {
					t.Fatal(e)
				}
				if e := d.Operations.Cluster.Runner.Tick(ctx); e != nil {
					t.Fatal(e)
				}
			}
			j, e := nodes[1].Operations.Cluster.GetJob(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			if j.State.Terminal() {
				return j
			}
			time.Sleep(5 * time.Millisecond)
		}
		j, _ := nodes[0].Operations.Cluster.GetJob(ctx, id)
		t.Fatalf("job did not settle: %+v", j)
		return j
	}
	// Exercise the public pack projection and promotion through the actual
	// runtime executor and AWS S3 adapter, not the native metadata-only seam.
	packResponse := call(1, "GET", "/admin/v1/packs", "")
	var inventory struct {
		Items []lifecycle.Pack `json:"items"`
	}
	if packResponse.Code != 200 || json.Unmarshal(packResponse.Body.Bytes(), &inventory) != nil {
		t.Fatal(packResponse.Code, packResponse.Body)
	}
	chosen := lifecycle.Pack{}
	for _, candidate := range inventory.Items {
		if candidate.Readable && candidate.LiveBytes > 0 {
			chosen = candidate
			break
		}
	}
	if chosen.PackID == "" {
		t.Fatal("native pack inventory has no live generation")
	}
	if r := call(0, "GET", "/admin/v1/packs/"+chosen.PackID, ""); r.Code != 200 {
		t.Fatal(r.Code, r.Body)
	}
	promotion := call(2, "POST", "/admin/v1/packs/"+chosen.PackID+"/"+chosen.Generation+"/promote", `{}`)
	var promotionJob jobs.Detail
	if promotion.Code != 202 || json.Unmarshal(promotion.Body.Bytes(), &promotionJob) != nil {
		t.Fatal(promotion.Code, promotion.Body)
	}
	if out := wait(promotionJob.ID); out.State != jobs.Succeeded {
		t.Fatal("native runtime promotion", out)
	}
	if r := call(1, "GET", "/admin/v1/jobs/"+promotionJob.ID, ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"source_retained":true`) {
		t.Fatal(r.Code, r.Body)
	}
	key := storage.CacheKey{Pack: storage.PackRef{ID: "native-cache", Generation: "generation-1"}, Range: storage.ByteRange{Length: 4}}
	historical := nodes[0].Operations.Registry.Identity
	historical.ID = "historical-boot"
	historical.Incarnation = "old-boot"
	historical.Revision = 0
	historical.Ready = false
	historical.HeartbeatAt = time.Now().Add(-3 * time.Minute)
	if _, e := nodes[0].Operations.Registry.Store.PutNode(ctx, 0, historical); e != nil {
		t.Fatal(e)
	}
	for _, d := range nodes {
		if e := d.Cache.Put(ctx, key, []byte("data")); e != nil {
			t.Fatal(e)
		}
	}
	dry := create(0, `{"type":"cache_purge","dry_run":true,"params":{"scope":"cluster"}}`)
	if out := wait(dry.ID); out.State != jobs.Succeeded {
		t.Fatal("dry run failed", out)
	}
	for _, d := range nodes {
		_, hit, e := d.Cache.Get(ctx, key)
		if e != nil || !hit {
			t.Fatal("dry run purged data", hit, e)
		}
	}
	if rec := call(0, "POST", "/admin/v1/jobs", `{"type":"cache_purge","params":{"scope":"node","node_id":"missing"}}`); rec.Code != 404 {
		t.Fatal("missing node is not a server failure", rec.Code, rec.Body)
	}
	if rec := call(0, "POST", "/admin/v1/jobs", `{"type":"cache_purge","params":{"scope":"cluster","targets":[{"node_id":"forged","incarnation":"forged"}]}}`); rec.Code != 400 {
		t.Fatal("caller forged target membership", rec.Code, rec.Body)
	}
	var j jobs.Detail
	// Dedicated cache routes use the same shared durable parent/child jobs.
	reportResponse := call(1, "GET", "/admin/v1/cache/stats?scope=cluster", "")
	var report jobs.CacheReport
	if reportResponse.Code != 200 || json.Unmarshal(reportResponse.Body.Bytes(), &report) != nil || !report.Complete || len(report.Nodes) != 3 {
		t.Fatal(reportResponse.Code, reportResponse.Body)
	}
	purgeResponse := call(2, "POST", "/admin/v1/cache/purge", `{"scope":"cluster"}`)
	if purgeResponse.Code != 202 || json.Unmarshal(purgeResponse.Body.Bytes(), &j) != nil {
		t.Fatal(purgeResponse.Code, purgeResponse.Body)
	}
	out := wait(j.ID)
	if out.State != jobs.Succeeded {
		t.Fatal(out)
	}
	var result jobs.Result
	if json.Unmarshal(out.Result, &result) != nil || len(result.Nodes) != 3 {
		t.Fatal(string(out.Result))
	}
	for _, d := range nodes {
		_, hit, e := d.Cache.Get(ctx, key)
		if e != nil || hit {
			t.Fatal("purge missed target", hit, e)
		}
	}
	rec := call(2, "GET", "/admin/v1/jobs/"+j.ID, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "checkpoint") || strings.Contains(rec.Body.String(), "fence") {
		t.Fatal(rec.Code, rec.Body)
	}
	stale := nodes[0].Operations.Registry.Identity
	stale.ID = "offline-node"
	stale.Incarnation = "offline-boot"
	stale.Revision = 0
	stale.Ready = false
	stale.HeartbeatAt = time.Now().Add(-time.Minute)
	if _, e := nodes[0].Operations.Registry.Store.PutNode(ctx, 0, stale); e != nil {
		t.Fatal(e)
	}
	reportResponse = call(0, "GET", "/admin/v1/cache/stats?scope=cluster", "")
	if reportResponse.Code != 200 || json.Unmarshal(reportResponse.Body.Bytes(), &report) != nil || report.Complete || len(report.Nodes) != 4 || report.IncludedNodes != 3 {
		t.Fatal("stale cache sample included", reportResponse.Code, reportResponse.Body)
	}
	j = purge(2, `{"scope":"cluster"}`)
	out = wait(j.ID)
	if out.State != jobs.Failed || out.ErrorCode != "partial_cluster" || !strings.Contains(string(out.Result), `"state":"unavailable"`) {
		t.Fatal("partial cluster reported success", out)
	}
	j = purge(0, `{"scope":"node","node_id":"offline-node"}`)
	out = wait(j.ID)
	if out.State != jobs.Failed || !strings.Contains(string(out.Result), `"scope":"node"`) || !strings.Contains(string(out.Result), `"state":"unavailable"`) {
		t.Fatal("unreachable node-scope job remained queued", out)
	}
	j = create(0, `{"type":"cache_purge","params":{"scope":"node"}}`)
	rec = call(1, "DELETE", "/admin/v1/jobs/"+j.ID, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"cancelled"`) {
		t.Fatal(rec.Code, rec.Body)
	}
	rec = call(0, "POST", "/admin/v1/jobs", `{"type":"gc"}`)
	if rec.Code != 501 {
		t.Fatal("unimplemented executor accepted", rec.Code, rec.Body)
	}
	target := nodes[2].Operations.Registry.Identity.ID
	drained := make(chan *httptest.ResponseRecorder, 1)
	go func() { drained <- call(0, "POST", "/admin/v1/cluster/nodes/"+target+"/drain", "{}") }()
	deadline := time.Now().Add(5 * time.Second)
	for !gates[2].Draining() && time.Now().Before(deadline) {
		_ = nodes[2].Operations.Registry.ObserveDrain(ctx, gates[2])
		time.Sleep(5 * time.Millisecond)
	}
	if !gates[2].Draining() {
		t.Fatal("remote drain gate not applied")
	}
	select {
	case rec = <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("target gate not acknowledged")
	}
	if rec.Code != 202 {
		t.Fatal(rec.Code, rec.Body)
	}
	if e := json.Unmarshal(rec.Body.Bytes(), &j); e != nil {
		t.Fatal(e)
	}
	out = wait(j.ID)
	if out.State != jobs.Succeeded || !strings.Contains(string(out.Result), `"flush_complete":false`) {
		t.Fatal("Edge claimed a physical flush", out)
	}
	// Cloud timeout/error proof must remain incomplete, even when its gate closed.
	cloud := nodes[1].Operations.Registry.Identity.ID
	go func() { drained <- call(0, "POST", "/admin/v1/cluster/nodes/"+cloud+"/drain", "{}") }()
	deadline = time.Now().Add(5 * time.Second)
	for !gates[1].Draining() && time.Now().Before(deadline) {
		_ = nodes[1].Operations.Registry.ObserveDrain(ctx, gates[1])
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case rec = <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("Cloud target gate not acknowledged")
	}
	if rec.Code != 202 {
		t.Fatal(rec.Code, rec.Body)
	}
	if e := json.Unmarshal(rec.Body.Bytes(), &j); e != nil {
		t.Fatal(e)
	}
	n, e := nodes[1].Operations.Registry.Store.GetNode(ctx, cloud)
	if e != nil {
		t.Fatal(e)
	}
	n.DrainState = "incomplete"
	if _, e = nodes[1].Operations.Registry.Store.PutNode(ctx, n.Revision, n); e != nil {
		t.Fatal(e)
	}
	out = wait(j.ID)
	if out.State != jobs.Failed || out.ErrorCode != "drain_incomplete" || !strings.Contains(string(out.Result), `"flush_complete":false`) {
		t.Fatal("Cloud error claimed completed flush", out)
	}
}
