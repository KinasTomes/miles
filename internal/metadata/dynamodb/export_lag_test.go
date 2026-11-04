package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type lagScan struct {
	API
	calls         int
	missing, loop bool
}

func (p *lagScan) Scan(_ context.Context, in *ddb.ScanInput, _ ...func(*ddb.Options)) (*ddb.ScanOutput, error) {
	p.calls++
	if aws.ToInt32(in.Limit) != 128 || !aws.ToBool(in.ConsistentRead) {
		return nil, s.ErrInvalid
	}
	item := Item{"history_id": text("history"), "event_observed_at": text("2026-11-04T00:00:00Z")}
	if p.missing {
		delete(item, "event_observed_at")
	}
	next := Item(nil)
	if p.loop {
		next = Item{"event_id": text("same")}
	}
	return &ddb.ScanOutput{Items: []Item{item}, LastEvaluatedKey: next}, nil
}
func TestExportLagRejectsLegacyUnknownAndCursorLoop(t *testing.T) {
	api := &lagScan{}
	m := &Store{api: api, tables: map[string]string{"Outbox": "test-Outbox"}}
	oldest, err := m.ObserveExportLag(t.Context(), "history")
	if err != nil || !oldest.Equal(time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatal(oldest, err)
	}
	api.missing = true
	if _, err = m.ObserveExportLag(t.Context(), "history"); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("missing timestamp became zero lag", err)
	}
	api.missing = false
	api.loop = true
	api.calls = 0
	if _, err = m.ObserveExportLag(t.Context(), "history"); !errors.Is(err, s.ErrUnavailable) || api.calls != 2 {
		t.Fatal("looping scan accepted", err, api.calls)
	}
}
