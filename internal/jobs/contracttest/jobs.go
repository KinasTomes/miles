// Package contracttest runs the same operational-state tests against durable adapters.
package contracttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	s "github.com/KinasTomes/miles/internal/storage"
)

var Now = time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC)

type Harness struct {
	Store, Peer  jobs.Store
	Policies     jobs.PolicyReader
	SetPolicy    func(s.PolicySnapshot)
	RemovePolicy func(uint64)
	SetLease     func(s.WriterLease)
}

func Policy(revision uint64) s.PolicySnapshot {
	b, _ := json.Marshal(config.DefaultGlobalPolicy())
	return s.PolicySnapshot{Version: 1, Revision: revision, Document: b}
}
func Node(id string) jobs.Node {
	return jobs.Node{ID: id, Incarnation: "boot-" + id, ServiceIncarnation: "history-1", Profile: "cloud", Address: "10.0.1.5:9000", HeartbeatAt: Now, Ready: true, AppliedPolicyRevision: 1}
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func want(t *testing.T, e, target error) {
	t.Helper()
	if !errors.Is(e, target) {
		t.Fatalf("got %v, want %v", e, target)
	}
}

func Run(t *testing.T, factory func(*testing.T) Harness) {
	setup := func(t *testing.T) (Harness, jobs.Job, jobs.Change) {
		t.Helper()
		h := factory(t)
		h.SetPolicy(Policy(1))
		for _, id := range []string{"node-a", "node-b"} {
			_, e := h.Store.PutNode(context.Background(), 0, Node(id))
			must(t, e)
		}
		service := jobs.Service{Store: h.Store, Policies: h.Policies, Now: func() time.Time { return Now }}
		j, e := service.Create(context.Background(), jobs.Create{Type: "gc", DryRun: true})
		must(t, e)
		f := s.WriterFence{LeaseID: j.ID, ServiceIncarnation: "history-1", Epoch: 1}
		h.SetLease(s.WriterLease{Fence: f, ExpiresAt: Now.Add(time.Minute)})
		return h, j, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "claim", NodeID: "node-a", NodeIncarnation: "boot-node-a", Fence: f}
	}
	ctx := context.Background()
	t.Run("shared_create_exact_policy_and_no_current_fallback", func(t *testing.T) {
		h, j, c := setup(t)
		other, e := h.Peer.GetJob(ctx, j.ID)
		must(t, e)
		if other.PolicyRevision != 1 {
			t.Fatal(other)
		}
		h.SetPolicy(Policy(2))
		svc := jobs.Service{Store: h.Store, Policies: h.Policies}
		p, e := svc.ResumePolicy(ctx, other)
		must(t, e)
		if p.Revision() != 1 {
			t.Fatal("policy changed during resume")
		}
		h.RemovePolicy(1)
		_, e = svc.ResumePolicy(ctx, other)
		if e == nil {
			t.Fatal("missing history fell back")
		}
		_, e = h.Store.ChangeJob(ctx, c)
		want(t, e, s.ErrNotFound)
		after, e := h.Peer.GetJob(ctx, j.ID)
		must(t, e)
		if after.State != jobs.Queued {
			t.Fatal("claim modified job despite missing policy")
		}
	})
	t.Run("only_one_concurrent_claim_wins", func(t *testing.T) {
		h, j, c := setup(t)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for _, node := range []string{"node-a", "node-b"} {
			wg.Go(func() {
				r := c
				r.NodeID = node
				r.NodeIncarnation = "boot-" + node
				_, e := h.Store.ChangeJob(ctx, r)
				if e == nil {
					wins.Add(1)
				} else if !errors.Is(e, s.ErrConflict) {
					t.Error(e)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatal("claim winners", wins.Load())
		}
		v, e := h.Peer.GetJob(ctx, j.ID)
		must(t, e)
		if v.State != jobs.Running || v.Revision != 2 {
			t.Fatal(v)
		}
	})
	t.Run("checkpoint_takeover_stale_fence_and_cancel_safe_point", func(t *testing.T) {
		h, _, c := setup(t)
		j, e := h.Store.ChangeJob(ctx, c)
		must(t, e)
		change := func(kind string) jobs.Job {
			t.Helper()
			c.ExpectedRevision = j.Revision
			c.Kind = kind
			out, e := h.Store.ChangeJob(ctx, c)
			must(t, e)
			return out
		}
		c.StepID = "step-a"
		j = change("begin_step")
		cancel := jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "cancel"}
		j, e = h.Peer.ChangeJob(ctx, cancel)
		must(t, e)
		if j.State != jobs.Cancelling {
			t.Fatal("cancelled before safe point")
		}
		c.ExpectedRevision = j.Revision
		c.Kind = "finish"
		_, e = h.Store.ChangeJob(ctx, c)
		want(t, e, jobs.ErrReconciliation)
		c.Checkpoint = json.RawMessage(`{"cursor":"after-pack-a"}`)
		c.Progress = map[string]uint64{"packs_scanned": 1}
		j = change("checkpoint")
		if j.State != jobs.Cancelled || j.PendingStep != "" {
			t.Fatal(j)
		}
		cancel.ExpectedRevision = j.Revision
		again, e := h.Store.ChangeJob(ctx, cancel)
		must(t, e)
		if again.Revision != j.Revision {
			t.Fatal("terminal cancel rewrote record")
		}
	})
	t.Run("safe_resume_uses_new_fence_and_retains_checkpoint", func(t *testing.T) {
		h, _, c := setup(t)
		j, e := h.Store.ChangeJob(ctx, c)
		must(t, e)
		c.Kind = "checkpoint"
		c.ExpectedRevision = j.Revision
		c.Checkpoint = json.RawMessage(`{"cursor":"safe-a"}`)
		j, e = h.Store.ChangeJob(ctx, c)
		must(t, e)
		old := c
		c.Kind = "claim"
		c.ExpectedRevision = j.Revision
		c.NodeID = "node-b"
		c.NodeIncarnation = "boot-node-b"
		c.Fence.Epoch = 2
		h.SetLease(s.WriterLease{Fence: c.Fence, ExpiresAt: Now.Add(time.Minute)})
		j, e = h.Peer.ChangeJob(ctx, c)
		must(t, e)
		if string(j.Checkpoint) != `{"cursor":"safe-a"}` {
			t.Fatal("lost checkpoint")
		}
		old.ExpectedRevision = j.Revision
		old.Kind = "finish"
		_, e = h.Store.ChangeJob(ctx, old)
		want(t, e, s.ErrFenced)
		c.ExpectedRevision = j.Revision
		c.Kind = "finish"
		c.Result = json.RawMessage(`{"checked":1}`)
		j, e = h.Store.ChangeJob(ctx, c)
		must(t, e)
		if j.State != jobs.Succeeded {
			t.Fatal(j)
		}
	})
	t.Run("unsettled_work_and_expired_lease_never_fake_success", func(t *testing.T) {
		h, _, c := setup(t)
		j, e := h.Store.ChangeJob(ctx, c)
		must(t, e)
		c.Kind = "begin_step"
		c.StepID = "pending-op"
		c.ExpectedRevision = j.Revision
		j, e = h.Store.ChangeJob(ctx, c)
		must(t, e)
		c.ExpectedRevision = j.Revision
		c.Kind = "checkpoint"
		c.Checkpoint = json.RawMessage(`{}`)
		c.StepID = "wrong-op"
		_, e = h.Store.ChangeJob(ctx, c)
		want(t, e, s.ErrConflict)
		c.Fence.Epoch = 2
		h.SetLease(s.WriterLease{Fence: c.Fence, ExpiresAt: Now.Add(time.Minute)})
		c.Kind = "claim"
		_, e = h.Store.ChangeJob(ctx, c)
		want(t, e, jobs.ErrReconciliation)
		h.SetLease(s.WriterLease{Fence: c.Fence, ExpiresAt: Now})
		_, e = h.Store.ChangeJob(ctx, c)
		want(t, e, s.ErrFenced)
	})
	t.Run("queued_cancel_and_sticky_node_drain", func(t *testing.T) {
		h, j, _ := setup(t)
		j, e := (jobs.Service{Store: h.Peer}).Cancel(ctx, j.ID)
		must(t, e)
		if j.State != jobs.Cancelled {
			t.Fatal(j)
		}
		n, e := h.Store.GetNode(ctx, "node-a")
		must(t, e)
		n.Draining = true
		n.Ready = false
		n, e = h.Peer.PutNode(ctx, n.Revision, n)
		must(t, e)
		n.Draining = false
		n.Ready = true
		n, e = h.Store.PutNode(ctx, n.Revision, n)
		must(t, e)
		if !n.Draining || n.Ready {
			t.Fatal("heartbeat cleared drain")
		}
		n.Incarnation = "new-boot"
		_, e = h.Peer.PutNode(ctx, n.Revision, n)
		want(t, e, s.ErrFenced)
	})
	t.Run("job_heartbeat_uses_lease_provider_and_drain_blocks_new_steps", func(t *testing.T) {
		h, _, c := setup(t)
		j, e := h.Store.ChangeJob(ctx, c)
		must(t, e)
		c.ExpectedRevision = j.Revision
		renewals := 0
		svc := jobs.Service{Store: h.Store, Policies: h.Policies, Renew: func(_ context.Context, f s.WriterFence) error {
			renewals++
			h.SetLease(s.WriterLease{Fence: f, ExpiresAt: Now.Add(time.Minute)})
			return nil
		}}
		j, e = svc.Heartbeat(ctx, c)
		must(t, e)
		if renewals != 1 || j.HeartbeatAt.IsZero() {
			t.Fatal("lease was not renewed")
		}
		n, e := h.Store.GetNode(ctx, c.NodeID)
		must(t, e)
		n.Draining = true
		n.Ready = false
		_, e = h.Store.PutNode(ctx, n.Revision, n)
		must(t, e)
		c.ExpectedRevision = j.Revision
		c.Kind = "begin_step"
		c.StepID = "new-step"
		_, e = h.Store.ChangeJob(ctx, c)
		want(t, e, s.ErrConflict)
		svc.Renew = func(context.Context, s.WriterFence) error { return s.ErrFenced }
		_, e = svc.Heartbeat(ctx, c)
		want(t, e, s.ErrFenced)
		after, e := h.Peer.GetJob(ctx, j.ID)
		must(t, e)
		if after.Revision != j.Revision {
			t.Fatal("failed heartbeat changed job")
		}
	})
	t.Run("bounded_pages_and_wire_does_not_expose_worker_capability", func(t *testing.T) {
		h, j, _ := setup(t)
		for i := range 5 {
			copy := j
			copy.ID = fmt.Sprintf("page-%d", i)
			must(t, h.Store.CreateJob(ctx, copy))
		}
		seen := map[string]bool{}
		p := jobs.PageRequest{Limit: 2}
		for page := 0; ; page++ {
			if page > 6 {
				t.Fatal("unbounded cursor")
			}
			r, e := h.Peer.ListJobs(ctx, p)
			must(t, e)
			if len(r.Jobs) > 2 {
				t.Fatal("page exceeds limit")
			}
			for _, v := range r.Jobs {
				if seen[v.ID] {
					t.Fatal("duplicate page item")
				}
				seen[v.ID] = true
			}
			if r.Next == "" {
				break
			}
			p.Cursor = r.Next
		}
		if len(seen) != 6 {
			t.Fatal("lost page items", seen)
		}
		wire, e := json.Marshal(j.View())
		must(t, e)
		var fields map[string]any
		must(t, json.Unmarshal(wire, &fields))
		for _, key := range []string{"Worker", "worker", "checkpoint", "Params", "pending_step"} {
			if _, ok := fields[key]; ok {
				t.Fatal("internal fields exposed", key)
			}
		}
		_, e = h.Store.ListNodes(ctx, jobs.PageRequest{Limit: 1001})
		want(t, e, s.ErrInvalid)
	})
}
