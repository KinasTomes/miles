package admin

import (
	"context"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/stats"
	st "github.com/KinasTomes/miles/internal/storage"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Auth fixture only: actual calculator/adapter/collection tests run separately.
type fixtureStatistics struct {
	calls int
	key   string
}

type realEmptyInventory struct{}

func (realEmptyInventory) CollectInventory(context.Context, int) (st.PhysicalInventory, error) {
	return st.PhysicalInventory{Complete: true, VersionScope: "all-retained"}, nil
}
func TestStatisticsActualBadgerCollectorHTTP(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	svc, err := stats.New(stats.Options{Store: w.store, Inventory: realEmptyInventory{}, Now: func() time.Time { return w.clk.Now().WallUTC }, MaxRecords: 100, MaxReferences: 100, MaxPhysicalObjects: 100, Timeout: time.Minute, MaxAge: time.Minute, Interval: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.opts.Statistics = svc
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	g := decode[stats.Summary](t, w.must(w.do("GET", "/admin/v1/stats", w.root, ""), http.StatusOK))
	b := decode[stats.BucketSummary](t, w.must(w.do("GET", "/admin/v1/buckets/demo/stats", w.root, ""), http.StatusOK))
	if g.SnapshotID == "" || g.SnapshotID != b.SnapshotID || g.BytesStored == nil || *g.BytesStored != 0 || g.TotalReduction != nil {
		t.Fatal(g, b)
	}
	w.expect(w.do("GET", "/admin/v1/objects/layout?bucket=missing&key=k", w.root, ""), http.StatusNotFound, "bucket_not_found")
	w.expect(w.do("GET", "/admin/v1/objects/layout?bucket=demo&key=missing", w.root, ""), http.StatusNotFound, "object_not_found")
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

func TestBucketStatisticsErrorOrder(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	f := &fixtureStatistics{}
	w.opts.Statistics = f
	var err error
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, bucket, query, token, code string
		status                           int
	}{
		{"missing bearer first", "missing", "?unexpected=1", "", "unauthorized", 401},
		{"invalid bearer first", "missing", "?unexpected=1", "invalid", "unauthorized", 401},
		{"missing bucket before owner role", "missing", "", w.bearer["owner"], "bucket_not_found", 404},
		{"missing bucket before stranger role", "missing", "?unexpected=1", w.bearer["stranger"], "bucket_not_found", 404},
		{"missing bucket before query", "missing", "?unexpected=1", w.root, "bucket_not_found", 404},
		{"owner forbidden before query", "demo", "?unexpected=1", w.bearer["owner"], "forbidden", 403},
		{"stranger forbidden", "demo", "", w.bearer["stranger"], "forbidden", 403},
		{"existing bucket invalid query", "demo", "?unexpected=1", w.root, "invalid_request", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Keep fixture failures attached to this case rather than its parent.
			cw := *w
			cw.t = t
			before := f.calls
			rec := cw.do("GET", "/admin/v1/buckets/"+tc.bucket+"/stats"+tc.query, tc.token, "")
			cw.expect(rec, tc.status, tc.code)
			if f.calls != before {
				t.Fatal("rejected request reached statistics provider")
			}
			if tc.status == 401 && rec.Header().Get("WWW-Authenticate") != "Bearer" {
				t.Fatal("missing Bearer challenge")
			}
		})
	}
	rec := w.must(w.do("GET", "/admin/v1/buckets/demo/stats", w.root, ""), http.StatusOK)
	if f.calls != 1 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("authorized statistics request", f.calls, rec.Header())
	}
	w.opts.Statistics = nil
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	w.expect(w.do("GET", "/admin/v1/buckets/missing/stats?unexpected=1", w.root, ""), 404, "bucket_not_found")
	w.expect(w.do("GET", "/admin/v1/buckets/demo/stats", w.root, ""), 503, "unavailable")
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
