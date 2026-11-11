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
				for _, binding := range []string{"exact", "different", "legacy-empty", "wrong-id", "wrong-job", "wrong-kind", "wrong-fence"} {
					t.Run(outcome+"/"+binding, func(t *testing.T) {
						a := lifecycle.Action{Version: lifecycle.ActionVersionCurrent, ID: original.ID, JobID: original.ID, StepID: original.PendingStep, Kind: kind, State: outcome, Revision: 1, Fence: original.Worker, Source: packs[chunk.Location.Pack]}
						switch binding {
						case "different":
							a.StepID = strings.Repeat("9", 32)
						case "legacy-empty":
							a.Version, a.StepID = lifecycle.ActionVersionLegacy, ""
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
