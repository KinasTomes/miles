package admin

import (
	"context"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/stats"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Auth fixture only: actual calculator/adapter/collection tests run separately.
type fixtureStatistics struct {
	calls int
	key   string
}

func (f *fixtureStatistics) Global(context.Context) (stats.Summary, error) {
	f.calls++
	return stats.Summary{}, nil
}
func (f *fixtureStatistics) Bucket(_ context.Context, b string) (stats.BucketSummary, error) {
	f.calls++
	return stats.BucketSummary{Bucket: b}, nil
}
func (f *fixtureStatistics) Layout(_ context.Context, b, k string) (stats.Layout, error) {
	f.calls++
	f.key = k
	return stats.Layout{Bucket: b, Key: k}, nil
}
func TestStatisticsAuthAndEncoding(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	f := &fixtureStatistics{}
	w.opts.Statistics = f
	var err error
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/admin/v1/stats", "/admin/v1/buckets/demo/stats", "/admin/v1/objects/layout?bucket=demo&key=k"} {
		before := f.calls
		w.expect(w.do("GET", target, "", ""), http.StatusUnauthorized, "unauthorized")
		for name, token := range w.bearer {
			p := w.f.Principals[name]
			if !p.SystemAdmin && p.UserStatus != "disabled" && p.AuthStatus != "revoked" {
				w.expect(w.do("GET", target, token, ""), http.StatusForbidden, "forbidden")
			}
		}
		if f.calls != before {
			t.Fatal("unauthorized request reached statistics")
		}
		rec := w.must(w.do("GET", target, w.root, ""), http.StatusOK)
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(rec.Header())
		}
	}
	for _, k := range []string{"Tiếng Việt/海", "a+b c", "%2F/%252F", "//a/../b/"} {
		q := url.Values{"bucket": {"demo"}, "key": {k}}
		w.must(w.do("GET", "/admin/v1/objects/layout?"+q.Encode(), w.root, ""), http.StatusOK)
		if f.key != k {
			t.Fatal("key changed", f.key, k)
		}
	}
	for _, query := range []string{"bucket=demo", "bucket=demo&key=k&key=x", "bucket=demo&key=k&x=1", "bucket=demo&key=%zz"} {
		w.expect(w.do("GET", "/admin/v1/objects/layout?"+query, w.root, ""), http.StatusBadRequest, "invalid_request")
	}
	rec := w.must(w.do("GET", "/admin/v1/stats", w.root, ""), http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"dedup_ratio":null`) {
		t.Fatal(rec.Body.String())
	}
	w.opts.Statistics = nil
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	w.expect(w.do("GET", "/admin/v1/stats", w.root, ""), http.StatusServiceUnavailable, "unavailable")
}
