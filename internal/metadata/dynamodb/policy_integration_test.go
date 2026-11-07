package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/config"
	contracts "github.com/KinasTomes/miles/internal/metadata/contracttest"
	s "github.com/KinasTomes/miles/internal/storage"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestLocalPolicyPersistence(t *testing.T) {
	contracts.RunPolicyPersistence(t, func(t *testing.T, f shared.Fixture) shared.MetadataHarness {
		m, _ := fixtureStore(t, f)
		fault := func(on bool) func() error {
			if on {
				return func() error { return s.ErrUnavailable }
			}
			return nil
		}
		return shared.MetadataHarness{Store: m, SetBucket: func(b s.Bucket) {
			t.Helper()
			if err := m.update(context.Background(), func(tx *transaction) error { return put(tx, key("buckets", b.Name), b) }); err != nil {
				t.Fatal(err)
			}
		}, FailCommit: func(before, after bool) { m.beforePublish = fault(before); m.afterPublish = fault(after) }}
	})
}

type policyCurrentReadHook struct {
	API
	key   Key
	after func()
}

func (a *policyCurrentReadHook) GetItem(ctx context.Context, in *ddb.GetItemInput, opts ...func(*ddb.Options)) (*ddb.GetItemOutput, error) {
	out, err := a.API.GetItem(ctx, in, opts...)
	if err == nil && a.after != nil && reflect.DeepEqual(in.Key, a.key.Attributes) {
		after := a.after
		a.after = nil
		after()
	}
	return out, err
}

func TestLocalBucketPolicyConcurrentHistoryReturnsConflict(t *testing.T) {
	f := shared.NewFixture()
	m, _ := fixtureStore(t, f)
	write := func(label string, expected uint64, cache config.CachePolicy) (s.PolicySnapshot, error) {
		op := strings.Repeat(label, 32)
		ctx := s.WithMutation(context.Background(), s.MutationContext{HistoryID: strings.Repeat("1", 32), LogicalOpID: op, MutationID: op, Writer: f.Lease.Fence})
		doc, err := json.Marshal(struct {
			Cache config.CachePolicy `json:"cache"`
		}{cache})
		if err != nil {
			t.Fatal(err)
		}
		return m.PutBucketPolicy(ctx, s.BucketPolicyUpdate{Bucket: f.Bucket.Name, BucketID: f.Bucket.ID, CredentialID: "owner-key", Writer: f.Lease.Fence, ExpectedRevision: expected, Candidate: s.PolicySnapshot{Version: 1, Document: doc}})
	}
	initial := config.DefaultCachePolicy()
	if _, err := write("a", 0, initial); err != nil {
		t.Fatal(err)
	}
	address, err := resolve(key("bucket-cache-current", f.Bucket.ID))
	if err != nil {
		t.Fatal(err)
	}
	hook := &policyCurrentReadHook{API: m.api, key: address.key}
	hook.after = func() {
		winner := initial
		winner.AdmitOnWrite = !winner.AdmitOnWrite
		if value, err := write("b", 1, winner); err != nil || value.Revision != 2 {
			t.Fatal("concurrent winner", value, err)
		}
	}
	m.api = hook
	loser := initial
	loser.ReadaheadChunks = 0
	if _, err := write("c", 1, loser); !errors.Is(err, s.ErrConflict) {
		t.Fatal("concurrent history was treated as an unknown outcome", err)
	}
	if hook.after != nil {
		t.Fatal("concurrent publication was not injected")
	}
}
