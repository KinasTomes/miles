package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
	"time"
)

type yieldStore struct {
	Store
	job    Job
	policy s.PolicySnapshot
	now    time.Time
}

func (st *yieldStore) GetJob(context.Context, string) (Job, error) { return st.job, nil }
func (st *yieldStore) GetPolicy(context.Context, uint64) (s.PolicySnapshot, error) {
	return st.policy, nil
}
func (st *yieldStore) ChangeJob(_ context.Context, c Change) (Job, error) {
	next, e := Apply(st.job, c, st.now)
	if e == nil {
		st.job = next
	}
	return next, e
}

type yieldAuthority struct {
	closes int
	joined *bool
	store  *yieldStore
	t      *testing.T
}

func (a *yieldAuthority) Acquire(_ context.Context, id, _ string) (s.WriterFence, error) {
	return s.WriterFence{LeaseID: id, Epoch: 1, ServiceIncarnation: "service"}, nil
}
func (a *yieldAuthority) Renew(context.Context, s.WriterFence) error { return nil }
func (a *yieldAuthority) Close(context.Context, s.WriterFence) error {
	if !*a.joined || a.store.job.PendingStep != "" {
		a.t.Fatal("released before work joined and checkpoint settled")
	}
	a.closes++
	return nil
}
func TestYieldLifecycleAuthorityOnlyAfterSettledCheckpoint(t *testing.T) {
	for _, settled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown-retains-authority", true: "settled-wait-yields"}[settled], func(t *testing.T) {
			now := time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)
			v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
			snapshot, e := v.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
			if e != nil {
				t.Fatal(e)
			}
			record, _ := snapshot.StorageSnapshot()
			st := &yieldStore{now: now, policy: record, job: Job{ID: "job-yield", Type: "pack_restore", State: Queued, Params: json.RawMessage(`{}`), PolicyRevision: 1, Revision: 1, CreatedAt: now}}
			clk, _ := clock.NewVirtual(now, 100)
			joined := false
			authority := &yieldAuthority{joined: &joined, store: st, t: t}
			runner := Runner{Service: Service{Store: st, Policies: st, Now: func() time.Time { return now }}, Authority: authority, Clock: clk, Node: Node{ID: "node", Incarnation: "boot"}, leases: map[string]s.WriterFence{}}
			executor := Executor{YieldAfterCheckpoint: true, Step: func(context.Context, Job, config.GlobalSnapshot) (Step, error) {
				joined = true
				if !settled {
					return Step{}, s.ErrUnavailable
				}
				return Step{Settled: true, Checkpoint: json.RawMessage(`{"phase":"waiting"}`)}, nil
			}}
			e = runner.advance(t.Context(), st.job, executor)
			if settled {
				if e != nil || authority.closes != 1 || len(runner.leases) != 0 || st.job.State != Running {
					t.Fatal(e, authority.closes, st.job)
				}
			} else if !errors.Is(e, s.ErrUnavailable) || authority.closes != 0 || len(runner.leases) != 1 || st.job.PendingStep == "" {
				t.Fatal(e, authority.closes, st.job)
			}
		})
	}
}
