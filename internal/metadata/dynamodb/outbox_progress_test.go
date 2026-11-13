package dynamodb

import (
	"context"
	"strings"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type outboxReceiptConflict struct {
	API
	calls int
}

func (a *outboxReceiptConflict) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, options ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	a.calls++
	if a.calls == 1 {
		return nil, &types.TransactionCanceledException{CancellationReasons: []types.CancellationReason{{Code: aws.String("TransactionConflict")}}}
	}
	return a.API.TransactWriteItems(ctx, in, options...)
}

func TestLocalOutboxProgressRetryRetainsWorkerFence(t *testing.T) {
	f := newRecoveryFixture(t)
	r, writer := f.mutation(t, "receipt-conflict")
	result, err := f.m.MutateControl(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	f.release(t, writer)
	api := &outboxReceiptConflict{API: f.m.api}
	f.m.api = api
	progress := s.OutboxExport{Event: result.Event, Worker: f.worker, DescriptorKey: "journal/fixture", DescriptorSHA256: strings.Repeat("a", 64)}
	if err := f.m.MarkOutboxExported(t.Context(), progress); err != nil {
		t.Fatal("CAS retry lost its recovery fence", err)
	}
	got, err := f.m.GetOutboxExport(t.Context(), result.Event.ID)
	if err != nil || got.Worker != (s.WriterFence{}) || got.Event.ID != progress.Event.ID || api.calls != 2 {
		t.Fatal("receipt retained attempt ownership or CAS was not retried", got, err, api.calls)
	}
}
