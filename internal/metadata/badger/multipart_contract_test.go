package badger

import (
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
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetTime: func(n time.Time) { now = n }}
	})
}
