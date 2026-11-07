package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
)

type measuredTransactions struct {
	API
	mu                                       sync.Mutex
	footprint                                bool
	calls, conflicts, throttles              int
	capacity                                 float64
	maxActions, maxWire, maxItem, maxWritten int
}

func (m *measuredTransactions) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	if m.footprint {
		var wire, largest, written int
		serializer := ddb.New(ddb.Options{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: wireClient(func(r *http.Request) (*http.Response, error) {
			n, err := io.Copy(io.Discard, r.Body)
			wire = int(n)
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("{}")), Request: r}, err
		})})
		if _, err := serializer.TransactWriteItems(ctx, in); err != nil {
			return nil, err
		}
		for _, action := range in.TransactItems {
			if action.Put == nil {
				continue
			}
			n, err := ItemSizeUpperBound(action.Put.Item)
			if err != nil {
				return nil, err
			}
			largest = max(largest, n)
			written += n
		}
		m.mu.Lock()
		m.maxActions = max(m.maxActions, len(in.TransactItems))
		m.maxWire = max(m.maxWire, wire)
		m.maxItem = max(m.maxItem, largest)
		m.maxWritten = max(m.maxWritten, written)
		m.mu.Unlock()
		return m.API.TransactWriteItems(ctx, in, opts...)
	}
	request := *in
	request.ReturnConsumedCapacity = types.ReturnConsumedCapacityTotal
	out, err := m.API.TransactWriteItems(ctx, &request, opts...)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls++
	if out != nil {
		for _, entry := range out.ConsumedCapacity {
			m.capacity += aws.ToFloat64(entry.CapacityUnits)
		}
	}
	var cancelled *types.TransactionCanceledException
	var service smithy.APIError
	if errors.As(err, &cancelled) {
		for _, reason := range cancelled.CancellationReasons {
			switch aws.ToString(reason.Code) {
			case "TransactionConflict", "ConditionalCheckFailed":
				m.conflicts++
			case "ProvisionedThroughputExceeded", "ThrottlingError":
				m.throttles++
			}
		}
	} else if errors.As(err, &service) {
		switch service.ErrorCode() {
		case "TransactionConflictException":
			m.conflicts++
		case "ProvisionedThroughputExceededException", "ThrottlingException", "RequestLimitExceeded":
			m.throttles++
		}
	}
	return out, err
}

func measureContractFootprint(t *testing.T, store *Store) {
	t.Helper()
	if os.Getenv("MILES_DDB_DEV_MEASURE") != "1" {
		return
	}
	meter := &measuredTransactions{API: store.api, footprint: true}
	store.api = meter
	t.Cleanup(func() {
		t.Logf("footprint max_actions=%d max_sdk_wire_bytes=%d max_native_put_item_bytes=%d max_native_put_sum_bytes=%d",
			meter.maxActions, meter.maxWire, meter.maxItem, meter.maxWritten)
	})
}

func TestAWSDevHotKeys(t *testing.T) {
	client, _, _ := liveClients(t)
	f := contracttest.NewFixture()
	f.Quota.Limit = 1 << 20
	m, _ := fixtureStoreWithClient(t, f, client, "miles_hai_contract_", true)
	runHotKeys(t, f, m)
}

func TestLocalDevHotKeys(t *testing.T) {
	if os.Getenv("MILES_DDB_DEV_MEASURE") != "1" {
		t.Skip("bounded contention probe not enabled")
	}
	f := contracttest.NewFixture()
	f.Quota.Limit = 1 << 20
	m, _ := fixtureStore(t, f)
	runHotKeys(t, f, m)
}

func runHotKeys(t *testing.T, f contracttest.Fixture, m *Store) {
	t.Helper()
	const operations, concurrent = 16, 4
	base := m.api
	defer func() { m.api = base }()
	for _, scenario := range []string{"owner-shared", "owner-distributed", "chunk-shared", "chunk-distributed"} {
		t.Run(scenario, func(t *testing.T) {
			m.api = base
			requests := make([]s.Reservation, operations)
			if strings.HasPrefix(scenario, "owner-") {
				for i := range requests {
					bucket := f.Bucket
					quota := f.Quota
					if scenario == "owner-distributed" {
						bucket.Name = fmt.Sprintf("dev-owner-%d", i)
						bucket.ID, bucket.OwnerID = bucket.Name, bucket.Name
						quota.OwnerID = bucket.OwnerID
						if err := m.update(t.Context(), func(tx *transaction) error {
							if err := put(tx, key("buckets", bucket.Name), bucket); err != nil {
								return err
							}
							return put(tx, key("quotas", quota.OwnerID), quota)
						}); err != nil {
							t.Fatal(err)
						}
					}
					tag := fmt.Sprintf("%s-%d", scenario, i)
					requests[i] = s.Reservation{Version: s.ModelVersion, ID: tag, OpID: f.Request(tag, 0).Head.OpID, OwnerID: bucket.OwnerID,
						Object: s.ObjectKey{Bucket: bucket.Name, BucketID: bucket.ID, Key: tag},
						Bytes:  4, StagedBytes: 4, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(time.Hour), Writer: f.Lease.Fence}
				}
			}
			meter := &measuredTransactions{API: base}
			m.api = meter
			elapsed := make([]time.Duration, operations)
			outcomes := make([]error, operations)
			inserted := make([]bool, operations)
			var group sync.WaitGroup
			for start := 0; start < operations; start += concurrent {
				for i := start; i < start+concurrent; i++ {
					group.Go(func() {
						begin := time.Now()
						if strings.HasPrefix(scenario, "owner-") {
							outcomes[i] = m.ReserveLogicalQuota(t.Context(), requests[i])
						} else {
							chunk := f.Chunk("default")
							chunk.Key.Hash = fmt.Sprintf("%064x", i+2)
							if scenario == "chunk-shared" {
								chunk.Key.Hash = fmt.Sprintf("%064x", 1)
							}
							_, inserted[i], outcomes[i] = m.PutChunkIfAbsent(t.Context(), chunk, f.Lease.Fence)
						}
						elapsed[i] = time.Since(begin)
					})
				}
				group.Wait()
			}
			successes, denied, unavailable, inserts := 0, 0, 0, 0
			for i, err := range outcomes {
				switch {
				case err == nil:
					successes++
				case errors.Is(err, s.ErrConflict):
					denied++
				case errors.Is(err, s.ErrUnavailable):
					unavailable++
				default:
					t.Errorf("probe operation %d: %v", i, err)
				}
				if inserted[i] {
					inserts++
				}
			}
			slices.Sort(elapsed)
			t.Logf("operations=%d concurrency=%d success=%d conflicts=%d unavailable=%d canonical_inserts=%d p50=%s p95=%s transaction_calls=%d conflict_reasons=%d throttle_reasons=%d returned_capacity=%.2f",
				operations, concurrent, successes, denied, unavailable, inserts, elapsed[(operations+1)/2-1], elapsed[(95*operations+99)/100-1], meter.calls, meter.conflicts, meter.throttles, meter.capacity)
			m.api = base
			for i, r := range requests {
				if r.ID == "" || outcomes[i] != nil {
					continue
				}
				if err := m.ReleaseLogicalQuota(t.Context(), s.ReservationRelease{ID: r.ID, OpID: r.OpID, ExpectedRevision: 1, Writer: r.Writer}); err != nil {
					t.Error("release probe reservation", err)
				}
			}
			if scenario == "chunk-shared" && inserts != 1 {
				t.Error("shared canonical insert must have exactly one winner", inserts)
			}
		})
	}
}
