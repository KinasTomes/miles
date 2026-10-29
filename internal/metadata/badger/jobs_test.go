package badger

import (
	"context"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	s "github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

func TestJobsNodesContract(t *testing.T) {
	opstest.Run(t, func(t *testing.T) opstest.Harness {
		m, e := Open(Options{Path: t.TempDir(), Now: func() time.Time { return opstest.Now }})
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if e := m.Close(); e != nil {
				t.Error(e)
			}
		})
		set := func(k string, v any) {
			t.Helper()
			if e := m.update(context.Background(), func(tx *db.Txn) error { return put(tx, k, v) }); e != nil {
				t.Fatal(e)
			}
		}
		return opstest.Harness{Store: m, Peer: m, Policies: m, SetPolicy: func(p s.PolicySnapshot) { set(policyKey(p.Revision), p); set("policy-current", p.Revision) }, RemovePolicy: func(r uint64) {
			if e := m.update(context.Background(), func(tx *db.Txn) error { return tx.Delete([]byte(policyKey(r))) }); e != nil {
				t.Fatal(e)
			}
		}, SetLease: func(l s.WriterLease) { set(key("writers", l.Fence.LeaseID), l) }}
	})
}
func TestJobsSurviveStoreRestart(t *testing.T) {
	path := t.TempDir()
	m, e := Open(Options{Path: path})
	if e != nil {
		t.Fatal(e)
	}
	p := opstest.Policy(1)
	if e = m.update(context.Background(), func(tx *db.Txn) error {
		if e := put(tx, policyKey(1), p); e != nil {
			return e
		}
		return put(tx, "policy-current", uint64(1))
	}); e != nil {
		t.Fatal(e)
	}
	j, e := (jobs.Service{Store: m, Policies: m}).Create(context.Background(), jobs.Create{Type: "gc"})
	if e != nil {
		t.Fatal(e)
	}
	if e = m.Close(); e != nil {
		t.Fatal(e)
	}
	m, e = Open(Options{Path: path})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	got, e := m.GetJob(context.Background(), j.ID)
	if e != nil || got.ID != j.ID || got.State != jobs.Queued {
		t.Fatal(got, e)
	}
}
