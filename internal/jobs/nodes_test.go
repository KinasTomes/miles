package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
)

type failedNodeWrites struct{ jobs.Store }

func (f failedNodeWrites) PutNode(context.Context, uint64, jobs.Node) (jobs.Node, error) {
	return jobs.Node{}, storage.ErrUnavailable
}
func TestHeartbeatDrainAndMonotonicStaleness(t *testing.T) {
	ctx := context.Background()
	store, e := badger.Open(badger.Options{Path: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	c, e := clock.NewVirtual(opstest.Now, 100)
	if e != nil {
		t.Fatal(e)
	}
	r := &jobs.Registry{Store: store, Clock: c, Identity: opstest.Node("node-a")}
	gate := new(app.Admission)
	release, ok := gate.Acquire()
	if !ok {
		t.Fatal("initial admission")
	}
	if e = r.Tick(ctx, gate, jobs.NodeStatus{Ready: true, AppliedPolicyRevision: 1}); e != nil {
		t.Fatal(e)
	}
	if !r.Healthy() {
		t.Fatal("missing initial heartbeat")
	}
	if e = c.SetWall(opstest.Now.Add(24 * time.Hour)); e != nil {
		t.Fatal(e)
	}
	if !r.Healthy() {
		t.Fatal("wall jump invalidated monotonic freshness")
	}
	if e = c.Advance(jobs.HeartbeatMaxAge); e != nil {
		t.Fatal(e)
	}
	if r.Healthy() {
		t.Fatal("stale heartbeat remained healthy at equality")
	}
	remote := &jobs.Registry{Store: store, Identity: opstest.Node("node-b")}
	if _, e = remote.RequestDrain(ctx, "node-a", nil); e != nil {
		t.Fatal(e)
	}
	if gate.Draining() {
		t.Fatal("remote marker falsely claimed local gate closed")
	}
	if e = r.Tick(ctx, gate, jobs.NodeStatus{Ready: true}); e != nil {
		t.Fatal(e)
	}
	n, e := store.GetNode(ctx, "node-a")
	if e != nil || !n.Draining || n.Ready || n.ActiveRequests != 1 || !gate.Draining() {
		t.Fatal(n, e)
	}
	if _, ok = gate.Acquire(); ok {
		t.Fatal("admitted after drain")
	}
	release()
	if e = r.Tick(ctx, gate, jobs.NodeStatus{Ready: true}); e != nil {
		t.Fatal(e)
	}
	n, e = store.GetNode(ctx, "node-a")
	if e != nil || n.ActiveRequests != 0 || !n.Draining {
		t.Fatal(n, e)
	}
}
func TestLocalDrainClosesAdmissionBeforeFailedWrite(t *testing.T) {
	store, e := badger.Open(badger.Options{Path: t.TempDir()})
	if e != nil {
		t.Fatal(e)
	}
	defer store.Close()
	c, e := clock.NewVirtual(opstest.Now, 100)
	if e != nil {
		t.Fatal(e)
	}
	r := &jobs.Registry{Store: store, Clock: c, Identity: opstest.Node("node-a")}
	gate := new(app.Admission)
	if e = r.Tick(context.Background(), gate, jobs.NodeStatus{Ready: true}); e != nil {
		t.Fatal(e)
	}
	r.Store = failedNodeWrites{Store: store}
	if _, e = r.RequestDrain(context.Background(), "node-a", gate); !errors.Is(e, storage.ErrUnavailable) {
		t.Fatal(e)
	}
	if !gate.Draining() {
		t.Fatal("write failure reopened admission")
	}
	if e = r.Tick(context.Background(), gate, jobs.NodeStatus{Ready: true}); e == nil || r.Healthy() {
		t.Fatal("failed heartbeat remained ready", e)
	}
}
