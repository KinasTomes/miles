package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type idleOutboxAPI struct {
	API
	t              *testing.T
	gate           s.R1Gate
	epoch          *s.RecoveryEpoch
	epochError     error
	event          s.OutboxEvent
	registered     bool
	queryError     error
	queries, scans int
}

func (a *idleOutboxAPI) GetItem(_ context.Context, in *ddb.GetItemInput, _ ...func(*ddb.Options)) (*ddb.GetItemOutput, error) {
	if !aws.ToBool(in.ConsistentRead) {
		a.t.Fatal("discovery needs consistent authority reads")
	}
	var value any = a.gate
	if aws.ToString(in.TableName) == "fixture-RecoveryEpochs" {
		if a.epochError != nil {
			return nil, a.epochError
		}
		if a.epoch == nil {
			return &ddb.GetItemOutput{}, nil
		}
		value = *a.epoch
	}
	row, err := nativeItem(value)
	return &ddb.GetItemOutput{Item: row}, err
}
func (a *idleOutboxAPI) Query(_ context.Context, in *ddb.QueryInput, _ ...func(*ddb.Options)) (*ddb.QueryOutput, error) {
	a.queries++
	partition := fmt.Sprintf("recovery/mutations/%s/%d", a.gate.HistoryID, a.gate.Epoch)
	if !aws.ToBool(in.ConsistentRead) || aws.ToInt32(in.Limit) != 1 || in.FilterExpression != nil || in.ExpressionAttributeValues[":partition"] == nil {
		a.t.Fatal("empty-epoch discovery needs a consistent, unfiltered base membership query", in)
	}
	if got := in.ExpressionAttributeValues[":partition"]; !reflect.DeepEqual(got, text(partition)) {
		a.t.Fatal("queried a different epoch", got)
	}
	if a.queryError != nil {
		return nil, a.queryError
	}
	if a.registered {
		return &ddb.QueryOutput{Items: []Item{{"lease_id": text(a.event.ID)}}}, nil
	}
	return &ddb.QueryOutput{}, nil
}
func (a *idleOutboxAPI) Scan(context.Context, *ddb.ScanInput, ...func(*ddb.Options)) (*ddb.ScanOutput, error) {
	a.scans++
	row, err := nativeItem(a.event)
	row["event_id"] = text(a.event.ID)
	return &ddb.ScanOutput{Items: []Item{row}}, err
}

func TestEmptyOpenEpochDoesNotScanClosedHistory(t *testing.T) {
	for _, mode := range []string{"empty open", "registered", "closed", "cut in progress", "legacy", "non-shared authority", "query failure", "existing epoch", "epoch read failure"} {
		t.Run(mode, func(t *testing.T) {
			e := contracttest.NewFixture().Request("old-epoch", 0).Event
			if err := e.Validate(); err != nil {
				t.Fatal(err)
			}
			api := &idleOutboxAPI{t: t, event: e, gate: s.R1Gate{Version: 1, Incarnation: "runtime", HistoryID: e.HistoryID, Epoch: e.Epoch + 1, Revision: 1, State: "Open", RecoveryVersion: 1, RecoveryEnabled: true}}
			m := &Store{api: api, authority: &SharedAuthority{}, maxAttempts: 1, requireProducers: true, tables: map[string]string{"SystemConfig": "fixture-SystemConfig", "Leases": "fixture-Leases", "Outbox": "fixture-Outbox", "RecoveryEpochs": "fixture-RecoveryEpochs"}}
			q := s.OutboxQuery{HistoryID: e.HistoryID, Epoch: api.gate.Epoch, Limit: 128}
			switch mode {
			case "registered":
				api.registered = true
			case "closed":
				q.Epoch = e.Epoch
			case "cut in progress":
				api.gate.State = "RecoveryCut"
				api.gate.RecoveryCutID = "11111111111111111111111111111111"
			case "legacy":
				m.requireProducers = false
			case "non-shared authority":
				m.authority = nil
			case "existing epoch":
				api.gate.Epoch = e.Epoch
				q.Epoch = e.Epoch
				api.epoch = &s.RecoveryEpoch{Version: 1, HistoryID: e.HistoryID, Incarnation: api.gate.Incarnation, Epoch: e.Epoch, Revision: 1, State: "Closed", CloseID: "11111111111111111111111111111111", Worker: s.WriterFence{LeaseID: "recovery", Epoch: 1, ServiceIncarnation: api.gate.Incarnation}, StartedAt: time.Unix(1, 0), CutAt: time.Unix(2, 0), Phase: "complete"}
				if err := api.epoch.Validate(); err != nil {
					t.Fatal(err)
				}
			case "epoch read failure":
				api.epochError = errors.New("epoch unavailable")
			case "query failure":
				api.queryError = errors.New("membership unavailable")
			}
			page, err := m.ListOutbox(t.Context(), q)
			if mode == "query failure" || mode == "epoch read failure" {
				if err == nil || api.scans != 0 {
					t.Fatal("failed membership observation became successful discovery", page, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "empty open" {
				if api.scans != 0 || api.queries != 1 || len(page.Events) != 0 || page.Next != "" {
					t.Fatal("idle exporter rescanned old history", page, api.scans, api.queries)
				}
			} else if api.scans != 1 {
				t.Fatal("closed or populated discovery skipped the independent Outbox scan", api.scans)
			}
			if (mode == "closed" || mode == "existing epoch") && (len(page.Events) != 1 || api.queries != 0) {
				t.Fatal("missing membership hid an unregistered Closed event", page, api.queries)
			}
		})
	}
}
