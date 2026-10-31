package badger

import (
	"context"
	"testing"
	"time"

	producertest "github.com/KinasTomes/miles/internal/metadata/contracttest"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	db "github.com/dgraph-io/badger/v4"
)

func TestMultipartContract(t *testing.T) {
	contracttest.RunMultipart(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := fixtureStore(t, f)
		t.Cleanup(func() { _ = m.Close() })
		now := f.Now
		m.now = func() time.Time { return now }
		setGrant := func(g s.Grant) {
			if err := m.update(context.Background(), func(tx *db.Txn) error { return put(tx, key("grants", g.BucketID, g.UserID), g) }); err != nil {
				t.Fatal(err)
			}
		}
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetStrict: func(v bool) { m.requireProducers = v }, SetTime: func(n time.Time) { now = n }, SetGrant: setGrant}
	})
}

func TestProducerContract(t *testing.T) {
	producertest.RunProducers(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := fixtureStore(t, f)
		t.Cleanup(func() { _ = m.Close() })
		now := f.Now
		m.now = func() time.Time { return now }
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetStrict: func(v bool) { m.requireProducers = v }, SetTime: func(n time.Time) { now = n }}
	})
}

func TestIdentityContract(t *testing.T) {
	producertest.RunIdentity(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := fixtureStore(t, f)
		t.Cleanup(func() { _ = m.Close() })
		now := f.Now
		m.now = func() time.Time { return now }
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetStrict: func(v bool) { m.requireProducers = v }, SetTime: func(n time.Time) { now = n }}
	})
}
