package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

type terminalFaultKey struct{}
type terminalHTTPFault struct {
	method      string
	drop, block int32
	calls       atomic.Int32
	failProof   atomic.Bool
	pause       *gcFlowPause
}
type terminalFaultTransport struct{ http.RoundTripper }

func (h terminalFaultTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	f, _ := r.Context().Value(terminalFaultKey{}).(*terminalHTTPFault)
	if f == nil {
		return h.RoundTripper.RoundTrip(r)
	}
	if f.failProof.Load() && r.Method == "GET" && r.URL.Query().Has("versions") {
		return nil, fmt.Errorf("injected terminal version-inventory outage")
	}
	mutable := r.Method == f.method && (r.Method == "PUT" && r.Header.Get("X-Amz-Copy-Source") != "" || r.Method == "DELETE" && r.URL.Query().Get("versionId") != "")
	if !mutable {
		return h.RoundTripper.RoundTrip(r)
	}
	n := f.calls.Add(1)
	if n == f.block {
		if err := f.pause.block(r.Context()); err != nil {
			return nil, err
		}
	}
	response, err := h.RoundTripper.RoundTrip(r)
	if err == nil && response.StatusCode >= 200 && response.StatusCode < 300 && n == f.drop {
		_, readErr := io.Copy(io.Discard, response.Body)
		closeErr := response.Body.Close()
		return nil, errors.Join(fmt.Errorf("injected lost physical ACK"), readErr, closeErr)
	}
	return response, err
}

func TestNativeLifecycleTerminalProvider(t *testing.T) {
	for _, scenario := range []string{"copy-lost-ack", "copy-inflight", "delete-lost-ack", "delete-partial-inflight"} {
		t.Run(scenario, func(t *testing.T) { runNativeRuntime(t, true, scenario) })
	}
}

// All state mutations use composed services and authority. Faults surround the
// real SDK transport; terminal evidence comes from runtimeLifecycleTerminal and
// its actual S3 backend, never the metadata contract's test-only oracle.
func nativeTerminalFlows(t *testing.T, ctx context.Context, nodes []Dependencies, m *dynamodb.Store, client *s3.Client, packBucket string, bucket storage.Bucket, clk *gcFlowClock, scenario, token string) {
	t.Helper()
	handler, err := admin.NewServer(*nodes[0].Admin)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PUT", "/admin/v1/policy", strings.NewReader(`{"expected_revision":1,"tiering":{"cold_after_days":0,"min_pack_live_ratio":0,"cold_storage_class":"GLACIER_IR"}}`))
	r.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, r)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	record, err := m.GetPolicy(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	validator, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	policy, err := validator.DecodeGlobal(record)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := nodes[0].ObjectsForRequest(ctx, bucket.Name)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("terminal provider keeps physical roots safe")
	if _, err = svc.PutObject(ctx, object.PutRequest{Bucket: bucket, Key: "terminal-source", UserID: "owner", CredentialID: "RUNTIMEKEY00000000001", ContentLength: int64(len(data)), Body: bodyForNative(data)}); err != nil {
		t.Fatal(err)
	}
	key, _ := engine.HashChunk(bucket.Domain, data)
	chunks, err := m.GetChunks(ctx, []storage.ChunkKey{key}, true)
	if err != nil || len(chunks) != 1 {
		t.Fatal(chunks, err)
	}
	ref := chunks[key].Location.Pack
	runner := nodes[0].Operations.Cluster.Runner
	tick := func() {
		t.Helper()
		for _, node := range nodes {
			if err := node.Operations.Registry.Tick(ctx, new(Admission), jobs.NodeStatus{Ready: true, AppliedPolicyRevision: 2}); err != nil {
				t.Fatal(err)
			}
		}
	}
	create := func(kind jobs.Type) jobs.Job {
		t.Helper()
		tick()
		params, _ := json.Marshal(lifecycle.Params{PackID: ref.ID, Generation: ref.Generation})
		j, err := runner.Create(ctx, jobs.Create{Type: kind, Params: params, PolicyRevision: 2})
		if err != nil {
			t.Fatal(err)
		}
		fence, err := runner.Authority.Acquire(ctx, j.ID, "control")
		if err != nil {
			t.Fatal(err)
		}
		for _, change := range []string{"claim", "begin_step"} {
			j, err = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: change, Fence: fence, NodeID: runner.Node.ID, NodeIncarnation: runner.Node.Incarnation, StepID: strings.Repeat("1", 32)})
			if err != nil {
				t.Fatal(err)
			}
		}
		return j
	}
	closeJob := func(j jobs.Job) {
		t.Helper()
		if err := runner.Authority.Close(ctx, j.Worker); err != nil {
			t.Fatal(err)
		}
	}
	deferred := func(kind jobs.Type) {
		t.Helper()
		j := create(kind)
		out, err := runner.Executors[kind].Step(ctx, j, policy)
		if err != nil && !errors.Is(err, storage.ErrR1Busy) || !out.Settled || out.Done {
			t.Fatal("protected pack was not deferred", kind, out, err)
		}
		closeJob(j)
	}
	// Fresh durable PUT protection prevents any tiering dispatch.
	deferred("tiering")
	clk.offset.Add(int64(2 * time.Hour))
	// A resolved real GET view retains an independent source pin.
	var captured bytes.Buffer
	get := pauseGCFlow(t, ctx, "", "", func(ctx context.Context, p *gcFlowPause) error {
		edge, err := nodes[2].ObjectsForRequest(ctx, bucket.Name)
		if err != nil {
			return err
		}
		_, err = edge.GetObject(ctx, object.GetRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: "terminal-source", CredentialID: "RUNTIMEKEY00000000001", Ready: func(object.GetResult) error { return p.block(ctx) }, Emit: func(_ context.Context, b []byte) error { _, err := captured.Write(b); return err }})
		return err
	})
	deferred("tiering")
	get.finish(t)
	if !bytes.Equal(captured.Bytes(), data) {
		t.Fatal("old reader lost payload")
	}
	writer := storage.WriterFence{LeaseID: "terminal-concurrent-writer", Epoch: 1, ServiceIncarnation: nodes[0].Journal.catalog.Incarnation}
	if _, err = m.AcquireR1(ctx, storage.R1Acquire{Fence: writer, Owner: "writer", Kind: "writer", Object: storage.ObjectKey{Bucket: bucket.Name, BucketID: bucket.ID, Key: "writer"}, Deadline: clk.Now().WallUTC.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	deferred("tiering")
	if err = m.CloseR1(ctx, writer); err != nil {
		t.Fatal(err)
	}
	if err = m.ReleaseR1(ctx, writer); err != nil {
		t.Fatal(err)
	}

	kind, method, sends := jobs.Type("tiering"), "PUT", int32(1)
	if strings.HasPrefix(scenario, "delete") {
		kind, method, sends = "gc", "DELETE", 2
		sealWarmGCCut(t, ctx, nodes[0].Journal)
		nativeProvisionedCheckpoint(t, ctx, nodes[0])
		state, err := m.GetRecoveryState(ctx, nodes[0].Journal.catalog.History)
		if err != nil {
			t.Fatal(err)
		}
		epoch := state.NextToSeal - 1
		retentionChange := func(pin bool) {
			t.Helper()
			journal := nodes[0].Journal
			fence, err := journal.authority.Acquire(ctx, "terminal-root-consumer", "recovery")
			if err != nil {
				t.Fatal(err)
			}
			retention := recovery.Retention{Store: m, Checkpointer: recovery.Checkpointer{Objects: journal.objects, Catalog: journal.catalog}, Packs: nodes[0].Packs, Worker: fence}
			if err = retention.Pin(ctx, epoch, "terminal-consumer", pin); err != nil {
				t.Fatal(err)
			}
			bounds, _ := clk.Bounds(ctx)
			if err = retention.Prune(ctx, bounds); err != nil {
				t.Fatal(err)
			}
			if err = journal.authority.Close(ctx, fence); err != nil {
				t.Fatal(err)
			}
		}
		retentionChange(true)
		if err = svc.DeleteObject(ctx, object.DeleteRequest{Bucket: bucket, Key: "terminal-source", UserID: "owner", CredentialID: "RUNTIMEKEY00000000001"}); err != nil {
			t.Fatal(err)
		}
		deferred("gc") // the published cut still roots the original generation
		clk.offset.Add(int64(25 * time.Hour))
		sealWarmGCCut(t, ctx, nodes[0].Journal)
		nativeProvisionedCheckpoint(t, ctx, nodes[0])
		deferred("gc") // the independent recovery consumer still pins the old cut
		retentionChange(false)
		retire := create("gc")
		if out, err := runner.Executors["gc"].Step(ctx, retire, policy); err != nil || !out.Done {
			t.Fatal(out, err)
		}
		closeJob(retire)
		deferred("gc") // retirement still needs the 24h+10m grace and a newer cut
		clk.offset.Add(int64(25 * time.Hour))
		sealWarmGCCut(t, ctx, nodes[0].Journal)
		nativeProvisionedCheckpoint(t, ctx, nodes[0])
	}
	j := create(kind)
	fault := &terminalHTTPFault{method: method, drop: sends}
	step := func(call context.Context) error {
		out, err := runner.Executors[kind].Step(context.WithValue(call, terminalFaultKey{}, fault), j, policy)
		if err == nil || out.Settled {
			return fmt.Errorf("lost ACK settled action: %+v, %v", out, err)
		}
		return nil
	}
	assertProtected := func() lifecycle.Action {
		t.Helper()
		a, err := m.GetLifecycle(ctx, j.ID)
		gate, gateErr := m.GetR1Gate(ctx)
		if err != nil || gateErr != nil || a.State != "Unknown" || gate.State != "Maintenance" || a.StepID != j.PendingStep {
			t.Fatal(a, gate, err, gateErr)
		}
		if _, err := runner.Authority.Acquire(ctx, j.ID, "control"); !errors.Is(err, jobs.ErrReconciliation) {
			t.Fatal("unknown admitted takeover", err)
		}
		other := storage.WriterFence{LeaseID: "terminal-concurrent-gc", Epoch: 1, ServiceIncarnation: j.Worker.ServiceIncarnation}
		if _, err := m.AcquireR1(ctx, storage.R1Acquire{Fence: other, Owner: "concurrent-gc", Kind: "control", Deadline: clk.Now().WallUTC.Add(time.Minute)}); err == nil {
			t.Fatal("unknown admitted concurrent GC")
		}
		return a
	}
	if strings.Contains(scenario, "inflight") {
		fault.block = sends
		paused := pauseGCFlow(t, ctx, "", "", func(call context.Context, p *gcFlowPause) error { fault.pause = p; return step(call) })
		if err := m.UnknownLifecycle(ctx, j); err != nil {
			t.Fatal(err)
		}
		assertProtected()
		if out, err := runner.Executors[kind].Step(context.WithValue(ctx, terminalFaultKey{}, fault), j, policy); err == nil || out.Settled {
			t.Fatal("in-flight request counted as terminal", out, err)
		}
		assertProtected()
		paused.finish(t)
	} else if err := step(ctx); err != nil {
		t.Fatal(err)
	}
	a := assertProtected()
	if fault.calls.Load() != sends {
		t.Fatal("physical SDK retry", fault.calls.Load(), sends)
	}
	// A provider read failure preserves the durable action and maintenance.
	fault.failProof.Store(true)
	if out, err := runner.Executors[kind].Step(context.WithValue(ctx, terminalFaultKey{}, fault), j, policy); err == nil || out.Settled {
		t.Fatal("unavailable physical proof released protection", out, err)
	}
	assertProtected()
	fault.failProof.Store(false)
	out, err := runner.Executors[kind].Step(context.WithValue(ctx, terminalFaultKey{}, fault), j, policy)
	if err != nil || !out.Done || !out.Settled {
		t.Fatal("real provider did not settle terminal action", out, err)
	}
	gate, err := m.GetR1Gate(ctx)
	if err != nil || gate.State != "Open" {
		t.Fatal("terminal proof stranded gate", gate, err)
	}
	closeJob(j)
	// The original action and StepID stay immutable while the replacement job
	// worker acquires its own epoch and step after a lost Jobs checkpoint ACK.
	next, err := runner.Authority.Acquire(ctx, j.ID, "control")
	if err != nil {
		t.Fatal(err)
	}
	old := j
	for _, change := range []string{"claim", "begin_step"} {
		j, err = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: change, Fence: next, NodeID: runner.Node.ID, NodeIncarnation: runner.Node.Incarnation, StepID: strings.Repeat("2", 32)})
		if err != nil {
			t.Fatal(err)
		}
	}
	out, err = runner.Executors[kind].Step(context.WithValue(ctx, terminalFaultKey{}, fault), j, policy)
	if err != nil || !out.Done || fault.calls.Load() != sends {
		t.Fatal("takeover redispatched physical action", out, err, fault.calls.Load())
	}
	if _, err := runner.Executors[kind].Step(ctx, old, policy); !errors.Is(err, storage.ErrFenced) {
		t.Fatal("old worker was not fenced", err)
	}
	closeJob(j)
	final, err := m.GetLifecycle(ctx, j.ID)
	if err != nil || final.StepID != a.StepID || final.Fence != a.Fence {
		t.Fatal("takeover rewrote original action identity", final, err)
	}
	if kind == "gc" {
		inventory, err := (packs3.VersionInventory{Client: client, Bucket: packBucket}).CollectInventory(ctx, 10000)
		if err != nil || !inventory.Complete {
			t.Fatal(inventory.Complete, err)
		}
		for _, o := range inventory.Objects {
			if o.Pack == ref {
				t.Fatal("exact swept generation remains", o)
			}
		}
	} else {
		edge, err := nodes[2].ObjectsForRequest(ctx, bucket.Name)
		if err != nil {
			t.Fatal(err)
		}
		var got bytes.Buffer
		if _, err = edge.GetObject(ctx, object.GetRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: "terminal-source", CredentialID: "RUNTIMEKEY00000000001", Emit: func(_ context.Context, b []byte) error { _, err := got.Write(b); return err }}); err != nil || !bytes.Equal(got.Bytes(), data) {
			t.Fatal("resolved copy lost data", err)
		}
	}
	t.Logf("provider=runtimeLifecycleTerminal scenario=%s action=%s original-step=%s SDK-mutations=%d", scenario, a.ID, a.StepID, fault.calls.Load())
}
