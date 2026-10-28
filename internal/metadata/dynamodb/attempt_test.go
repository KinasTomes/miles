package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type dispatchFault struct {
	API
	objectTable   string
	before, after bool
	requests      [][]byte
}

func (f *dispatchFault) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, options ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	final := false
	for _, action := range in.TransactItems {
		if action.Put != nil && *action.Put.TableName == f.objectTable {
			final = true
		}
	}
	if final {
		body, _ := json.Marshal(in)
		f.requests = append(f.requests, body)
		if f.before {
			f.before = false
			return nil, s.ErrUnavailable
		}
	}
	out, err := f.API.TransactWriteItems(ctx, in, options...)
	if final && f.after && err == nil {
		f.after = false
		return nil, s.ErrUnavailable
	}
	return out, err
}
func restartFixture(t *testing.T, m *Store) *Store {
	t.Helper()
	restarted, err := New(Options{API: m.api, Tables: m.tables, Now: m.now, Authority: m.authority})
	if err != nil {
		t.Fatal(err)
	}
	return restarted
}

func TestLocalDurableAttemptSurvivesRestartWithExactParameters(t *testing.T) {
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	r := f.Request("durable-dispatch", 0)
	contracttest.Prepare(t, m, f, r, 4)
	fault := &dispatchFault{API: m.api, objectTable: m.tables["Objects"], before: true}
	m.api = fault
	if _, err := m.CommitObject(context.Background(), r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := m.GetCommitOutcome(context.Background(), r.Head.OpID); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("pending attempt incorrectly classified absent", err)
	}
	if err := m.ReleaseLogicalQuota(context.Background(), s.ReservationRelease{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: r.Writer}); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("unknown attempt released", err)
	}
	changed := r
	changed.Head.ETag = "changed"
	if _, err := restartFixture(t, m).CommitObject(context.Background(), changed); !errors.Is(err, s.ErrConflict) {
		t.Fatal("different input resumed original request", err)
	}
	result, err := restartFixture(t, m).CommitObject(context.Background(), r)
	if err != nil || result.Head.Revision != 1 {
		t.Fatal(result, err)
	}
	if len(fault.requests) != 2 || !reflect.DeepEqual(fault.requests[0], fault.requests[1]) {
		t.Fatal("restart changed ClientRequestToken or concrete parameters")
	}
	quota, err := m.GetQuota(context.Background(), f.Quota.OwnerID)
	if err != nil || quota.Used != 4 || quota.Reserved != 0 {
		t.Fatal(quota, err)
	}
}

func TestLocalLostResponseReplaysOriginalOutcomeAfterOverwrite(t *testing.T) {
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	first := f.Request("lost-ack", 0)
	contracttest.Prepare(t, m, f, first, 4)
	fault := &dispatchFault{API: m.api, objectTable: m.tables["Objects"], after: true}
	m.api = fault
	if _, err := m.CommitObject(context.Background(), first); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	second := f.Request("later-write", 1)
	contracttest.Prepare(t, m, f, second, 0)
	if _, err := m.CommitObject(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	before := len(fault.requests)
	restarted := restartFixture(t, m)
	result, err := restarted.CommitObject(context.Background(), first)
	if err != nil || !result.AlreadyCommitted || result.Head.Revision != 1 || result.Head.OpID != first.Head.OpID {
		t.Fatal(result, err)
	}
	if len(fault.requests) != before {
		t.Fatal("original result lookup dispatched another write")
	}
	current, err := m.GetObject(context.Background(), first.Head.Object)
	if err != nil || current.Revision != 2 {
		t.Fatal(current, err)
	}
}

func TestLocalKnownConflictRetiresAttemptWithoutPartialPublication(t *testing.T) {
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	r := f.Request("known-conflict", 0)
	contracttest.Prepare(t, m, f, r, 4)
	fault := &dispatchFault{API: m.api, objectTable: m.tables["Objects"], before: true}
	m.api = fault
	if _, err := m.CommitObject(context.Background(), r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	if err := m.update(context.Background(), func(tx *transaction) error {
		grant := f.Grant
		grant.Write = false
		grant.Revision++
		return put(tx, key("grants", grant.BucketID, grant.UserID), grant)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := restartFixture(t, m).CommitObject(context.Background(), r); !errors.Is(err, s.ErrAccessDenied) {
		t.Fatal(err)
	}
	contracttest.NoPublication(t, m, r)
	// Restore current authorization with a new revision. Replaying the exact old
	// request must cancel on its stale grant predicate, then retire the attempt.
	if err := m.update(context.Background(), func(tx *transaction) error {
		grant := f.Grant
		grant.Revision += 2
		return put(tx, key("grants", grant.BucketID, grant.UserID), grant)
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := restartFixture(t, m).CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
	q, err := m.GetQuota(ctx, f.Quota.OwnerID)
	if err != nil || q.Used != 4 || q.Reserved != 0 {
		t.Fatal(q, err)
	}
}
