package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	s "github.com/KinasTomes/miles/internal/storage"
)

type fenceTestStore struct {
	Store
	node Node
	jobs map[string]Job
	list []Job
}

func (st *fenceTestStore) GetNode(_ context.Context, id string) (Node, error) {
	if st.node.ID == id {
		return st.node, nil
	}
	return Node{}, s.ErrNotFound
}

func (st *fenceTestStore) GetJob(_ context.Context, id string) (Job, error) {
	if j, ok := st.jobs[id]; ok {
		return j, nil
	}
	return Job{}, s.ErrNotFound
}

func (st *fenceTestStore) ListJobs(_ context.Context, _ PageRequest) (JobPage, error) {
	return JobPage{Jobs: st.list}, nil
}

func (st *fenceTestStore) GetPolicy(_ context.Context, _ uint64) (s.PolicySnapshot, error) {
	return s.PolicySnapshot{}, nil
}

type fenceTestAuthority struct {
	closeErr map[string]error
	closes   map[string]int
}

func (a *fenceTestAuthority) Acquire(_ context.Context, id, _ string) (s.WriterFence, error) {
	return s.WriterFence{LeaseID: id, Epoch: 1, ServiceIncarnation: "svc"}, nil
}

func (a *fenceTestAuthority) Renew(context.Context, s.WriterFence) error { return nil }

func (a *fenceTestAuthority) Close(_ context.Context, f s.WriterFence) error {
	if a.closes == nil {
		a.closes = map[string]int{}
	}
	a.closes[f.LeaseID]++
	if err, ok := a.closeErr[f.LeaseID]; ok {
		return err
	}
	return nil
}

func TestRunnerTickEvictsFencedLeaseOnTerminalJob(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	clk, _ := clock.NewVirtual(now, 100)
	node := Node{ID: "node-1", Incarnation: "boot-1", Ready: true}
	fence := s.WriterFence{LeaseID: "job-fenced", Epoch: 1, ServiceIncarnation: "svc"}
	terminalJob := Job{ID: "job-fenced", Type: "purge_uploads", State: Cancelled, Worker: fence}

	st := &fenceTestStore{node: node, jobs: map[string]Job{"job-fenced": terminalJob}}
	auth := &fenceTestAuthority{closeErr: map[string]error{"job-fenced": s.ErrFenced}}
	runner := &Runner{
		Service:   Service{Store: st, Policies: st, Now: func() time.Time { return now }},
		Authority: auth,
		Clock:     clk,
		Node:      node,
		leases:    map[string]s.WriterFence{"job-fenced": fence},
		Executors: map[Type]Executor{},
	}

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("expected Tick to ignore ErrFenced on terminal lease cleanup, got %v", err)
	}

	if _, exists := runner.leases["job-fenced"]; exists {
		t.Fatalf("expected job-fenced to be evicted from leases, but it was retained")
	}
	if auth.closes["job-fenced"] != 1 {
		t.Fatalf("expected exactly 1 Close attempt, got %d", auth.closes["job-fenced"])
	}
}

func TestRunnerTickEvictsNotFoundLeaseOnTerminalJob(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	clk, _ := clock.NewVirtual(now, 100)
	node := Node{ID: "node-1", Incarnation: "boot-1", Ready: true}
	fence := s.WriterFence{LeaseID: "job-missing", Epoch: 1, ServiceIncarnation: "svc"}
	terminalJob := Job{ID: "job-missing", Type: "purge_uploads", State: Cancelled, Worker: fence}

	st := &fenceTestStore{node: node, jobs: map[string]Job{"job-missing": terminalJob}}
	auth := &fenceTestAuthority{closeErr: map[string]error{"job-missing": s.ErrNotFound}}
	runner := &Runner{
		Service:   Service{Store: st, Policies: st, Now: func() time.Time { return now }},
		Authority: auth,
		Clock:     clk,
		Node:      node,
		leases:    map[string]s.WriterFence{"job-missing": fence},
		Executors: map[Type]Executor{},
	}

	if err := runner.Tick(context.Background()); err != nil {
		t.Fatalf("expected Tick to ignore ErrNotFound on terminal lease cleanup, got %v", err)
	}

	if _, exists := runner.leases["job-missing"]; exists {
		t.Fatalf("expected job-missing to be evicted from leases, but it was retained")
	}
}

func TestRunnerTickRetainsLeaseOnTransientError(t *testing.T) {
	now := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	clk, _ := clock.NewVirtual(now, 100)
	node := Node{ID: "node-1", Incarnation: "boot-1", Ready: true}
	fence := s.WriterFence{LeaseID: "job-transient", Epoch: 1, ServiceIncarnation: "svc"}
	terminalJob := Job{ID: "job-transient", Type: "purge_uploads", State: Cancelled, Worker: fence}

	st := &fenceTestStore{node: node, jobs: map[string]Job{"job-transient": terminalJob}}
	auth := &fenceTestAuthority{closeErr: map[string]error{"job-transient": s.ErrUnavailable}}
	runner := &Runner{
		Service:   Service{Store: st, Policies: st, Now: func() time.Time { return now }},
		Authority: auth,
		Clock:     clk,
		Node:      node,
		leases:    map[string]s.WriterFence{"job-transient": fence},
		Executors: map[Type]Executor{},
	}

	err := runner.Tick(context.Background())
	if !errors.Is(err, s.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}

	if _, exists := runner.leases["job-transient"]; !exists {
		t.Fatalf("expected job-transient to be retained in leases on transient error")
	}
}

func TestRunnerCloseIgnoresFencedAndNotFound(t *testing.T) {
	node := Node{ID: "node-1", Incarnation: "boot-1"}
	f1 := s.WriterFence{LeaseID: "j1", Epoch: 1, ServiceIncarnation: "svc"}
	f2 := s.WriterFence{LeaseID: "j2", Epoch: 1, ServiceIncarnation: "svc"}
	j1 := Job{ID: "j1", Worker: f1}
	j2 := Job{ID: "j2", Worker: f2}

	st := &fenceTestStore{node: node, jobs: map[string]Job{"j1": j1, "j2": j2}}
	auth := &fenceTestAuthority{
		closeErr: map[string]error{
			"j1": s.ErrFenced,
			"j2": s.ErrNotFound,
		},
	}
	runner := &Runner{
		Service:   Service{Store: st},
		Authority: auth,
		Node:      node,
		leases:    map[string]s.WriterFence{"j1": f1, "j2": f2},
	}

	if err := runner.Close(context.Background()); err != nil {
		t.Fatalf("expected Close to ignore ErrFenced and ErrNotFound, got %v", err)
	}
	if len(runner.leases) != 0 {
		t.Fatalf("expected all leases to be cleared in Close, got %v", runner.leases)
	}
}
