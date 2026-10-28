package badger

import (
	producertest "github.com/KinasTomes/miles/internal/metadata/contracttest"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"testing"
	"time"
)

func TestMultipartContract(t *testing.T) {
	contracttest.RunMultipart(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := fixtureStore(t, f)
		t.Cleanup(func() { _ = m.Close() })
		now := f.Now
		m.now = func() time.Time { return now }
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetStrict: func(v bool) { m.requireProducers = v }, SetTime: func(n time.Time) { now = n }}
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
