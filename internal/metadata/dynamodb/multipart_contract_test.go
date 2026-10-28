package dynamodb

import (
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"testing"
	"time"
)

func TestLocalMultipartContract(t *testing.T) {
	contracttest.RunMultipart(t, func(t *testing.T, f contracttest.Fixture) contracttest.MultipartHarness {
		m, _ := fixtureStore(t, f)
		now := f.Now
		m.now = func() time.Time { return now }
		m.authority = fixtureAuthority{m.now}
		return contracttest.MultipartHarness{Metadata: m, Store: m, SetTime: func(n time.Time) { now = n }}
	})
}
