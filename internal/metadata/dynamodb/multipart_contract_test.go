package dynamodb

import (
	"context"
	"testing"
	"time"

	producertest "github.com/KinasTomes/miles/internal/metadata/contracttest"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestLocalMultipartContract(t *testing.T) {
	runMultipartContract(t, fixtureStore)
}

func TestAWSDevMultipartContract(t *testing.T) {
	client, _, _ := liveClients(t)
	runMultipartContract(t, func(t *testing.T, f contracttest.Fixture) (*Store, *ddb.Client) {
		return fixtureStoreWithClient(t, f, client, "miles_hai_contract_", true)
	})
}

func runMultipartContract(t *testing.T, factory func(*testing.T, contracttest.Fixture) (*Store, *ddb.Client)) {
	t.Helper()
	contracttest.RunMultipart(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := factory(t, f)
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
