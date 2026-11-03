package dynamodb

import (
	"context"
	"errors"
	"strings"
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

func TestLocalOperationalWritesRecheckActorAtCommit(t *testing.T) {
	f := contracttest.NewFixture()
	f.Now = opstest.Now
	m, client := fixtureStore(t, f)
	ctx := t.Context()
	token := s.AdminToken{ID: strings.Repeat("a", 32), UserID: "operator", Scope: s.TokenScopeSystem, Hash: make([]byte, 32), Active: true, IssuedAt: f.Now, ExpiresAt: f.Now.Add(time.Hour)}
	policy := opstest.Policy(1)
	policy.Revision = 0
	if err := m.BootstrapAdmin(ctx, s.AdminBootstrap{Token: token, Policy: policy, QuotaLimit: 1024}); err != nil {
		t.Fatal(err)
	}
	peer, err := New(Options{API: client, Tables: m.tables, Now: m.now, Authority: m.authority})
	if err != nil {
		t.Fatal(err)
	}
	writer := s.WriterFence{LeaseID: "api-actor", Epoch: 1, ServiceIncarnation: "history-1"}
	setJobLease(t, m, s.WriterLease{Fence: writer, ExpiresAt: f.Now.Add(time.Hour)})
	actor := jobs.WithAdminActor(ctx, jobs.AdminActor{TokenID: token.ID, Writer: writer})
	for _, change := range []string{"revoke", "disable_user", "expire", "remove_admin_scope"} {
		for _, action := range []string{"create", "cancel", "node"} {
			t.Run(change+"/"+action, func(t *testing.T) {
				m.api = client
				user, err := peer.GetUser(ctx, token.UserID)
				if err != nil {
					t.Fatal(err)
				}
				user.Active = true
				if err := peer.update(ctx, func(tx *transaction) error {
					if err := put(tx, key("users", user.ID), user); err != nil {
						return err
					}
					return put(tx, key("admin-tokens", token.ID), token)
				}); err != nil {
					t.Fatal(err)
				}
				svc := jobs.Service{Store: m, Policies: m}
				job, err := svc.Create(ctx, jobs.Create{Type: "cache_purge"})
				if err != nil {
					t.Fatal(err)
				}
				node, err := m.PutNode(ctx, 0, opstest.Node("node_"+change+"_"+action))
				if err != nil {
					t.Fatal(err)
				}
				table := m.tables["Jobs"]
				if action == "node" {
					table = m.tables["Nodes"]
				}
				m.api = &beforeJobCommit{API: client, table: table, before: func() {
					changed := token
					switch change {
					case "revoke":
						changed.Active = false
					case "disable_user":
						user.Active = false
					case "expire":
						changed.ExpiresAt = f.Now
					case "remove_admin_scope":
						changed.Scope = s.TokenScopeUser
					}
					if err := peer.update(ctx, func(tx *transaction) error {
						if err := put(tx, key("users", user.ID), user); err != nil {
							return err
						}
						return put(tx, key("admin-tokens", token.ID), changed)
					}); err != nil {
						t.Fatal(err)
					}
				}}
				switch action {
				case "create":
					_, err = svc.Create(actor, jobs.Create{Type: "cache_purge"})
				case "cancel":
					_, err = svc.Cancel(actor, job.ID)
				case "node":
					node.Draining, node.Ready = true, false
					_, err = m.PutNode(actor, node.Revision, node)
				}
				if !errors.Is(err, s.ErrAccessDenied) {
					t.Fatal("actor change did not fence the transaction", err)
				}
				got, err := peer.GetJob(ctx, job.ID)
				if err != nil || got.State != jobs.Queued {
					t.Fatal("denied cancellation changed job", got, err)
				}
				gotNode, err := peer.GetNode(ctx, node.ID)
				if err != nil || gotNode.Draining {
					t.Fatal("denied drain changed node", gotNode, err)
				}
			})
		}
	}
}

type beforeJobCommit struct {
	API
	table  string
	before func()
	once   sync.Once
}

func (a *beforeJobCommit) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, options ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	for _, item := range in.TransactItems {
		if item.Put != nil && aws.ToString(item.Put.TableName) == a.table {
			a.once.Do(a.before)
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
	m.api = &beforeJobCommit{API: client, table: m.tables["Jobs"], before: func() {
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

func TestLocalManagedJobCreateIsFencedInItsTransaction(t *testing.T) {
	f := contracttest.NewFixture()
	f.Now = opstest.Now
	m, client := fixtureStore(t, f)
	ctx := context.Background()
	if e := m.update(ctx, func(t *transaction) error {
		if e := put(t, policyKey(1), opstest.Policy(1)); e != nil {
			return e
		}
		return put(t, "policy-current", uint64(1))
	}); e != nil {
		t.Fatal(e)
	}
	fence := s.WriterFence{LeaseID: "schedule_gc", ServiceIncarnation: "history-1", Epoch: 1}
	setJobLease(t, m, s.WriterLease{Fence: fence, ExpiresAt: opstest.Now.Add(time.Minute)})
	peer, e := New(Options{API: client, Tables: m.tables, Now: m.now, Authority: m.authority})
	if e != nil {
		t.Fatal(e)
	}
	m.api = &beforeJobCommit{API: client, table: m.tables["Jobs"], before: func() {
		next := fence
		next.Epoch++
		setJobLease(t, peer, s.WriterLease{Fence: next, ExpiresAt: opstest.Now.Add(time.Minute)})
	}}
	_, e = (jobs.Service{Store: m, Policies: m}).CreateNamed(jobs.WithCreationFence(ctx, fence), jobs.Create{Type: "gc"}, "periodic_gc_123")
	if !errors.Is(e, s.ErrFenced) {
		t.Fatal("late scheduler enqueued after authority transfer", e)
	}
	if _, e = peer.GetJob(ctx, "periodic_gc_123"); !errors.Is(e, s.ErrNotFound) {
		t.Fatal("late scheduler left a job", e)
	}
}
