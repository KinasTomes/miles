package admin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/KinasTomes/miles/internal/metadata/badger"
)

func TestCacheHTTPSharedSamplesWeightedRatioAndPurge(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	identity := opstest.Node("node-a")
	cluster := &jobs.Cluster{Runner: &jobs.Runner{Service: jobs.Service{Store: w.store, Policies: w.store, Now: func() time.Time { return w.clk.Now().WallUTC }}}, Registry: &jobs.Registry{Store: w.store, Clock: w.clk, Identity: identity}}
	cluster.Runner.Executors = map[jobs.Type]jobs.Executor{"cache_purge": cluster.CacheExecutor(nil)}
	w.opts.Operations = cluster
	w.opts.CacheStatistics = cluster
	var err error
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{"node-a", "node-b", "node-empty", "node-stale", "node-old-service", "node-historical", "node-no-sample"} {
		n := identity
		n.ID = id
		n.HeartbeatAt = w.clk.Now().WallUTC
		n.Cache = jobs.CacheSample{SampledAt: n.HeartbeatAt, Hits: 9, Misses: 1, Lookups: 10}
		if i == 1 {
			n.Cache.Hits = 1
			n.Cache.Misses = 89
			n.Cache.Lookups = 90
		}
		if i == 2 {
			n.Cache = jobs.CacheSample{SampledAt: n.HeartbeatAt}
		}
		if i == 3 {
			n.HeartbeatAt = n.HeartbeatAt.Add(-time.Minute)
			n.Cache.SampledAt = n.HeartbeatAt
		}
		if i == 4 {
			n.ServiceIncarnation = "old-service"
		}
		if i == 5 {
			n.HeartbeatAt = n.HeartbeatAt.Add(-3 * time.Minute)
			n.Cache.SampledAt = n.HeartbeatAt
		}
		if i == 6 {
			n.Cache = jobs.CacheSample{}
		}
		if _, err = w.store.PutNode(t.Context(), 0, n); err != nil {
			t.Fatal(err)
		}
	}
	r := decode[jobs.CacheReport](t, w.must(w.do("GET", "/admin/v1/cache/stats?scope=cluster", w.root, ""), 200))
	if r.Complete || len(r.Nodes) != 5 || r.IncludedNodes != 3 || r.Denominator != 100 || r.Hits != 10 || r.HitRatio == nil || *r.HitRatio != 0.1 {
		t.Fatal(r)
	}
	empty := decode[jobs.CacheReport](t, w.must(w.do("GET", "/admin/v1/cache/stats?scope=node&node_id=node-empty", w.root, ""), 200))
	if !empty.Complete || empty.Denominator != 0 || empty.HitRatio != nil {
		t.Fatal(empty)
	}
	peer, err := NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	j := decode[jobs.Detail](t, w.must(w.do("POST", "/admin/v1/cache/purge", w.root, `{"scope":"cluster"}`), 202))
	w.h = peer
	w.must(w.do("GET", "/admin/v1/jobs/"+j.ID, w.root, ""), 200)
	page, err := w.store.ListJobs(context.Background(), jobs.PageRequest{Limit: 10})
	if err != nil || len(page.Jobs) != 1 {
		t.Fatal(page, err)
	}
	var p struct {
		Targets []struct {
			NodeID string `json:"node_id"`
		} `json:"targets"`
	}
	if json.Unmarshal(page.Jobs[0].Params, &p) != nil || len(p.Targets) != 5 {
		t.Fatal(string(page.Jobs[0].Params))
	}
	w.must(w.do("POST", "/admin/v1/cache/purge", w.root, `{"scope":"node","node_id":"node-b"}`), 202)
	for _, path := range []string{"/admin/v1/cache/stats?scope=cluster", "/admin/v1/cache/purge"} {
		method, body := "GET", ""
		if path == "/admin/v1/cache/purge" {
			method = "POST"
			body = `{"scope":"cluster"}`
		}
		w.expect(w.do(method, path, "", body), 401, "unauthorized")
		w.expect(w.do(method, path, w.bearer["owner"], body), 403, "forbidden")
	}
	for _, q := range []string{"", "?scope=node", "?scope=cluster&node_id=node-a", "?scope=cluster&unexpected=1", "?scope=node&node_id=node-a&node_id=node-b"} {
		w.must(w.do("GET", "/admin/v1/cache/stats"+q, w.root, ""), 400)
	}
	for _, body := range []string{`{}`, `{"scope":"node"}`, `{"scope":"cluster","node_id":"node-a"}`, `{"scope":"cluster","targets":[]}`} {
		w.must(w.do("POST", "/admin/v1/cache/purge", w.root, body), 400)
	}
	w.expect(w.do("GET", "/admin/v1/cache/stats?scope=node&node_id=missing", w.root, ""), 404, "node_not_found")
	w.expect(w.do("POST", "/admin/v1/cache/purge", w.root, `{"scope":"node","node_id":"missing"}`), 404, "node_not_found")
}
