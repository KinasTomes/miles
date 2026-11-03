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
func TestRunnerRetiresTakenOverLeaseAndAcceptsNewWork(t *testing.T) {
	for _, successorDone := range []bool{false, true} {
		t.Run(map[bool]string{false: "running_successor", true: "terminal_successor"}[successorDone], func(t *testing.T) {
			m, leases, a, b := newRunnerFixture(t)
			ctx := t.Context()
			calls := 0
			step := func(_ context.Context, j jobs.Job, _ config.GlobalSnapshot) (jobs.Step, error) {
				calls++
				return jobs.Step{Settled: true, Done: calls > 1 && successorDone, Checkpoint: json.RawMessage(`{"page":1}`)}, nil
			}
			a.Executors["cache_purge"] = jobs.Executor{Step: step}
			b.Executors["cache_purge"] = jobs.Executor{Step: step}
			first, err := a.Create(ctx, jobs.Create{Type: "cache_purge"})
			if err != nil {
				t.Fatal(err)
			}
			if err = a.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			old, err := m.GetJob(ctx, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = leases.Close(ctx, old.Worker); err != nil {
				t.Fatal(err)
			}
			if err = b.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			successor, err := m.GetJob(ctx, first.ID)
			if err != nil || successor.WorkerNodeID != b.Node.ID {
				t.Fatal(successor, err)
			}
			if !successorDone {
				if err := a.Close(ctx); err != nil {
					t.Fatal("shutdown tried to close successor authority", err)
				}
			}
			// The old process resumes while the successor owns the authoritative fence.
			for range 2 {
				if err = a.Tick(ctx); err != nil {
					t.Fatal("old fence wedged resumed worker", err)
				}
			}
			a.Executors["gc"] = jobs.Executor{Step: func(context.Context, jobs.Job, config.GlobalSnapshot) (jobs.Step, error) {
				return jobs.Step{Settled: true, Done: true}, nil
			}}
			next, err := a.Create(ctx, jobs.Create{Type: "gc"})
			if err != nil {
				t.Fatal(err)
			}
			if err = a.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := m.GetJob(ctx, next.ID)
			if err != nil || got.State != jobs.Succeeded || got.WorkerNodeID != a.Node.ID {
				t.Fatal("resumed worker did not handle fresh work", got, err)
			}
			leases.Lock()
			live := leases.live[first.ID]
			leases.Unlock()
			if !live {
				t.Fatal("old worker closed successor authority")
			}
			if err = a.Close(ctx); err != nil {
				t.Fatal("stale authority poisoned shutdown", err)
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
	page, e := m.ListJobs(ctx, jobs.PageRequest{Limit: 1000})
	list := page.Jobs
	if e != nil || len(list) != 1 {
		t.Fatal(len(list), e)
	}
	if e = a.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	if e = b.Tick(ctx); e != nil {
		t.Fatal(e)
	}
	page, e = m.ListJobs(ctx, jobs.PageRequest{Limit: 1000})
	list = page.Jobs
	if e != nil || len(list) != 1 || list[0].State != jobs.Succeeded {
		t.Fatal(list, e)
	}
}

type heartbeatTestClock struct {
	clock.Clock
	waiting chan struct{}
	due     chan struct{}
}

func (c heartbeatTestClock) Sleep(ctx context.Context, _ time.Duration) error {
	select {
	case c.waiting <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-c.due:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type stopRenewal struct {
	jobs.Authority
	calls   int
	renewed chan struct{}
}

func (a *stopRenewal) Renew(ctx context.Context, f s.WriterFence) error {
	a.calls++
	if a.calls > 1 {
		close(a.renewed)
		return s.ErrFenced
	}
	return a.Authority.Renew(ctx, f)
}
func TestRunnerHeartbeatFailureCancelsAndJoinsTheActiveStep(t *testing.T) {
	m, _, r, _ := newRunnerFixture(t)
	ctx := context.Background()
	c := heartbeatTestClock{Clock: r.Clock, waiting: make(chan struct{}, 1), due: make(chan struct{}, 1)}
	r.Clock = c
	authority := &stopRenewal{Authority: r.Authority, renewed: make(chan struct{})}
	r.Authority = authority
	stopped := make(chan struct{})
	r.Executors["gc"] = jobs.Executor{Step: func(ctx context.Context, _ jobs.Job, _ config.GlobalSnapshot) (jobs.Step, error) {
		<-ctx.Done()
		close(stopped)
		return jobs.Step{}, ctx.Err()
	}}
	j, e := r.Create(ctx, jobs.Create{Type: "gc"})
	if e != nil {
		t.Fatal(e)
	}
	finished := make(chan error, 1)
	go func() { finished <- r.Tick(ctx) }()
	select {
	case <-c.waiting:
	case <-time.After(time.Second):
		t.Fatal("heartbeat never started")
	}
	c.due <- struct{}{}
	select {
	case <-authority.renewed:
	case <-time.After(time.Second):
		t.Fatal("long step was not renewed")
	}
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("worker did not join cancelled step")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("step still active after worker returned")
	}
	out, e := m.GetJob(ctx, j.ID)
	if e != nil || out.State != jobs.Running || out.PendingStep == "" {
		t.Fatal("lost authority incorrectly checkpointed unknown work", out, e)
	}
	if e = r.Close(ctx); e != nil {
		t.Fatal(e)
	}
}

func TestRunnerManagedCreationRequiresLiveAuthority(t *testing.T) {
	m, a, r, _ := newRunnerFixture(t)
	ctx := context.Background()
	f, e := a.Acquire(ctx, "schedule_gc", "control")
	if e != nil {
		t.Fatal(e)
	}
	if e = a.Close(ctx, f); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"periodic_gc_123", "child_123"} {
		_, e = r.Service.CreateNamed(jobs.WithCreationFence(ctx, f), jobs.Create{Type: "gc"}, id)
		if !errors.Is(e, s.ErrFenced) {
			t.Fatal("closed creator enqueued work", id, e)
		}
		_, e = r.Service.CreateNamed(ctx, jobs.Create{Type: "gc"}, id)
		if !errors.Is(e, s.ErrFenced) {
			t.Fatal("managed job created without authority", id, e)
		}
		if _, e = m.GetJob(ctx, id); !errors.Is(e, s.ErrNotFound) {
			t.Fatal("failed managed create left a record", id, e)
		}
	}
}
