package badger

import (
	"context"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	db "github.com/dgraph-io/badger/v4"
)

func TestMetadataContract(t *testing.T) {
	contracttest.RunMetadata(t, func(t *testing.T, f contracttest.Fixture) contracttest.MetadataHarness {
		m, _ := fixtureStore(t, f)
		t.Cleanup(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
		set := func(k string, v any) {
			t.Helper()
			if err := m.update(context.Background(), func(tx *db.Txn) error { return put(tx, k, v) }); err != nil {
				t.Fatal(err)
			}
		}
		fault := func(on bool) func() error {
			if !on {
				return nil
			}
			return func() error { return s.ErrUnavailable }
		}
		return contracttest.MetadataHarness{Store: m, SetPack: func(v s.PackInfo) { set(packKey(v.Ref), v) }, SetBucket: func(v s.Bucket) { set(key("buckets", v.Name), v) }, RemoveBucket: func(name string) {
			if err := m.update(context.Background(), func(tx *db.Txn) error { return backend(tx.Delete([]byte(key("buckets", name)))) }); err != nil {
				t.Fatal(err)
			}
		}, SetCredential: func(v s.Credential) { set(key("credentials", v.ID), v) }, SetUser: func(v s.User) { set(key("users", v.ID), v) }, SetGrant: func(v s.Grant) { set(key("grants", v.BucketID, v.UserID), v) }, SetQuota: func(v s.LogicalQuota) { set(key("quotas", v.OwnerID), v) }, SetWriter: func(v s.WriterLease) { set(key("writers", v.Fence.LeaseID), v) }, FailCommit: func(before, after bool) { m.beforePublish = fault(before); m.afterPublish = fault(after) }, FailReconcile: func(on bool) { m.reconcile = fault(on) }}
	})
}
