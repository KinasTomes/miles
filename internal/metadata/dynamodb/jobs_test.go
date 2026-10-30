package dynamodb

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type closeJobLeaseBeforeCommit struct {
	API
	table string
	close func()
	once  sync.Once
}

func (a *closeJobLeaseBeforeCommit) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, options ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	for _, item := range in.TransactItems {
		if item.Put != nil && aws.ToString(item.Put.TableName) == a.table {
			a.once.Do(a.close)
			break
		}
	}
	return a.API.TransactWriteItems(ctx, in, options...)
}
func setJobLease(t *testing.T, m *Store, l s.WriterLease) {
	t.Helper()
	if e := m.update(context.Background(), func(tx *transaction) error {
		row, e := tx.observe(writerKey(l.Fence.LeaseID))
		if e != nil {
			return e
		}
		row.after, e = nativeItem(l)
		if e != nil {
			return e
		}
		for k, v := range row.key.Attributes {
			row.after[k] = v
		}
		row.dirty = true
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
func TestLocalJobClaimCannotPublishAfterFenceClosure(t *testing.T) {
	f := contracttest.NewFixture()
	f.Now = opstest.Now
	m, client := fixtureStore(t, f)
	ctx := context.Background()
	p := opstest.Policy(1)
	if e := m.update(ctx, func(tx *transaction) error {
		if e := put(tx, policyKey(1), p); e != nil {
			return e
		}
		return put(tx, "policy-current", uint64(1))
	}); e != nil {
		t.Fatal(e)
	}
	if _, e := m.PutNode(ctx, 0, opstest.Node("node-a")); e != nil {
		t.Fatal(e)
	}
	j, e := (jobs.Service{Store: m, Policies: m}).Create(ctx, jobs.Create{Type: "gc"})
	if e != nil {
		t.Fatal(e)
	}
	fence := s.WriterFence{LeaseID: j.ID, ServiceIncarnation: "history-1", Epoch: 1}
	setJobLease(t, m, s.WriterLease{Fence: fence, ExpiresAt: opstest.Now.Add(time.Minute)})
	peer, e := New(Options{API: client, Tables: m.tables, Now: m.now, Authority: m.authority})
	if e != nil {
		t.Fatal(e)
	}
	m.api = &closeJobLeaseBeforeCommit{API: client, table: m.tables["Jobs"], close: func() {
		next := fence
		next.Epoch++
		setJobLease(t, peer, s.WriterLease{Fence: next, ExpiresAt: opstest.Now.Add(time.Minute)})
	}}
	_, e = m.ChangeJob(ctx, jobs.Change{JobID: j.ID, ExpectedRevision: j.Revision, Kind: "claim", NodeID: "node-a", NodeIncarnation: "boot-node-a", Fence: fence})
	if !errors.Is(e, s.ErrFenced) {
		t.Fatal("late claim published after closure", e)
	}
	got, e := peer.GetJob(ctx, j.ID)
	if e != nil || got.State != jobs.Queued || got.Revision != 1 {
		t.Fatal(got, e)
	}
}

func TestLocalJobsNodesContract(t *testing.T) {
	opstest.Run(t, func(t *testing.T) opstest.Harness {
		f := contracttest.NewFixture()
		f.Now = opstest.Now
		m, client := fixtureStore(t, f)
		peer, e := New(Options{API: client, Tables: m.tables, Now: m.now, Authority: m.authority})
		if e != nil {
			t.Fatal(e)
		}
		set := func(k string, v any) {
			t.Helper()
			if e := m.update(context.Background(), func(tx *transaction) error { return put(tx, k, v) }); e != nil {
				t.Fatal(e)
			}
		}
		return opstest.Harness{Store: m, Peer: peer, Policies: m, SetPolicy: func(p s.PolicySnapshot) { set(policyKey(p.Revision), p); set("policy-current", p.Revision) }, RemovePolicy: func(r uint64) {
			a, e := resolve(policyKey(r))
			if e != nil {
				t.Fatal(e)
			}
			if _, e = client.DeleteItem(context.Background(), &ddb.DeleteItemInput{TableName: aws.String(m.tables[a.key.Table]), Key: a.key.Attributes}); e != nil {
				t.Fatal(e)
			}
		}, SetLease: func(l s.WriterLease) {
			if e := m.update(context.Background(), func(tx *transaction) error {
				row, e := tx.observe(writerKey(l.Fence.LeaseID))
				if e != nil {
					return e
				}
				row.after, e = nativeItem(l)
				if e != nil {
					return e
				}
				for k, v := range row.key.Attributes {
					row.after[k] = v
				}
				row.dirty = true
				return nil
			}); e != nil {
				t.Fatal(e)
			}
		}}
	})
}
