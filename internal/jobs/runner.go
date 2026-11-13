package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

// Authority owns real R1 leases, including conservative expiry/reclamation.
// Job timestamps and the Nodes heartbeat must never substitute for authority.
type Authority interface {
	Acquire(context.Context, string, string) (storage.WriterFence, error)
	Renew(context.Context, storage.WriterFence) error
	Close(context.Context, storage.WriterFence) error
}

// Step returns only after its synchronous side effects have quiesced. Settled
// explicitly distinguishes a safe checkpoint from an unknown physical outcome.
// Implementations protect physical work with the supplied R1 fence as required.
type Step struct {
	Checkpoint    json.RawMessage
	Progress      map[string]uint64
	Result        json.RawMessage
	Done, Settled bool
	ErrorCode     string
}
type Executor struct {
	YieldAfterCheckpoint bool // release only after a joined, settled nonterminal step
	ResumePending        bool // resume this node's in-flight reconciliation; backend still fences every new dispatch
	Validate             func(json.RawMessage) error
	Eligible             func(Job, Node) bool
	Step                 func(context.Context, Job, config.GlobalSnapshot) (Step, error)
	// Periodic is opt-in: no periodic scrub or destructive job is invented.
	Periodic time.Duration
	Kind     string // control or recovery; never chosen by the HTTP caller
}
type Runner struct {
	Service      Service
	Authority    Authority
	Node         Node
	Clock        clock.Clock
	Executors    map[Type]Executor
	ObserveError func()
	leases       map[string]storage.WriterFence
	slots        map[Type]int64
}

// Each pass is bounded. Large inventories report unavailable, never silently
// drop entries or create a duplicate periodic job based on a partial scan.
func runnableInventory(ctx context.Context, store Store) ([]Job, error) {
	out := []Job{}
	cursor := ""
	for page := 0; page < 1000; page++ {
		p, e := store.ListJobs(ctx, PageRequest{Limit: 100, Cursor: cursor, ActiveOnly: true})
		if e != nil {
			return nil, e
		}
		out = append(out, p.Jobs...)
		if len(out) > 1000 {
			return nil, storage.ErrUnavailable
		}
		if p.Next == "" {
			return out, nil
		}
		if p.Next == cursor {
			return nil, storage.ErrUnavailable
		}
		cursor = p.Next
	}
	return nil, storage.ErrUnavailable
}
func (r *Runner) Create(ctx context.Context, q Create) (Job, error) {
	e, ok := r.Executors[q.Type]
	if !ok || e.Step == nil {
		return Job{}, ErrUnsupported
	}
	if len(q.Params) == 0 {
		q.Params = json.RawMessage(`{}`)
	}
	if e.Validate != nil {
		if err := e.Validate(q.Params); err != nil {
			return Job{}, err
		}
	}
	return r.Service.Create(ctx, q)
}
func (r *Runner) Run(ctx context.Context) (result error) {
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result = errors.Join(result, r.Close(cleanup))
	}()
	for ctx.Err() == nil {
		if e := r.Tick(ctx); e != nil && ctx.Err() == nil && r.ObserveError != nil {
			r.ObserveError()
		}
		if e := r.Clock.Sleep(ctx, 5*time.Second); e != nil {
			return e
		}
	}
	return ctx.Err()
}
func (r *Runner) Tick(ctx context.Context) error {
	if r.Service.Store == nil || r.Authority == nil || r.Clock == nil {
		return storage.ErrInvalid
	}
	n, e := r.Service.Store.GetNode(ctx, r.Node.ID)
	if e != nil {
		return e
	}
	if n.Incarnation != r.Node.Incarnation {
		return storage.ErrFenced
	}
	if r.leases == nil {
		r.leases = map[string]storage.WriterFence{}
	}
	for id, f := range r.leases {
		old, err := r.Service.Store.GetJob(ctx, id)
		if err != nil {
			return err
		}
		if old.Worker != f {
			// Takeover already retired this generation. Closing by its old
			// identity would fence cleanup and stall every subsequent tick.
			delete(r.leases, id)
			continue
		}
		if old.State.Terminal() {
			if err = r.Authority.Close(ctx, f); err != nil {
				return err
			}
			delete(r.leases, id)
		}
	}
	records, e := runnableInventory(ctx, r.Service.Store)
	if e != nil {
		return e
	}
	for t, executor := range r.Executors {
		if executor.Periodic > 0 && n.Ready && !n.Draining && os.Getenv("MILES_DISABLE_PERIODIC") != "1" {
			if err := r.schedule(ctx, t, executor); err != nil && !errors.Is(err, storage.ErrR1Busy) && !errors.Is(err, storage.ErrFenced) {
				return err
			}
		}
	}
	var failures error
	for _, j := range records {
		if j.State.Terminal() {
			if f, ok := r.leases[j.ID]; ok {
				if j.Worker != f {
					delete(r.leases, j.ID)
					continue
				}
				if e = r.Authority.Close(ctx, f); e != nil {
					return e
				}
				delete(r.leases, j.ID)
			}
			continue
		}
		executor, ok := r.Executors[j.Type]
		if !ok || executor.Step == nil {
			continue
		}
		if executor.Eligible != nil && !executor.Eligible(j, r.Node) {
			continue
		}
		resume := executor.ResumePending && j.PendingStep != "" && j.WorkerNodeID == r.Node.ID && j.WorkerNodeIncarnation == r.Node.Incarnation
		if (!n.Ready || n.Draining) && j.Type != "drain" && j.State != Cancelling && !resume {
			continue
		}
		if e = r.advance(ctx, j, executor); e != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !errors.Is(e, storage.ErrR1Busy) && !errors.Is(e, storage.ErrFenced) && !errors.Is(e, storage.ErrConflict) && !errors.Is(e, ErrReconciliation) {
				failures = errors.Join(failures, e)
			}
		}
	}
	return failures
}
func (r *Runner) schedule(ctx context.Context, t Type, exec Executor) error {
	slot := r.Clock.Now().WallUTC.UnixNano() / int64(exec.Periodic)
	if r.slots == nil {
		r.slots = map[Type]int64{}
	}
	if last, ok := r.slots[t]; ok && last == slot {
		return nil
	}
	f, e := r.Authority.Acquire(ctx, "schedule_"+string(t), "control")
	if e != nil {
		return e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.Authority.Close(cleanup, f)
	}()
	// Read again under the scheduler lease: another owner may have just queued.
	records, e := runnableInventory(ctx, r.Service.Store)
	if e != nil {
		return e
	}
	prefix := "periodic_" + string(t) + "_"
	for _, j := range records {
		if strings.HasPrefix(j.ID, prefix) && !j.State.Terminal() {
			r.slots[t] = slot
			return nil
		}
	}
	id := fmt.Sprintf("%s%d", prefix, slot)
	_, e = r.Service.CreateNamed(WithCreationFence(ctx, f), Create{Type: t}, id)
	if e == nil || errors.Is(e, storage.ErrConflict) {
		r.slots[t] = slot
		return nil
	}
	return e
}
func (r *Runner) change(ctx context.Context, id string, f storage.WriterFence, kind string, fill func(*Change)) (Job, error) {
	for range 4 {
		j, e := r.Service.Store.GetJob(ctx, id)
		if e != nil {
			return Job{}, e
		}
		c := Change{JobID: id, ExpectedRevision: j.Revision, Kind: kind, Fence: f, NodeID: r.Node.ID, NodeIncarnation: r.Node.Incarnation}
		if fill != nil {
			fill(&c)
		}
		out, e := r.Service.Change(ctx, c)
		if !errors.Is(e, storage.ErrConflict) {
			return out, e
		}
	}
	return Job{}, storage.ErrConflict
}
func (r *Runner) advance(ctx context.Context, j Job, exec Executor) error {
	f, owned := r.leases[j.ID]
	if !owned {
		var e error
		kind := exec.Kind
		if kind == "" {
			kind = "control"
		}
		f, e = r.Authority.Acquire(ctx, j.ID, kind)
		if e != nil {
			return e
		}
		r.leases[j.ID] = f
		j, e = r.change(ctx, j.ID, f, "claim", nil)
		if e != nil {
			_ = r.Authority.Close(ctx, f)
			delete(r.leases, j.ID)
			return e
		}
	}
	if e := r.Authority.Renew(ctx, f); e != nil {
		delete(r.leases, j.ID)
		return e
	}
	var e error
	j, e = r.change(ctx, j.ID, f, "heartbeat", nil)
	if e != nil {
		return e
	}
	policy, e := r.Service.ResumePolicy(ctx, j)
	if e != nil {
		return e
	}
	if j.PendingStep == "" {
		if j.State == Cancelling {
			_, e = r.change(ctx, j.ID, f, "finish", nil)
			return e
		}
		sum := sha256.Sum256(append([]byte(j.ID), j.Checkpoint...))
		step := hex.EncodeToString(sum[:16])
		j, e = r.change(ctx, j.ID, f, "begin_step", func(c *Change) { c.StepID = step })
		if e != nil {
			return e
		}
	}
	out, runErr := r.execute(ctx, j, exec, policy)
	if !out.Settled {
		return runErr
	} // retain the pending marker, including lost ACKs
	if len(out.Checkpoint) == 0 {
		out.Checkpoint = json.RawMessage(`{}`)
	}
	j, e = r.change(ctx, j.ID, f, "checkpoint", func(c *Change) { c.StepID = j.PendingStep; c.Checkpoint = out.Checkpoint; c.Progress = out.Progress })
	if e != nil {
		return e
	}
	if j.State.Terminal() {
		return nil
	}
	if exec.YieldAfterCheckpoint && !out.Done && runErr == nil {
		if e = r.Authority.Close(ctx, f); e != nil {
			return e
		}
		delete(r.leases, j.ID)
	}
	if out.Done || runErr != nil {
		code := out.ErrorCode
		if runErr != nil && code == "" {
			code = "step_failed"
		}
		_, e = r.change(ctx, j.ID, f, "finish", func(c *Change) { c.Result = out.Result; c.ErrorCode = code })
	}
	return e
}

func (r *Runner) execute(ctx context.Context, j Job, exec Executor, policy config.GlobalSnapshot) (Step, error) {
	// Renew soft expiry while a bounded step runs; the authority's hard deadline
	// still fences it. Heartbeats join before checkpoint/finish can publish.
	budget, cancel := context.WithTimeout(ctx, 8*time.Minute)
	defer cancel()
	call, stop := context.WithCancelCause(budget)
	done := make(chan error, 1)
	go func() {
		for {
			if e := r.Clock.Sleep(call, 15*time.Second); e != nil {
				done <- nil
				return
			}
			e := r.Authority.Renew(call, j.Worker)
			if e == nil {
				_, e = r.change(call, j.ID, j.Worker, "heartbeat", nil)
			}
			if e != nil {
				if call.Err() != nil && errors.Is(context.Cause(call), context.Canceled) {
					done <- nil
					return
				}
				stop(e)
				done <- e
				return
			}
		}
	}()
	out, e := exec.Step(call, j, policy)
	stop(nil)
	if renewErr := <-done; renewErr != nil {
		return Step{}, errors.Join(e, renewErr)
	}
	return out, e
}

// Close joins after the worker loop and reports every authority cleanup error.
func (r *Runner) Close(ctx context.Context) error {
	var out error
	for id, f := range r.leases {
		j, e := r.Service.Store.GetJob(ctx, id)
		if e == nil && j.Worker == f {
			e = r.Authority.Close(ctx, f)
		}
		out = errors.Join(out, e)
		if e == nil {
			delete(r.leases, id)
		}
	}
	return out
}
