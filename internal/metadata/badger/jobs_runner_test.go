package badger

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

type runnerLeases struct {
	sync.Mutex
	store  *Store
	epochs map[string]uint64
	live   map[string]bool
}

func (a *runnerLeases) Acquire(ctx context.Context, id, kind string) (s.WriterFence, error) {
	a.Lock()
	defer a.Unlock()
	if a.live[id] {
		return s.WriterFence{}, s.ErrR1Busy
	}
	a.epochs[id]++
	a.live[id] = true
	f := s.WriterFence{LeaseID: id, ServiceIncarnation: "history-1", Epoch: a.epochs[id]}
	err := a.store.update(ctx, func(t *db.Txn) error {
		return put(t, key("writers", id), s.WriterLease{Fence: f, ExpiresAt: opstest.Now.Add(time.Hour)})
	})
	return f, err
}
func (a *runnerLeases) Renew(_ context.Context, f s.WriterFence) error {
	a.Lock()
	defer a.Unlock()
	if !a.live[f.LeaseID] || a.epochs[f.LeaseID] != f.Epoch {
		return s.ErrFenced
	}
	return nil
}
func (a *runnerLeases) Close(ctx context.Context, f s.WriterFence) error {
	a.Lock()
	defer a.Unlock()
	if a.epochs[f.LeaseID] != f.Epoch {
		return s.ErrFenced
	}
	a.live[f.LeaseID] = false
	return a.store.update(ctx, func(t *db.Txn) error {
		return put(t, key("writers", f.LeaseID), s.WriterLease{Fence: f, ExpiresAt: opstest.Now.Add(-time.Second)})
	})
}
func newRunnerFixture(t *testing.T) (*Store, *runnerLeases, *jobs.Runner, *jobs.Runner) {
	t.Helper()
	m, e := Open(Options{Path: t.TempDir(), Now: func() time.Time { return opstest.Now }})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close() })
	e = m.update(context.Background(), func(tx *db.Txn) error {
		if e := put(tx, policyKey(1), opstest.Policy(1)); e != nil {
			return e
		}
		return put(tx, "policy-current", uint64(1))
	})
	if e != nil {
		t.Fatal(e)
	}
	leases := &runnerLeases{store: m, epochs: map[string]uint64{}, live: map[string]bool{}}
	runners := []*jobs.Runner{}
	for _, id := range []string{"node-a", "node-b"} {
		node := opstest.Node(id)
		_, e = m.PutNode(context.Background(), 0, node)
		if e != nil {
			t.Fatal(e)
		}
		clk, _ := clock.NewVirtual(opstest.Now, 10000)
		runners = append(runners, &jobs.Runner{Service: jobs.Service{Store: m, Policies: m, Now: func() time.Time { return opstest.Now }}, Authority: leases, Node: node, Clock: clk, Executors: map[jobs.Type]jobs.Executor{}})
	}
	return m, leases, runners[0], runners[1]
}
func TestRunnerCancelAtSettledCheckpointAndNoUnavailableExecutor(t *testing.T) {
	m, _, r, _ := newRunnerFixture(t)
	ctx := context.Background()
	r.Executors["gc"] = jobs.Executor{Step: func(ctx context.Context, j jobs.Job, _ config.GlobalSnapshot) (jobs.Step, error) {
		if _, e := r.Service.Cancel(ctx, j.ID); e != nil {
			t.Fatal(e)
		}
		return jobs.Step{Settled: true, Checkpoint: json.RawMessage(`{"page":1}`)}, nil
	}}
	if _, e := r.Create(ctx, jobs.Create{Type: "tiering"}); !errors.Is(e, jobs.ErrUnsupported) {
		t.Fatal(e)
	}
	j, e := r.Create(ctx, jobs.Create{Type: "gc"})
	if e != nil {
		t.Fatal(e)
	}
	if e = r.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	out, e := m.GetJob(ctx, j.ID)
	if e != nil || out.State != jobs.Cancelled || out.PendingStep != "" || string(out.Checkpoint) != `{"page":1}` {
		t.Fatal(out, e)
	}
}
func TestRunnerTakeoverAtSafePointAndPreservesUnknownWork(t *testing.T) {
	for _, settled := range []bool{true, false} {
		t.Run(map[bool]string{true: "safe_checkpoint", false: "unsettled"}[settled], func(t *testing.T) {
			m, leases, a, b := newRunnerFixture(t)
			ctx := context.Background()
			calls := 0
			step := func(_ context.Context, j jobs.Job, _ config.GlobalSnapshot) (jobs.Step, error) {
				calls++
				if calls == 1 {
					return jobs.Step{Settled: settled, Checkpoint: json.RawMessage(`{"page":1}`)}, nil
				}
				return jobs.Step{Settled: true, Done: true, Result: json.RawMessage(`{"verified":1}`)}, nil
			}
			a.Executors["gc"] = jobs.Executor{Step: step}
			b.Executors["gc"] = jobs.Executor{Step: step}
			j, e := a.Create(ctx, jobs.Create{Type: "gc"})
			if e != nil {
				t.Fatal(e)
			}
			if e = a.Tick(ctx); e != nil {
				t.Fatal(e)
			}
			old, e := m.GetJob(ctx, j.ID)
			if e != nil {
				t.Fatal(e)
			}
			if e = leases.Close(ctx, old.Worker); e != nil {
				t.Fatal(e)
			}
			if e = b.Tick(ctx); e != nil {
				t.Fatal(e)
			}
			out, e := m.GetJob(ctx, j.ID)
			if e != nil {
				t.Fatal(e)
			}
			if settled {
				if calls != 2 || out.State != jobs.Succeeded || out.Worker.Epoch != 2 {
					t.Fatal(out, calls)
				}
			} else {
				if calls != 1 || out.PendingStep == "" || out.State != jobs.Running {
					t.Fatal("unknown work replayed or cleared", out, calls)
				}
			}
			_, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: out.Revision, Kind: "finish", Fence: old.Worker, NodeID: a.Node.ID, NodeIncarnation: a.Node.Incarnation})
			if !errors.Is(e, s.ErrFenced) {
				t.Fatal("old owner retained authority", e)
			}
		})
	}
}
func TestRunnerPeriodicCreationIsExclusiveAcrossGateways(t *testing.T) {
	m, _, a, b := newRunnerFixture(t)
	ctx := context.Background()
	exec := jobs.Executor{Periodic: time.Minute, Step: func(context.Context, jobs.Job, config.GlobalSnapshot) (jobs.Step, error) {
		return jobs.Step{Settled: true, Done: true}, nil
	}}
	a.Executors["gc"] = exec
	b.Executors["gc"] = exec
	var group sync.WaitGroup
	for _, r := range []*jobs.Runner{a, b} {
		group.Add(1)
		go func(r *jobs.Runner) { defer group.Done(); _ = r.Tick(ctx) }(r)
	}
	group.Wait()
	list, e := jobs.Inventory(ctx, m)
	if e != nil || len(list) != 1 {
		t.Fatal(len(list), e)
	}
	if e = a.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	if e = b.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	list, e = jobs.Inventory(ctx, m)
	if e != nil || len(list) != 1 || list[0].State != jobs.Succeeded {
		t.Fatal(list, e)
	}
}
