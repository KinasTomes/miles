package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type lifecycleGateAPI struct {
	API
	rows       map[string]Item
	scanned    []Item
	getTables  []string
	scanTables []string
	getErr     error
	missing    map[string]bool
}

func upgradeRow(t *testing.T, table string, key map[string]types.AttributeValue, value any) Item {
	t.Helper()
	item, err := nativeItem(value)
	if err != nil {
		t.Fatal(err)
	}
	for name, attr := range key {
		item[name] = attr
	}
	item["record_kind"] = text("r1_coordination_v1")
	return item
}

func gateStore(t *testing.T, state string, active, resolving uint64, actions bool) (*Store, *lifecycleGateAPI) {
	t.Helper()
	tables := map[string]string{}
	for name := range Tables() {
		tables[name] = "pre_" + name
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	fence := s.WriterFence{LeaseID: "job-1", ServiceIncarnation: "service-1", Epoch: 1}
	gate := s.R1Gate{Version: s.ModelVersion, Incarnation: "inc-1", HistoryID: strings.Repeat("a", 32), Epoch: 1, Revision: 2, Active: active, Resolving: resolving, State: state}
	rows := map[string]Item{}
	gk := stringKey("SystemConfig", "config_id", "coordination/r1/current")
	rows[physicalIdentity(tables["SystemConfig"], gk.Attributes)] = upgradeRow(t, "SystemConfig", gk.Attributes, gate)
	if actions {
		ref := s.PackRef{ID: strings.Repeat("b", 32), Generation: strings.Repeat("c", 32)}
		a := lifecycle.Action{Version: lifecycle.ActionVersionLegacy, ID: "job-1", JobID: "job-1", Kind: "tiering", State: "Done", Revision: 1, Fence: fence, Source: s.PackInfo{Ref: ref}}
		ak := lifecycleKey("job-1")
		rows[physicalIdentity(tables["SystemConfig"], ak.Attributes)] = upgradeRow(t, "SystemConfig", ak.Attributes, a)
		j := jobs.Job{ID: "job-1", Type: "tiering", State: jobs.Succeeded, PolicyRevision: 1, Revision: 3, Params: json.RawMessage(`{}`), CreatedAt: now.Add(-time.Hour), StartedAt: now.Add(-30 * time.Minute), FinishedAt: now, WorkerNodeID: "node-1", WorkerNodeIncarnation: "node-inc-1", Worker: fence}
		jk := stringKey("Jobs", "job_id", "job-1")
		rows[physicalIdentity(tables["Jobs"], jk.Attributes)] = upgradeRow(t, "Jobs", jk.Attributes, j)
		lease := s.R1Lease{Version: s.ModelVersion, Fence: fence, Owner: "owner", Kind: "control", State: "Released", Deadline: now.Add(time.Hour), ExpiresAt: now.Add(30 * time.Minute), Revision: 1}
		lk := r1LeaseKey("job-1")
		rows[physicalIdentity(tables["Leases"], lk.Attributes)] = upgradeRow(t, "Leases", lk.Attributes, lease)
	}
	api := &lifecycleGateAPI{rows: rows, missing: map[string]bool{}}
	store, err := New(Options{API: api, Tables: tables, Authority: denyGateWriters{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range rows {
		if id, ok := item["config_id"].(*types.AttributeValueMemberS); ok && strings.HasPrefix(id.Value, "lifecycle/action/") {
			api.scanned = append(api.scanned, item)
		}
	}
	return store, api
}

// The inspector's API dependency is read-only; Store.New only needs the seam.
type denyGateWriters struct{}

func (denyGateWriters) Check(context.Context, s.WriterFence, Observe) error { return s.ErrAccessDenied }

func (a *lifecycleGateAPI) Scan(_ context.Context, in *ddb.ScanInput, _ ...func(*ddb.Options)) (*ddb.ScanOutput, error) {
	a.scanTables = append(a.scanTables, *in.TableName)
	return &ddb.ScanOutput{Items: a.scanned}, nil
}
func (a *lifecycleGateAPI) GetItem(_ context.Context, in *ddb.GetItemInput, _ ...func(*ddb.Options)) (*ddb.GetItemOutput, error) {
	table := *in.TableName
	a.getTables = append(a.getTables, table)
	if a.getErr != nil {
		return nil, a.getErr
	}
	key := physicalIdentity(table, in.Key)
	if a.missing[key] {
		return &ddb.GetItemOutput{}, nil
	}
	if item := a.rows[key]; item != nil {
		return &ddb.GetItemOutput{Item: item}, nil
	}
	return &ddb.GetItemOutput{}, nil
}

func TestLifecycleUpgradeGateAllowsQuiescentClosingEmptyInventory(t *testing.T) {
	m, _ := gateStore(t, "Closing", 0, 0, false)
	findings, err := m.InspectLifecycleUpgrade(context.Background())
	if err != nil || len(findings) != 0 {
		t.Fatalf("expected empty quiescent V1 inventory to pass, findings=%#v err=%v", findings, err)
	}
}

func TestLifecycleUpgradeGateBlocksLegacyUnknownWithRetainedProtection(t *testing.T) {
	m, api := gateStore(t, "Closing", 0, 0, true)
	fence := s.WriterFence{LeaseID: "job-1", ServiceIncarnation: "service-1", Epoch: 1}
	ref := s.PackRef{ID: strings.Repeat("b", 32), Generation: strings.Repeat("c", 32)}
	a := lifecycle.Action{Version: lifecycle.ActionVersionLegacy, ID: "job-1", JobID: "job-1", Kind: "tiering", State: "Unknown", Revision: 1, Fence: fence, Source: s.PackInfo{Ref: ref}}
	ak := lifecycleKey("job-1")
	item := upgradeRow(t, "SystemConfig", ak.Attributes, a)
	api.rows[physicalIdentity("pre_SystemConfig", ak.Attributes)] = item
	api.scanned = []Item{item}
	jk := stringKey("Jobs", "job_id", "job-1")
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	job := jobs.Job{ID: "job-1", Type: "tiering", State: jobs.Running, PolicyRevision: 1, Revision: 2, Params: json.RawMessage(`{}`), CreatedAt: now.Add(-time.Hour), StartedAt: now.Add(-30 * time.Minute), WorkerNodeID: "node-1", WorkerNodeIncarnation: "node-inc-1", Worker: fence, PendingStep: strings.Repeat("1", 32)}
	api.rows[physicalIdentity("pre_Jobs", jk.Attributes)] = upgradeRow(t, "Jobs", jk.Attributes, job)
	leaseKey := r1LeaseKey("job-1")
	lease := s.R1Lease{Version: s.ModelVersion, Fence: fence, Owner: "owner", Kind: "control", State: "Closed", Deadline: now.Add(time.Hour), ExpiresAt: now.Add(30 * time.Minute), Revision: 2, Pending: []string{"lifecycle/job-1"}, PinCount: 1}
	api.rows[physicalIdentity("pre_Leases", leaseKey.Attributes)] = upgradeRow(t, "Leases", leaseKey.Attributes, lease)
	findings, err := m.InspectLifecycleUpgrade(context.Background())
	if err != nil || len(findings) != 1 || !findings[0].Blocked || !findings[0].Pending {
		t.Fatalf("expected retained unknown action to block, findings=%#v err=%v", findings, err)
	}
}

func TestLifecycleUpgradeGateBlocksActiveOrUnsafeGate(t *testing.T) {
	for _, tc := range []struct {
		name, state       string
		active, resolving uint64
	}{{"open", "Open", 0, 0}, {"active", "Closing", 1, 0}, {"resolving", "Closing", 0, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			m, api := gateStore(t, tc.state, tc.active, tc.resolving, false)
			if _, err := m.InspectLifecycleUpgrade(context.Background()); !errors.Is(err, s.ErrFenced) {
				t.Fatalf("expected unsafe gate to block, got %v", err)
			}
			if len(api.scanTables) != 0 {
				t.Fatal("inventory scan ran before quiescence was proven")
			}
		})
	}
}

func TestLifecycleUpgradeGateFailsClosedOnMissingOrCorruptGate(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		m, api := gateStore(t, "Closing", 0, 0, false)
		api.missing[physicalIdentity("pre_SystemConfig", r1GateKey().Attributes)] = true
		if _, err := m.InspectLifecycleUpgrade(context.Background()); err == nil {
			t.Fatal("missing R1 gate must fail closed")
		}
	})
	t.Run("read error", func(t *testing.T) {
		m, api := gateStore(t, "Closing", 0, 0, false)
		api.getErr = errors.New("injected read failure")
		if _, err := m.InspectLifecycleUpgrade(context.Background()); err == nil {
			t.Fatal("R1 gate read error must fail closed")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		m, api := gateStore(t, "Closing", 0, 0, false)
		key := physicalIdentity("pre_SystemConfig", r1GateKey().Attributes)
		api.rows[key] = Item{"config_id": text("coordination/r1/current"), "v": number(99)}
		if _, err := m.InspectLifecycleUpgrade(context.Background()); !errors.Is(err, s.ErrFenced) {
			t.Fatalf("corrupt gate must fail closed, got %v", err)
		}
	})
}

func TestLifecycleUpgradeGateUsesOnlyRequiredReadAPIsAndTables(t *testing.T) {
	m, api := gateStore(t, "Closing", 0, 0, true)
	findings, err := m.InspectLifecycleUpgrade(context.Background())
	if err != nil || len(findings) != 1 || findings[0].Blocked {
		t.Fatalf("expected terminal legacy action to pass: %#v %v", findings, err)
	}
	if len(api.scanTables) != 1 || api.scanTables[0] != "pre_SystemConfig" {
		t.Fatalf("unexpected Scan calls: %#v", api.scanTables)
	}
	seen := map[string]bool{}
	for _, table := range api.getTables {
		seen[table] = true
	}
	for _, table := range []string{"pre_SystemConfig", "pre_Jobs", "pre_Leases"} {
		if !seen[table] {
			t.Errorf("missing required GetItem table %s; got %#v", table, api.getTables)
		}
	}
	if len(api.getTables) != 3 {
		t.Fatalf("expected gate/job/lease GetItem reads only, got %#v", api.getTables)
	}
}
