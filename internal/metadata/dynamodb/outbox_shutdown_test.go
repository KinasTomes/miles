package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/recovery"
	s "github.com/KinasTomes/miles/internal/storage"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/smithy-go"
)

type throttledOutboxAPI struct {
	API
	mu             sync.Mutex
	table          string
	starts, faults int
}

func (a *throttledOutboxAPI) Scan(ctx context.Context, in *ddb.ScanInput, options ...func(*ddb.Options)) (*ddb.ScanOutput, error) {
	if in.TableName != nil && *in.TableName == a.table {
		a.mu.Lock()
		if len(in.ExclusiveStartKey) == 0 {
			a.starts++
		} else if a.faults == 0 {
			a.faults++
			a.mu.Unlock()
			return nil, &smithy.GenericAPIError{Code: "ProvisionedThroughputExceededException"}
		}
		a.mu.Unlock()
	}
	return a.API.Scan(ctx, in, options...)
}

func TestLocalShutdownDiscoveryThrottleStillSealsHistoricalAndCurrentEpoch(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	for i := range 134 {
		r, w := f.mutation(t, fmt.Sprintf("shutdown-history-%03d", i))
		if _, err := f.m.MutateControl(ctx, r); err != nil {
			t.Fatal(err)
		}
		f.release(t, w)
	}
	closeRecovery(t, f.m, f.request(0))
	for i := range 7 {
		r, w := f.mutation(t, fmt.Sprintf("shutdown-current-%03d", i))
		if _, err := f.m.MutateControl(ctx, r); err != nil {
			t.Fatal(err)
		}
		f.release(t, w)
	}
	// No new mutations after this point. Real table pages, native metadata CAS
	// and the S3 adapter remain in use; only one later-page capacity error is fake.
	api := &throttledOutboxAPI{API: f.m.api, table: f.m.tables["Outbox"]}
	f.m.api, f.peer.api = api, api
	done := make(chan error, 2)
	for _, store := range []*Store{f.m, f.peer} {
		go func() { done <- f.service(store, f.worker).Flush(ctx) }()
	}
	for range 2 {
		err := <-done
		if err != nil && !errors.Is(err, s.ErrConflict) && !errors.Is(err, s.ErrR1Busy) {
			t.Fatal("concurrent export lost progress or authority", err)
		}
	}
	if err := f.service(f.m, f.worker).Flush(ctx); err != nil {
		t.Fatal("flush did not finish after transient later-page throttle", err)
	}
	if _, err := f.service(f.m, f.worker).CloseAndSeal(ctx, f.request(1)); err != nil {
		t.Fatal(err)
	}
	for epoch := range uint64(2) {
		if _, err := recovery.VerifyCut(ctx, f.objects, f.catalog, epoch); err != nil {
			t.Fatal("independent immutable seal/receipt verification", epoch, err)
		}
	}
	if api.faults != 1 {
		t.Fatal("test did not reach a later native scan page", api.faults)
	}
	t.Logf("verified 134 historical + 7 current producers, concurrent exporters, later-page throttle and two S3-verified seals; discovery starts=%d", api.starts)
}
