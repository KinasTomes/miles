package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
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

type gcFlowClock struct {
	clock.Clock
	offset atomic.Int64
}

func (c *gcFlowClock) Now() clock.Reading {
	sample := c.Clock.Now()
	sample.WallUTC = sample.WallUTC.Add(time.Duration(c.offset.Load()))
	return sample
}

func (c *gcFlowClock) Bounds(ctx context.Context) (storage.R1Bounds, error) {
	now := c.Now().WallUTC
	return storage.R1Bounds{Lower: now.Add(-time.Millisecond), Upper: now.Add(time.Millisecond), Horizon: now.Add(time.Hour)}, ctx.Err()
}

type gcFlowPauseKey struct{}

type gcFlowPause struct {
	operation, match      string
	reached, resume, done chan struct{}
	stop                  sync.Once
	arrived               atomic.Bool
	err                   error
}

func (p *gcFlowPause) block(ctx context.Context) error {
	if !p.arrived.CompareAndSwap(false, true) {
		return nil
	}
	close(p.reached)
	select {
	case <-p.resume:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func pauseGCFlow(t *testing.T, ctx context.Context, operation, match string, run func(context.Context, *gcFlowPause) error) *gcFlowPause {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	p := &gcFlowPause{operation: operation, match: match, reached: make(chan struct{}), resume: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(func() {
		cancel()
		p.release()
		select {
		case <-p.done:
		case <-time.After(10 * time.Second):
			t.Error("paused request did not exit")
		}
	})
	go func() {
		defer close(p.done)
		p.err = run(context.WithValue(ctx, gcFlowPauseKey{}, p), p)
	}()
	select {
	case <-p.reached:
		return p
	case <-p.done:
		t.Fatalf("request exited before pause: %v", p.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	case <-time.After(15 * time.Second):
		t.Fatal("request did not reach pause")
	}
	return nil
}

func (p *gcFlowPause) release() { p.stop.Do(func() { close(p.resume) }) }
func (p *gcFlowPause) finish(t *testing.T) {
	t.Helper()
	p.release()
	select {
	case <-p.done:
		if p.err != nil {
			t.Fatal(p.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("request did not resume")
	}
}

// Only requests tagged by the test can stop. All SDK calls, including the
// paused one, still go through the configured native client.
type gcFlowTransport struct{ http.RoundTripper }

func (h gcFlowTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p, _ := r.Context().Value(gcFlowPauseKey{}).(*gcFlowPause)
	if p != nil && p.operation != "" && !p.arrived.Load() && strings.HasSuffix(r.Header.Get("X-Amz-Target"), "."+p.operation) && r.Body != nil {
		body, e := io.ReadAll(r.Body)
		closeErr := r.Body.Close()
		if e != nil {
			return nil, e
		}
		if closeErr != nil {
			return nil, closeErr
		}
		r = r.Clone(r.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		if bytes.Contains(body, []byte(p.match)) {
			if e = p.block(r.Context()); e != nil {
				return nil, e
			}
		}
	}
	return h.RoundTripper.RoundTrip(r)
}

func nativeWarmGCFlows(t *testing.T, ctx context.Context, nodes []Dependencies, m *dynamodb.Store, client *s3.Client, packBucket string, bucket storage.Bucket, clk *gcFlowClock, policy config.GlobalSnapshot) {
	const credential = "RUNTIMEKEY00000000001"
	cloud, e := nodes[0].ObjectsForRequest(ctx, bucket.Name)
	if e != nil {
		t.Fatal(e)
	}
	edge, e := nodes[2].ObjectsForRequest(ctx, bucket.Name)
	if e != nil {
		t.Fatal(e)
	}
	put := func(ctx context.Context, svc *object.Service, key string, data []byte) error {
		_, e := svc.PutObject(ctx, object.PutRequest{Bucket: bucket, Key: key, UserID: "owner", CredentialID: credential, ContentLength: int64(len(data)), Body: bodyForNative(data)})
		return e
	}
	mustPut := func(t *testing.T, key string, data []byte) {
		t.Helper()
		if e := put(ctx, cloud, key, data); e != nil {
			t.Fatal(e)
		}
	}
	remove := func(t *testing.T, key string) {
		t.Helper()
		if e := cloud.DeleteObject(ctx, object.DeleteRequest{Bucket: bucket, Key: key, UserID: "owner", CredentialID: credential}); e != nil {
			t.Fatal(e)
		}
	}
	read := func(ctx context.Context, svc *object.Service, key string, ready func(object.GetResult) error) ([]byte, error) {
		var out bytes.Buffer
		_, e := svc.GetObject(ctx, object.GetRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: key, CredentialID: credential, Ready: ready, Emit: func(_ context.Context, p []byte) error { _, e := out.Write(p); return e }})
		return out.Bytes(), e
	}
	verify := func(t *testing.T, key string, want []byte) {
		t.Helper()
		got, e := read(ctx, edge, key, nil)
		if e != nil || !bytes.Equal(got, want) || sha256.Sum256(got) != sha256.Sum256(want) {
			t.Fatalf("read %s: got %x want %x: %v", key, sha256.Sum256(got), sha256.Sum256(want), e)
		}
	}
	canonical := func(t *testing.T, data []byte) storage.Chunk {
		t.Helper()
		key, e := engine.HashChunk(bucket.Domain, data)
		if e != nil {
			t.Fatal(e)
		}
		rows, e := m.GetChunks(ctx, []storage.ChunkKey{key}, true)
		row, ok := rows[key]
		if e != nil || !ok {
			t.Fatal(rows, e)
		}
		return row
	}
	inventory := packs3.VersionInventory{Client: client, Bucket: packBucket}
	versions := func(t *testing.T, ref storage.PackRef) map[storage.SweepVersion]bool {
		t.Helper()
		out, e := inventory.CollectInventory(ctx, 1024)
		if e != nil || !out.Complete {
			t.Fatal(out.Complete, e)
		}
		result := map[storage.SweepVersion]bool{}
		for _, o := range out.Objects {
			if o.Pack == ref && (o.Category == "pack" || o.Category == "sidecar") {
				if o.VersionID == "" || o.VersionID == "null" {
					t.Fatal("expected versioned object", o)
				}
				result[storage.SweepVersion{Category: o.Category, VersionID: o.VersionID}] = true
			}
		}
		return result
	}
	present := func(t *testing.T, ref storage.PackRef) map[storage.SweepVersion]bool {
		t.Helper()
		got := versions(t, ref)
		if len(got) != 2 {
			t.Fatal("expected exact pack and sidecar", ref, got)
		}
		return got
	}
	run := func(t *testing.T, kind jobs.Type, ref storage.PackRef) (jobs.Step, error) {
		t.Helper()
		runner := nodes[0].Operations.Cluster.Runner
		if e := nodes[0].Operations.Registry.Tick(ctx, new(Admission), jobs.NodeStatus{Ready: true, AppliedPolicyRevision: 1}); e != nil {
			t.Fatal(e)
		}
		params, e := json.Marshal(lifecycle.Params{PackID: ref.ID, Generation: ref.Generation})
		if e != nil {
			t.Fatal(e)
		}
		j, e := runner.Create(ctx, jobs.Create{Type: kind, Params: params})
		if e != nil {
			t.Fatal(e)
		}
		fence, e := runner.Authority.Acquire(ctx, j.ID, "control")
		if e != nil {
			t.Fatal(e)
		}
		closed := false
		defer func() {
			if !closed {
				if e := runner.Authority.Close(ctx, fence); e != nil {
					t.Error(e)
				}
			}
		}()
		change := jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "claim", Fence: fence, NodeID: runner.Node.ID, NodeIncarnation: runner.Node.Incarnation}
		j, e = m.ChangeJob(ctx, change)
		if e != nil {
			t.Fatal(e)
		}
		change.ExpectedRevision = j.Revision
		change.Kind = "begin_step"
		change.StepID = strings.Repeat("1", 32)
		j, e = m.ChangeJob(ctx, change)
		if e != nil {
			t.Fatal(e)
		}
		out, e := runner.Executors[kind].Step(ctx, j, policy)
		if kind != "compaction" || e != nil || !out.Done || !out.Settled {
			return out, e
		}
		// Simulate losing the Jobs checkpoint ACK after durable compaction.
		// Use the composed runtime authority, including pin cleanup, rather
		// than granting a synthetic replacement fence in the test.
		if e = runner.Authority.Close(ctx, fence); e != nil {
			return jobs.Step{}, e
		}
		closed = true
		next, e := runner.Authority.Acquire(ctx, j.ID, "control")
		if e != nil {
			return jobs.Step{}, e
		}
		defer func() {
			if e := runner.Authority.Close(ctx, next); e != nil {
				t.Error(e)
			}
		}()
		if next.Epoch <= fence.Epoch {
			return jobs.Step{}, fmt.Errorf("takeover did not advance epoch")
		}
		old := j
		for _, change := range []string{"claim", "begin_step"} {
			j, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: change, Fence: next, NodeID: runner.Node.ID, NodeIncarnation: runner.Node.Incarnation, StepID: strings.Repeat("2", 32)})
			if e != nil {
				return jobs.Step{}, e
			}
		}
		if _, e = runner.Executors[kind].Step(ctx, old, policy); !errors.Is(e, storage.ErrFenced) {
			return jobs.Step{}, fmt.Errorf("old compaction worker was not fenced: %v", e)
		}
		return runner.Executors[kind].Step(ctx, j, policy)
	}
	succeeds := func(t *testing.T, kind jobs.Type, ref storage.PackRef) {
		t.Helper()
		step, e := run(t, kind, ref)
		if e != nil || !step.Settled || !step.Done || step.ErrorCode != "" {
			t.Fatal(kind, step, e)
		}
	}
	defers := func(t *testing.T, ref storage.PackRef) {
		t.Helper()
		step, e := run(t, "gc", ref)
		if e != nil || !step.Settled || step.Done || !bytes.Contains(step.Checkpoint, []byte("r1_busy_or_retained")) {
			t.Fatal(step, e)
		}
	}
	t.Run("put_reuse_and_retirement", func(t *testing.T) {
		data := []byte("object awaiting reuse")
		mustPut(t, "reuse-old", data)
		old := canonical(t, data)
		remove(t, "reuse-old")
		clk.offset.Add(int64(2 * time.Hour))
		paused := pauseGCFlow(t, ctx, "BatchGetItem", "Chunks", func(ctx context.Context, _ *gcFlowPause) error { return put(ctx, edge, "reuse-result", data) })
		gate, e := m.GetR1Gate(ctx)
		if e != nil || gate.Active != 1 || gate.State != "Open" {
			t.Fatal("expected the paused PUT to hold a writer lease", gate, e)
		}
		defers(t, old.Location.Pack)
		if canonical(t, data) != old {
			t.Fatal("canonical location changed under writer")
		}
		present(t, old.Location.Pack)
		paused.finish(t)
		verify(t, "reuse-result", data)
		if canonical(t, data) != old {
			t.Fatal("PUT did not reuse the retained winner")
		}

		data = []byte("object reinserted after retirement")
		mustPut(t, "retire-old", data)
		old = canonical(t, data)
		remove(t, "retire-old")
		clk.offset.Add(int64(2 * time.Hour))
		succeeds(t, "gc", old.Location.Pack)
		rows, e := m.GetChunks(ctx, []storage.ChunkKey{old.Key}, true)
		if e != nil || len(rows) != 0 {
			t.Fatal(rows, e)
		}
		mustPut(t, "retire-result", data)
		current := canonical(t, data)
		if current.Location.Pack == old.Location.Pack || current.LocationRevision <= old.LocationRevision+1 {
			t.Fatal(old, current)
		}
		verify(t, "retire-result", data)
	})
	t.Run("get_copy_compaction_and_version_sweep", func(t *testing.T) {
		oldData := []byte("captured original source")
		newData := []byte("overwritten source bytes")
		mustPut(t, "source", oldData)
		mustPut(t, "keeper", oldData)
		old := canonical(t, oldData)
		before := present(t, old.Location.Pack)
		clk.offset.Add(int64(2 * time.Hour))
		var captured []byte
		get := pauseGCFlow(t, ctx, "", "", func(ctx context.Context, p *gcFlowPause) error {
			var e error
			captured, e = read(ctx, edge, "source", func(object.GetResult) error { return p.block(ctx) })
			return e
		})
		copy := pauseGCFlow(t, ctx, "GetItem", "copy-destination", func(ctx context.Context, _ *gcFlowPause) error {
			_, e := edge.CopyObject(ctx, object.CopyRequest{Source: bucket, SourceKey: "source", Bucket: bucket, Key: "copy-destination", UserID: "owner", CredentialID: credential})
			return e
		})
		pinned, e := m.ObservePackPins(ctx, old.Location.Pack)
		if e != nil || !pinned {
			t.Fatal("GET did not pin the captured source generation", e)
		}
		mustPut(t, "source", newData)
		defers(t, old.Location.Pack)
		succeeds(t, "compaction", old.Location.Pack)
		next := canonical(t, oldData)
		if next.Location.Pack == old.Location.Pack {
			t.Fatal("compaction did not relocate the captured chunks")
		}
		retained := present(t, old.Location.Pack)
		for v := range before {
			if !retained[v] {
				t.Fatal("lost protected version", v)
			}
		}
		get.finish(t)
		copy.finish(t)
		gate, e := m.GetR1Gate(ctx)
		if e != nil || gate.Active != 0 || gate.Resolving != 0 {
			t.Fatal("request protection leaked", gate, e)
		}
		if !bytes.Equal(captured, oldData) || sha256.Sum256(captured) != sha256.Sum256(oldData) {
			t.Fatal("GET lost the captured source", captured)
		}
		verify(t, "source", newData)
		verify(t, "copy-destination", oldData)
		succeeds(t, "gc", old.Location.Pack)
		defers(t, old.Location.Pack)
		clk.offset.Add(int64(25 * time.Hour))
		sealWarmGCCut(t, ctx, nodes[0].Journal)
		nativeProvisionedCheckpoint(t, ctx, nodes[0])
		currentVersions := present(t, next.Location.Pack)
		succeeds(t, "gc", old.Location.Pack)
		if remaining := versions(t, old.Location.Pack); len(remaining) != 0 {
			t.Fatal("old exact versions survived sweep", before, remaining)
		}
		remainingCurrent := present(t, next.Location.Pack)
		for v := range currentVersions {
			if !remainingCurrent[v] {
				t.Fatal("sweep removed current version", v)
			}
		}
		verify(t, "source", newData)
		verify(t, "copy-destination", oldData)
		verify(t, "keeper", oldData)
	})
}

func sealWarmGCCut(t *testing.T, ctx context.Context, j *journalRuntime) {
	t.Helper()
	fence, e := j.authority.Acquire(ctx, "gc-flow-cut", "recovery")
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := j.authority.Close(ctx, fence); e != nil {
			t.Error(e)
		}
	}()
	gate, e := j.store.GetR1Gate(ctx)
	if e != nil {
		t.Fatal(e)
	}
	worker := recovery.Worker{Store: j.store, Payloads: j.store, Objects: j.objects, Catalog: j.catalog, Clock: j.clock, Fence: fence}
	if _, e = worker.CloseAndSeal(ctx, storage.RecoveryCutRequest{HistoryID: gate.HistoryID, Epoch: gate.Epoch, CloseID: strings.Repeat("4", 32), Worker: fence}); e != nil {
		t.Fatal("seal post-retirement cut", e)
	}
}
