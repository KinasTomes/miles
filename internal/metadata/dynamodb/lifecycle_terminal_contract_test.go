package dynamodb

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// This is an adversarial fixture for the provider contract, not a production
// terminal oracle. Runtime acceptance still requires the trusted provider.
type terminalContractProof struct {
	Action                      lifecycle.Action
	Terminal, Isolated, Applied bool
}

type terminalContractGuard struct{}

func terminalContractKey() Key {
	return stringKey("SystemConfig", "config_id", "fixture/lifecycle/exact-terminal")
}

func (terminalContractGuard) Terminal(_ context.Context, a lifecycle.Action, observe Observe) (bool, error) {
	proof, err := r1Observed[terminalContractProof](observe, terminalContractKey())
	if err != nil {
		return false, err
	}
	if !reflect.DeepEqual(proof.Action, a) || !proof.Terminal || !proof.Isolated {
		return false, s.ErrR1Busy
	}
	return proof.Applied, nil
}

func unknownLifecycleFixture(t *testing.T, kind jobs.Type) (*Store, *lifecycle.Worker, jobs.Job, config.GlobalSnapshot, *r1Clock, func() int) {
	t.Helper()
	m, w, j, policy, utc, physical, chunk := lifeFixture(t, kind, s.GlacierIR)
	count := func() int { return physical.calls }
	if kind == "tiering" {
		physical.lose = true
	} else {
		rows, err := m.GetPacks(t.Context(), []s.PackRef{chunk.Location.Pack})
		if err != nil {
			t.Fatal(err)
		}
		dead := rows[chunk.Location.Pack]
		dead.Ref = s.PackRef{ID: "terminal-gc", Generation: "retiring"}
		dead.State, dead.Class, dead.MinRetentionUntil = "Retiring", s.Standard, time.Time{}
		when := utc.Now().Add(-26 * time.Hour)
		if err := m.update(t.Context(), func(tx *transaction) error {
			if err := put(tx, packKey(dead.Ref), dead); err != nil {
				return err
			}
			if err := r1Put(tx, sweepRetirementKey(dead.Ref), sweepRetirement{Version: 1, Ref: dead.Ref, At: when}); err != nil {
				return err
			}
			j.Params, _ = json.Marshal(lifecycle.Params{PackID: dead.Ref.ID, Generation: dead.Ref.Generation})
			return put(tx, key("jobs", j.ID), j)
		}); err != nil {
			t.Fatal(err)
		}
		w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000,
			RecoveryRoots: func(context.Context) ([]s.PackRef, error) { return nil, nil }}
		w.VerifyCutAfter = func(context.Context, time.Time) error { return nil }
		sweep := &sweepPhysicalFixture{unknown: true}
		w.Sweep = sweep
		count = func() int { return len(sweep.calls) }
	}
	if out, err := w.Step(t.Context(), j, policy); err == nil || out.Settled {
		t.Fatal("lost ACK did not preserve uncertainty", out, err)
	}
	a, err := m.GetLifecycle(t.Context(), j.ID)
	if err != nil || a.State != "Unknown" {
		t.Fatal(a, err)
	}
	return m, w, j, policy, utc, count
}

func assertUnknownProtected(t *testing.T, m *Store, j jobs.Job) {
	t.Helper()
	a, err := m.GetLifecycle(t.Context(), j.ID)
	gate, gateErr := m.GetR1Gate(t.Context())
	if err != nil || gateErr != nil || a.State != "Unknown" || gate.State != "Maintenance" {
		t.Fatal("unknown released protection", a.State, gate, err, gateErr)
	}
	if err := m.LifecycleQuiescent(t.Context(), j.ID, j.Worker); !errors.Is(err, jobs.ErrReconciliation) {
		t.Fatal("unknown allowed authority cleanup", err)
	}
}

func TestLocalUnknownLifecycleCannotExpireIntoDispatchPermission(t *testing.T) {
	for _, kind := range []jobs.Type{"tiering", "gc"} {
		t.Run(string(kind), func(t *testing.T) {
			m, w, j, policy, utc, calls := unknownLifecycleFixture(t, kind)
			before := calls()
			for attempt := 0; attempt < 3; attempt++ {
				if out, err := w.Step(t.Context(), j, policy); err == nil || out.Settled {
					t.Fatal("unknown became settled without proof", attempt, out, err)
				}
				assertUnknownProtected(t, m, j)
			}
			concurrent := s.WriterFence{LeaseID: "concurrent-gc", Epoch: 1, ServiceIncarnation: j.Worker.ServiceIncarnation}
			if _, err := m.AcquireR1(t.Context(), s.R1Acquire{Fence: concurrent, Owner: "concurrent-gc", Kind: "control", Deadline: utc.Now().Add(time.Minute)}); err == nil {
				t.Fatal("concurrent GC entered unresolved maintenance")
			}
			utc.Advance(time.Minute)
			if out, err := w.Step(t.Context(), j, policy); err == nil || out.Settled {
				t.Fatal("expiry settled unknown", out, err)
			}
			assertUnknownProtected(t, m, j)
			next := j.Worker
			next.Epoch++
			if _, err := m.AcquireR1(t.Context(), s.R1Acquire{Fence: next, Owner: "takeover", Kind: "control", Deadline: utc.Now().Add(time.Minute)}); err == nil {
				t.Fatal("takeover reused an unresolved physical slot")
			}
			if calls() != before {
				t.Fatal("unknown redispatched physical operation", before, calls())
			}
		})
	}
}

type terminalProofRaceAPI struct {
	API
	table, id string
	before    func()
	fired     bool
}

func (a *terminalProofRaceAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	for _, item := range in.TransactItems {
		if item.Put != nil && item.Put.TableName != nil && *item.Put.TableName == a.table {
			id, _ := item.Put.Item["config_id"].(*types.AttributeValueMemberS)
			if !a.fired && id != nil && id.Value == a.id {
				a.fired = true
				a.before()
			}
		}
	}
	return a.API.TransactWriteItems(ctx, in, opts...)
}

func TestLocalTerminalEvidenceIsBoundToResolutionCAS(t *testing.T) {
	for _, kind := range []jobs.Type{"tiering", "gc"} {
		t.Run(string(kind), func(t *testing.T) {
			m, w, j, policy, utc, calls := unknownLifecycleFixture(t, kind)
			before := calls()
			a, err := m.GetLifecycle(t.Context(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			m.authority.(*SharedAuthority).LifecycleTerminal = terminalContractGuard{}
			proof := terminalContractProof{Action: a, Terminal: true, Isolated: true, Applied: true}
			putProof := func(store *Store, proof terminalContractProof) {
				t.Helper()
				if err := store.update(t.Context(), func(tx *transaction) error { return r1Put(tx, terminalContractKey(), proof) }); err != nil {
					t.Fatal(err)
				}
			}
			for _, field := range []string{"action", "generation", "fence", "target", "versions", "terminal", "isolation"} {
				bad := proof
				switch field {
				case "action":
					bad.Action.ID = "different-action"
				case "generation":
					bad.Action.Source.Ref.Generation = "different-generation"
				case "fence":
					bad.Action.Fence.Epoch++
				case "target":
					bad.Action.Target = s.StandardIA
				case "versions":
					bad.Action.SweepVersions = []s.SweepVersion{{Category: "pack", VersionID: "different-version"}}
				case "terminal":
					bad.Terminal = false
				case "isolation":
					bad.Isolated = false
				}
				putProof(m, bad)
				if _, err := m.ResolveLifecycle(t.Context(), j); err == nil {
					t.Fatal("mismatched proof accepted", field)
				}
				assertUnknownProtected(t, m, j)
			}
			putProof(m, proof)
			peer, err := New(Options{API: m.api, Tables: m.tables, Authority: m.authority, Now: utc.Now})
			if err != nil {
				t.Fatal(err)
			}
			seam := &terminalProofRaceAPI{API: m.api, table: m.tables["SystemConfig"], id: "lifecycle/action/" + j.ID, before: func() {
				invalid := proof
				invalid.Isolated = false
				putProof(peer, invalid)
			}}
			m.api = seam
			if _, err := m.ResolveLifecycle(t.Context(), j); err == nil || !seam.fired {
				t.Fatal("proof changed after observation but resolution committed", err, seam.fired)
			}
			assertUnknownProtected(t, m, j)
			putProof(m, proof)
			out, err := w.Step(t.Context(), j, policy)
			if err != nil || !out.Done || !out.Settled || calls() != before {
				t.Fatal("valid proof failed to resume without redispatch", out, err, calls())
			}
			if err := m.LifecycleQuiescent(t.Context(), j.ID, j.Worker); err != nil {
				t.Fatal("terminal proof did not release pending barrier", err)
			}
			gate, err := m.GetR1Gate(t.Context())
			if err != nil || gate.State != "Open" {
				t.Fatal("terminal outcome left gate blocked", gate, err)
			}
			// Crash after durable completion, before the Jobs checkpoint ACK.
			lease, err := m.GetR1Lease(t.Context(), j.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err = m.CloseR1(t.Context(), j.Worker); err != nil {
				t.Fatal(err)
			}
			if err = m.ReleaseR1(t.Context(), j.Worker); err != nil {
				t.Fatal(err)
			}
			old := j
			next := j.Worker
			next.Epoch++
			if _, err = m.AcquireR1(t.Context(), s.R1Acquire{Fence: next, Owner: "takeover", Kind: "control", OpID: lease.OpID, Deadline: utc.Now().Add(5 * time.Minute)}); err != nil {
				t.Fatal(err)
			}
			for _, change := range []string{"claim", "begin_step"} {
				j, err = m.ChangeJob(t.Context(), jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: change, Fence: next, NodeID: j.WorkerNodeID, NodeIncarnation: j.WorkerNodeIncarnation, StepID: strings.Repeat("3", 32)})
				if err != nil {
					t.Fatal(err)
				}
			}
			out, err = w.Step(t.Context(), j, policy)
			if err != nil || !out.Done || calls() != before {
				t.Fatal("takeover redispatched settled operation", out, err, calls())
			}
			if _, err = w.Step(t.Context(), old, policy); !errors.Is(err, s.ErrFenced) {
				t.Fatal("old worker published after takeover", err)
			}
			// Keep a compact fingerprint available in verbose evidence logs.
			body, _ := json.Marshal(a)
			t.Logf("action=%s generation=%s proof-action-sha256=%x dispatches=%d", a.ID, a.Source.Ref.Generation, sha256.Sum256(body), calls())
		})
	}
}
