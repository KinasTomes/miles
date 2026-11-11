package dynamodb

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
)

func TestLocalLifecycleTakeoverRequiresExactOriginalStep(t *testing.T) {
	for _, kind := range []jobs.Type{"gc", "compaction", "tiering", "pack_restore", "pack_promote"} {
		t.Run(string(kind), func(t *testing.T) {
			m, _, original, _, utc, _, chunk := lifeFixture(t, kind, s.GlacierIR)
			ctx := t.Context()
			lease, err := m.GetR1Lease(ctx, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.CloseR1(ctx, original.Worker); err != nil {
				t.Fatal(err)
			}
			if err = m.ReleaseR1(ctx, original.Worker); err != nil {
				t.Fatal(err)
			}
			next := original.Worker
			next.Epoch++
			if _, err = m.AcquireR1(ctx, s.R1Acquire{Fence: next, Owner: "takeover", Kind: "control", OpID: lease.OpID, Deadline: utc.Now().Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			packs, err := m.GetPacks(ctx, []s.PackRef{chunk.Location.Pack})
			if err != nil {
				t.Fatal(err)
			}
			claim := jobs.Change{JobID: original.ID, ExpectedRevision: original.Revision, Kind: "claim", Fence: next, NodeID: original.WorkerNodeID, NodeIncarnation: original.WorkerNodeIncarnation}
			if _, err := m.ChangeJob(ctx, claim); !errors.Is(err, jobs.ErrReconciliation) {
				t.Fatal("missing action allowed takeover", err)
			}
			for _, outcome := range []string{"Done", "Skipped", "NotApplied", "Waiting", "Unknown", "Dispatched"} {
				for _, binding := range []string{"exact", "different", "legacy-empty", "v2-empty", "wrong-id", "wrong-job", "wrong-kind", "wrong-fence"} {
					t.Run(outcome+"/"+binding, func(t *testing.T) {
						a := lifecycle.Action{Version: lifecycle.ActionVersionCurrent, ID: original.ID, JobID: original.ID, StepID: original.PendingStep, Kind: kind, State: outcome, Revision: 1, Fence: original.Worker, Source: packs[chunk.Location.Pack]}
						switch binding {
						case "different":
							a.StepID = strings.Repeat("9", 32)
						case "legacy-empty":
							a.Version, a.StepID = lifecycle.ActionVersionLegacy, ""
						case "v2-empty":
							a.StepID = ""
						case "wrong-id":
							a.ID = "job_" + strings.Repeat("9", 32)
						case "wrong-job":
							a.JobID = "job_" + strings.Repeat("9", 32)
						case "wrong-kind":
							a.Kind = "scrub"
						case "wrong-fence":
							a.Fence.Epoch++
						}
						// Inject durable rows to test claim preconditions independently
						// of the physical operation; no fake proof is used for runtime acceptance.
						if err := m.update(ctx, func(tx *transaction) error {
							if err := put(tx, key("jobs", original.ID), original); err != nil {
								return err
							}
							// Alias permits malformed identity fixtures without changing
							// production validators; native reads must reject them.
							type rawAction lifecycle.Action
							return r1Put(tx, lifecycleKey(original.ID), rawAction(a))
						}); err != nil {
							t.Fatal(err)
						}
						got, err := m.ChangeJob(ctx, jobs.Change{JobID: original.ID, ExpectedRevision: original.Revision, Kind: "claim", Fence: next, NodeID: original.WorkerNodeID, NodeIncarnation: original.WorkerNodeIncarnation})
						allowed := binding == "exact" && (outcome == "Done" || outcome == "Skipped" || outcome == "NotApplied" || kind == "pack_restore" && outcome == "Waiting")
						if allowed {
							if err != nil || got.PendingStep != "" || got.Worker != next || string(got.Checkpoint) != `{"lifecycle_reconciled":true}` {
								t.Fatal("exact terminal did not permit takeover", got, err)
							}
						} else {
							if !errors.Is(err, jobs.ErrReconciliation) && !errors.Is(err, s.ErrUnavailable) {
								t.Fatal("unbound outcome permitted takeover", got, err)
							}
							stored, readErr := m.GetJob(ctx, original.ID)
							if readErr != nil || !reflect.DeepEqual(stored, original) {
								t.Fatal("rejected claim mutated pending job", stored, readErr)
							}
						}
					})
				}
			}
		})
	}
}

func TestLocalLifecycleSettledOutcomesResumeWithoutDispatch(t *testing.T) {
	for _, outcome := range []string{"Skipped", "NotApplied", "Waiting"} {
		t.Run(outcome, func(t *testing.T) {
			kind, class := jobs.Type("tiering"), s.GlacierIR
			if outcome == "Waiting" {
				kind, class = "pack_restore", s.Glacier
			}
			m, w, j, policy, utc, physical, _ := lifeFixture(t, kind, class)
			ctx := t.Context()
			if outcome != "Waiting" {
				params, err := lifecycle.Decode(j.Params, j.Type)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = m.PrepareLifecycle(ctx, j, params, nil, ""); err != nil {
					t.Fatal(err)
				}
				if outcome == "Skipped" {
					if _, err = m.SkipLifecycle(ctx, j); err != nil {
						t.Fatal(err)
					}
				} else {
					// Model a request proven terminal with no effect. No physical
					// call is sent; this remains a provider-contract fixture.
					if _, err = m.DispatchLifecycle(ctx, j, s.GlacierIR, 4); err != nil {
						t.Fatal(err)
					}
					if _, fresh, err := m.ClaimLifecycleSend(ctx, j); err != nil || !fresh {
						t.Fatal(fresh, err)
					}
					if err = m.UnknownLifecycle(ctx, j); err != nil {
						t.Fatal(err)
					}
					m.authority.(*SharedAuthority).LifecycleTerminal = lifeTerminalGuard{}
					proof := lifeTerminalProof{ID: j.ID, Fence: j.Worker, Target: s.GlacierIR, Terminal: true, Isolated: true}
					if err = m.update(ctx, func(tx *transaction) error {
						return r1Put(tx, stringKey("SystemConfig", "config_id", "fixture/lifecycle/terminal"), proof)
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			out, err := w.Step(ctx, j, policy)
			if err != nil || !out.Settled || out.Done != (outcome != "Waiting") {
				t.Fatal(out, err)
			}
			a, err := m.GetLifecycle(ctx, j.ID)
			if err != nil || a.State != outcome || a.StepID != j.PendingStep {
				t.Fatal(a, err)
			}
			if err = m.LifecycleQuiescent(ctx, j.ID, j.Worker); err != nil {
				t.Fatal(err)
			}
			calls := physical.calls
			lease, err := m.GetR1Lease(ctx, j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.CloseR1(ctx, j.Worker); err != nil {
				t.Fatal(err)
			}
			if err = m.ReleaseR1(ctx, j.Worker); err != nil {
				t.Fatal(err)
			}
			old := j
			next := j.Worker
			next.Epoch++
			if _, err = m.AcquireR1(ctx, s.R1Acquire{Fence: next, Owner: "takeover", Kind: "control", OpID: lease.OpID, Deadline: utc.Now().Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			step := strings.Repeat("3", 32)
			if outcome == "Waiting" {
				step = a.StepID
			} // the asynchronous action is still the original step
			for _, change := range []string{"claim", "begin_step"} {
				j, err = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: change, Fence: next, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation, StepID: step})
				if err != nil {
					t.Fatal(err)
				}
			}
			out, err = w.Step(ctx, j, policy)
			if err != nil || !out.Settled || out.Done != (outcome != "Waiting") || physical.calls != calls {
				t.Fatal("settled outcome redispatched", out, err, physical.calls, calls)
			}
			if _, err := w.Step(ctx, old, policy); !errors.Is(err, s.ErrFenced) {
				t.Fatal("stale worker was not fenced", err)
			}
		})
	}
}

func TestLocalLifecycleCleanupKeepsMismatchedStepAuthority(t *testing.T) {
	for _, kind := range []jobs.Type{"gc", "compaction", "tiering", "pack_restore"} {
		t.Run(string(kind), func(t *testing.T) {
			m, _, j, _, _, _, chunk := lifeFixture(t, kind, s.GlacierIR)
			packs, err := m.GetPacks(t.Context(), []s.PackRef{chunk.Location.Pack})
			if err != nil {
				t.Fatal(err)
			}
			for _, outcome := range []string{"Done", "Skipped", "NotApplied", "Waiting", "Unknown", "Dispatched"} {
				for _, step := range []string{j.PendingStep, strings.Repeat("9", 32), ""} {
					a := lifecycle.Action{Version: lifecycle.ActionVersionCurrent, ID: j.ID, JobID: j.ID, StepID: step, Kind: kind, State: outcome, Revision: 1, Fence: j.Worker, Source: packs[chunk.Location.Pack]}
					if step == "" {
						a.Version = lifecycle.ActionVersionLegacy
					}
					if err := m.update(t.Context(), func(tx *transaction) error { return r1Put(tx, lifecycleKey(j.ID), a) }); err != nil {
						t.Fatal(err)
					}
					err := m.LifecycleQuiescent(t.Context(), j.ID, j.Worker)
					allowed := step == j.PendingStep && (outcome == "Done" || outcome == "Skipped" || outcome == "NotApplied" || kind == "pack_restore" && outcome == "Waiting")
					if allowed && err != nil || !allowed && !errors.Is(err, jobs.ErrReconciliation) {
						t.Fatalf("kind=%s outcome=%s step=%q allowed=%t err=%v", kind, outcome, step, allowed, err)
					}
					lease, err := m.GetR1Lease(t.Context(), j.ID)
					if err != nil || lease.State != "Active" || lease.Fence != j.Worker {
						t.Fatal("quiescence check changed authority", lease, err)
					}
				}
			}
		})
	}
}

func TestLocalLifecycleTakeoverBindsActionObservationToClaimCAS(t *testing.T) {
	m, w, j, policy, utc, _, _ := lifeFixture(t, "tiering", s.GlacierIR)
	ctx := t.Context()
	if out, err := w.Step(ctx, j, policy); err != nil || !out.Done {
		t.Fatal(out, err)
	}
	lease, err := m.GetR1Lease(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.CloseR1(ctx, j.Worker); err != nil {
		t.Fatal(err)
	}
	if err = m.ReleaseR1(ctx, j.Worker); err != nil {
		t.Fatal(err)
	}
	next := j.Worker
	next.Epoch++
	if _, err = m.AcquireR1(ctx, s.R1Acquire{Fence: next, Owner: "takeover", Kind: "control", OpID: lease.OpID, Deadline: utc.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	peer, err := New(Options{API: m.api, Tables: m.tables, Authority: m.authority, Now: utc.Now})
	if err != nil {
		t.Fatal(err)
	}
	seam := &lifeLateCASAPI{API: m.api, table: m.tables["Jobs"], before: func() {
		// Adversarially change an otherwise valid original-step observation
		// immediately before the claim transaction reaches native DynamoDB.
		if err := peer.update(ctx, func(tx *transaction) error {
			a, err := r1Read[lifecycle.Action](tx, lifecycleKey(j.ID))
			if err != nil {
				return err
			}
			a.StepID = strings.Repeat("9", 32)
			return r1Put(tx, lifecycleKey(j.ID), a)
		}); err != nil {
			t.Fatal(err)
		}
	}}
	m.api = seam
	if _, err = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "claim", Fence: next, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation}); !errors.Is(err, jobs.ErrReconciliation) || !seam.fired {
		t.Fatal("stale action observation cleared pending step", err, seam.fired)
	}
	stored, err := m.GetJob(ctx, j.ID)
	if err != nil || !reflect.DeepEqual(stored, j) {
		t.Fatal("rejected proof race changed job", stored, err)
	}
}
