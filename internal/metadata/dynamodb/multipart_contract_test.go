package dynamodb

import (
	"context"
	"testing"
	"time"

	producertest "github.com/KinasTomes/miles/internal/metadata/contracttest"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
)

func TestLocalMultipartContract(t *testing.T) {
	contracttest.RunMultipart(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := fixtureStore(t, f)
		now := f.Now
		m.now = func() time.Time { return now }
		m.authority = fixtureAuthority{m.now}
		setGrant := func(g s.Grant) {
			if err := m.update(context.Background(), func(tx *transaction) error { return put(tx, key("grants", g.BucketID, g.UserID), g) }); err != nil {
				t.Fatal(err)
			}
		}
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetStrict: func(v bool) { m.requireProducers = v }, SetTime: func(n time.Time) { now = n }, SetGrant: setGrant}
	})
}

func TestLocalProducerContract(t *testing.T) {
	producertest.RunProducers(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := fixtureStore(t, f)
		now := f.Now
		m.now = func() time.Time { return now }
		m.authority = fixtureAuthority{m.now}
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetStrict: func(v bool) { m.requireProducers = v }, SetTime: func(n time.Time) { now = n }}
	})
}

func TestLocalIdentityContract(t *testing.T) {
	producertest.RunIdentity(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := fixtureStore(t, f)
		now := f.Now
		m.now = func() time.Time { return now }
		m.authority = fixtureAuthority{m.now}
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetStrict: func(v bool) { m.requireProducers = v }, SetTime: func(n time.Time) { now = n }}
	})
}
