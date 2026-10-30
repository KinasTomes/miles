package badger

import (
	"context"
	contracts "github.com/KinasTomes/miles/internal/metadata/contracttest"
	s "github.com/KinasTomes/miles/internal/storage"
	shared "github.com/KinasTomes/miles/internal/storage/contracttest"
	db "github.com/dgraph-io/badger/v4"
	"testing"
)

func TestPolicyPersistence(t *testing.T) {
	contracts.RunPolicyPersistence(t, func(t *testing.T, f shared.Fixture) shared.MetadataHarness {
		m, _ := fixtureStore(t, f)
		t.Cleanup(func() { _ = m.Close() })
		fault := func(on bool) func() error {
			if on {
				return func() error { return s.ErrUnavailable }
			}
			return nil
		}
		return shared.MetadataHarness{Store: m, SetBucket: func(b s.Bucket) {
			t.Helper()
			if err := m.update(context.Background(), func(tx *db.Txn) error { return put(tx, key("buckets", b.Name), b) }); err != nil {
				t.Fatal(err)
			}
		}, FailCommit: func(before, after bool) { m.beforePublish = fault(before); m.afterPublish = fault(after) }}
	})
}
