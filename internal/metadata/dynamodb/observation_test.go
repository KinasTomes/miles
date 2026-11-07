package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	suite "github.com/KinasTomes/miles/internal/metadata/contracttest"
	"github.com/KinasTomes/miles/internal/storage"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestLocalObservationContract(t *testing.T) {
	suite.RunObservation(t, func(t *testing.T, f shared.Fixture) suite.ObservingStore { m, _ := fixtureStore(t, f); return m })
}

func TestLocalObservationCAS(t *testing.T) {
	suite.RunObservationCAS(t, func(t *testing.T, f shared.Fixture) suite.ObservingStore { m, _ := fixtureStore(t, f); return m })
}

func TestLocalExportLagUsesAtomicEventObservationAndExportProgress(t *testing.T) {
	f := newRecoveryFixture(t)
	request, writer := f.mutation(t, "pilot-lag")
	before := f.utc.Now()
	if _, err := f.m.MutateControl(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	oldest, err := f.m.ObserveExportLag(t.Context(), f.catalog.History)
	if err != nil || !oldest.Equal(before) {
		t.Fatal("missing publication timestamp", oldest, err)
	}
	f.utc.Advance(10 * time.Second)
	if _, err = f.peer.MutateControl(t.Context(), request); err != nil {
		t.Fatal("idempotent mutation", err)
	}
	again, err := f.m.ObserveExportLag(t.Context(), f.catalog.History)
	if err != nil || !again.Equal(oldest) {
		t.Fatal("retry reset event lag", again, err)
	}
	f.release(t, writer)
	if err = f.service(f.m, f.worker).Flush(t.Context()); err != nil {
		t.Fatal(err)
	}
	if oldest, err = f.m.ObserveExportLag(t.Context(), f.catalog.History); err != nil || !oldest.IsZero() {
		t.Fatal("exported event still pending", oldest, err)
	}
}

func TestLocalRuntimeObservationUsesSealedCut(t *testing.T) {
	f := newRecoveryFixture(t)
	initial, err := f.m.ObserveRuntime(t.Context(), f.catalog.History, f.utc.Now())
	if err != nil || initial.GateClosed || !initial.CutAt.Equal(f.utc.Now()) {
		t.Fatal("initial cut schedule", initial, err)
	}
	if _, err = f.m.ObserveRuntime(t.Context(), "foreign-history", f.utc.Now()); err != storage.ErrFenced {
		t.Fatal("mixed history observation", err)
	}
	f.utc.Advance(10 * time.Second)
	if _, err = f.service(f.m, f.worker).CloseAndSeal(t.Context(), f.request(0)); err != nil {
		t.Fatal(err)
	}
	epoch, err := f.m.GetRecoveryEpoch(t.Context(), f.catalog.History, 0)
	if err != nil {
		t.Fatal(err)
	}
	sample, err := f.m.ObserveRuntime(t.Context(), f.catalog.History, f.utc.Now())
	if err != nil || epoch.State != "Sealed" || !sample.CutAt.Equal(epoch.CutAt) || sample.CutAt.Equal(initial.CutAt) {
		t.Fatal("cut observation does not follow sealed publication", sample, epoch, err)
	}
}

func TestLocalRuntimeObservationDoesNotResetAgeAtUnsealedClose(t *testing.T) {
	f := newRecoveryFixture(t)
	f.utc.Advance(10 * time.Second)
	epoch := closeRecovery(t, f.m, f.request(0))
	if epoch.State != "Closed" {
		t.Fatal(epoch.State)
	}
	if _, err := f.m.ObserveRuntime(t.Context(), f.catalog.History, f.utc.Now()); err != storage.ErrUnavailable {
		t.Fatal("advanced cadence was treated as a sealed cut", err)
	}
}

func TestLocalRuntimeObservationDeepScanOptIn(t *testing.T) {
	f := newRecoveryFixture(t)
	// Default (deep=false): fast O(1) point lookups, no table scans
	shallow, err := f.m.ObserveRuntime(t.Context(), f.catalog.History, f.utc.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if shallow.DeepScanned || shallow.StuckCompleting != 0 || !shallow.OldestPendingExport.IsZero() {
		t.Fatalf("shallow scan should not scan tables, got deep=%v stuck=%d oldest=%v", shallow.DeepScanned, shallow.StuckCompleting, shallow.OldestPendingExport)
	}

	// Opt-in (deep=true): scans uploads and outbox
	deep, err := f.m.ObserveRuntime(t.Context(), f.catalog.History, f.utc.Now(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !deep.DeepScanned || deep.CutAt != shallow.CutAt {
		t.Fatalf("CutAt mismatch or DeepScanned false: deep=%+v shallow=%+v", deep, shallow)
	}
}

type uploadPaginationMock struct {
	API
	calls     int
	morePages bool
}

func (p *uploadPaginationMock) GetItem(ctx context.Context, in *ddb.GetItemInput, opt ...func(*ddb.Options)) (*ddb.GetItemOutput, error) {
	table := aws.ToString(in.TableName)
	now := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
	history := strings.Repeat("a", 32)
	if strings.HasSuffix(table, "SystemConfig") {
		cfgID := in.Key["config_id"].(*types.AttributeValueMemberS).Value
		if cfgID == "coordination/r1/current" {
			item, _ := nativeItem(storage.R1Gate{
				Version:     storage.ModelVersion,
				HistoryID:   history,
				Incarnation: "inc1",
				State:       "Open",
				Revision:    1,
			})
			for k, v := range in.Key {
				item[k] = v
			}
			return &ddb.GetItemOutput{Item: item}, nil
		}
		if cfgID == "recovery/state/"+history {
			item, _ := nativeItem(storage.RecoveryState{
				Version:       storage.ModelVersion,
				HistoryID:     history,
				Incarnation:   "inc1",
				GenesisSHA256: strings.Repeat("0", 64),
				GenesisLength: 1,
				Revision:      1,
				NextToSeal:    0,
				NextCutEpoch:  0,
				NextCutAt:     now.Add(time.Hour),
			})
			for k, v := range in.Key {
				item[k] = v
			}
			return &ddb.GetItemOutput{Item: item}, nil
		}
	}
	return &ddb.GetItemOutput{}, nil
}

func (p *uploadPaginationMock) Scan(ctx context.Context, in *ddb.ScanInput, opt ...func(*ddb.Options)) (*ddb.ScanOutput, error) {
	table := aws.ToString(in.TableName)
	if strings.HasSuffix(table, "Uploads") {
		p.calls++
		upRecord, _ := nativeItem(storage.MultipartUpload{
			Version:    storage.ModelVersion,
			ID:         fmt.Sprintf("up-%d", p.calls),
			State:      "Completing",
			LeaseUntil: time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC),
		})
		item := Item{
			"upload_id":     text(fmt.Sprintf("up-%d", p.calls)),
			"upload-record": &types.AttributeValueMemberM{Value: upRecord},
		}
		var next Item
		if p.morePages || p.calls < 3 {
			next = Item{"upload_id": text(fmt.Sprintf("up-%d", p.calls))}
		}
		return &ddb.ScanOutput{Items: []Item{item}, LastEvaluatedKey: next}, nil
	}
	if strings.HasSuffix(table, "Outbox") {
		return &ddb.ScanOutput{}, nil
	}
	if p.API != nil {
		return p.API.Scan(ctx, in, opt...)
	}
	return &ddb.ScanOutput{}, nil
}

func TestLocalRuntimeObservationPagination(t *testing.T) {
	tables := map[string]string{
		"R1Gates":        "test-R1Gates",
		"SystemConfig":   "test-SystemConfig",
		"RecoveryEpochs": "test-RecoveryEpochs",
		"Uploads":        "test-Uploads",
		"Outbox":         "test-Outbox",
	}
	now := time.Date(2026, 11, 4, 1, 0, 0, 0, time.UTC)
	history := strings.Repeat("a", 32)

	// Case 1: Uploads pagination completes within 4 pages (3 pages total)
	mockUnderLimit := &uploadPaginationMock{morePages: false}
	mUnder := &Store{api: mockUnderLimit, tables: tables, maxAttempts: 1}
	res, err := mUnder.ObserveRuntime(t.Context(), history, now, true)
	if err != nil {
		t.Fatalf("expected pagination under limit to succeed, got error: %v", err)
	}
	if !res.DeepScanned || res.StuckCompleting != 3 {
		t.Fatalf("expected DeepScanned=true and StuckCompleting=3, got deep=%v stuck=%d", res.DeepScanned, res.StuckCompleting)
	}
	if mockUnderLimit.calls != 3 {
		t.Fatalf("expected 3 scan calls, got %d", mockUnderLimit.calls)
	}

	// Case 2: Uploads pagination exceeds 4 pages (morePages=true) -> MUST fail with storage.ErrUnavailable
	mockExceeded := &uploadPaginationMock{morePages: true}
	mExceeded := &Store{api: mockExceeded, tables: tables, maxAttempts: 1}
	_, err = mExceeded.ObserveRuntime(t.Context(), history, now, true)
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("expected pagination exceeding 4 pages to return storage.ErrUnavailable, got %v", err)
	}
	if mockExceeded.calls != 4 {
		t.Fatalf("expected exactly 4 bounded scan calls before aborting, got %d", mockExceeded.calls)
	}
}
