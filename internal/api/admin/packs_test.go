package admin

import (
	"context"
	"encoding/json"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	s "github.com/KinasTomes/miles/internal/storage"
	"strings"
	"testing"
	"time"
)

type packOperations struct{ testOperations }

type unavailablePackInventory struct{ calls int }

func (i *unavailablePackInventory) Collect(context.Context) ([]lifecycle.Pack, map[s.PackRef][]s.Chunk, error) {
	i.calls++
	return nil, nil, s.ErrUnavailable
}

func TestPackActionsDoNotDependOnClusterInventory(t *testing.T) {
	for _, fault := range []string{"unavailable", "over-budget", "not-installed"} {
		t.Run(fault, func(t *testing.T) {
			ref := s.PackRef{ID: "pack-a", Generation: "generation-1"}
			w := newWorld(t, badger.Bootstrap{Packs: []s.PackInfo{
				{Version: 1, Ref: ref, Size: 100, State: "Cold", Class: s.GlacierIR, StateRevision: 1},
				{Version: 1, Ref: s.PackRef{ID: "unrelated", Generation: "generation-2"}, Size: 100, State: "Warm", Class: s.Standard, StateRevision: 1},
			}})
			unavailable := &unavailablePackInventory{}
			switch fault {
			case "unavailable":
				w.opts.PackInventory = unavailable
			case "over-budget":
				w.opts.PackInventory = &lifecycle.Catalog{Store: w.store, Now: func() time.Time { return w.clk.Now().WallUTC }, MaxRecords: 1, MaxReferences: 1}
			}
			w.opts.Operations = packOperations{testOperations{w.store, jobs.Service{Store: w.store, Policies: w.store, Now: func() time.Time { return w.clk.Now().WallUTC }}}}
			var e error
			w.h, e = NewServer(w.opts)
			if e != nil {
				t.Fatal(e)
			}
			w.must(w.do("GET", "/admin/v1/packs", w.root, ""), 503)
			calls := unavailable.calls
			for _, action := range []string{"restore", "promote"} {
				path := "/admin/v1/packs/pack-a/generation-1/" + action
				j := decode[jobs.Detail](t, w.must(w.do("POST", path, w.root, `{}`), 202))
				stored, e := w.store.GetJob(t.Context(), j.ID)
				if e != nil {
					t.Fatal(e)
				}
				params, e := lifecycle.Decode(stored.Params, stored.Type)
				if e != nil || params.PackID != ref.ID || params.Generation != ref.Generation || stored.Type != jobs.Type("pack_"+action) {
					t.Fatal(stored, params, e)
				}
				w.expect(w.do("POST", "/admin/v1/packs/pack-a/missing/"+action, w.root, `{}`), 404, "pack_not_found")
			}
			if unavailable.calls != calls {
				t.Fatal("pack action scanned unavailable cluster inventory")
			}
			page, e := w.store.ListJobs(t.Context(), jobs.PageRequest{Limit: 10})
			if e != nil || len(page.Jobs) != 2 {
				t.Fatal(page, e)
			}
		})
	}
}

func (o packOperations) Create(ctx context.Context, q jobs.Create) (jobs.Job, error) {
	if _, e := lifecycle.Decode(q.Params, q.Type); e != nil {
		return jobs.Job{}, e
	}
	return o.service.Create(ctx, q)
}
func TestPackAdminInventoryGenerationFiltersAndActions(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{Packs: []s.PackInfo{
		{Version: 1, Ref: s.PackRef{ID: "pack-a", Generation: "generation-1"}, Size: 100, State: "Cold", Class: s.GlacierIR, StateRevision: 2, ProtectedUntil: testNow.Add(time.Hour), MinRetentionUntil: testNow.Add(90 * 24 * time.Hour)},
		{Version: 1, Ref: s.PackRef{ID: "pack-a", Generation: "generation-2"}, Size: 120, State: "Restored", Class: s.Glacier, StateRevision: 4, RestoredUntil: testNow.Add(time.Hour)},
	}})
	w.opts.PackInventory = &lifecycle.Catalog{Store: w.store, Now: func() time.Time { return w.clk.Now().WallUTC }, MaxRecords: 100, MaxReferences: 100}
	w.opts.Operations = packOperations{testOperations{w.store, jobs.Service{Store: w.store, Policies: w.store, Now: func() time.Time { return w.clk.Now().WallUTC }}}}
	var e error
	w.h, e = NewServer(w.opts)
	if e != nil {
		t.Fatal(e)
	}
	type page struct {
		Items []lifecycle.Pack `json:"items"`
		Next  string           `json:"next_cursor"`
	}
	first := decode[page](t, w.must(w.do("GET", "/admin/v1/packs?limit=1", w.root, ""), 200))
	if len(first.Items) != 1 || !first.Items[0].Readable || first.Items[0].MinRetentionUntil.IsZero() || first.Items[0].LiveRatio == nil || *first.Items[0].LiveRatio != 0 || first.Next == "" {
		t.Fatal(first)
	}
	second := decode[page](t, w.must(w.do("GET", "/admin/v1/packs?limit=1&cursor="+first.Next, w.root, ""), 200))
	if len(second.Items) != 1 || second.Items[0].Generation != "generation-2" || !second.Items[0].Readable {
		t.Fatal(second)
	}
	w.expect(w.do("GET", "/admin/v1/packs?limit=1&state=Cold&cursor="+first.Next, w.root, ""), 400, "invalid_cursor")
	rows := decode[page](t, w.must(w.do("GET", "/admin/v1/packs?state=Cold&storage_class=GLACIER_IR&live_ratio_lt=0.5", w.root, ""), 200))
	if len(rows.Items) != 1 {
		t.Fatal(rows)
	}
	all := decode[page](t, w.must(w.do("GET", "/admin/v1/packs/pack-a", w.root, ""), 200))
	if len(all.Items) != 2 {
		t.Fatal(all)
	}
	w.expect(w.do("GET", "/admin/v1/packs/missing", w.root, ""), 404, "pack_not_found")
	for _, q := range []string{"?state=invalid", "?storage_class=invalid", "?live_ratio_lt=nan", "?live_ratio_lt=1/2", "?live_ratio_lt=-1", "?live_ratio_lt=1.1", "?state=Cold&state=Warm"} {
		w.must(w.do("GET", "/admin/v1/packs"+q, w.root, ""), 400)
	}
	for _, tc := range []struct{ path, body string }{{"restore", `{"days":2,"tier":"Bulk"}`}, {"promote", `{}`}} {
		path := "/admin/v1/packs/pack-a/generation-1/" + tc.path
		w.expect(w.do("POST", path, "", tc.body), 401, "unauthorized")
		w.expect(w.do("POST", path, w.bearer["owner"], tc.body), 403, "forbidden")
		j := decode[jobs.Detail](t, w.must(w.do("POST", path, w.root, tc.body), 202))
		if !strings.HasPrefix(j.ID, "job_") {
			t.Fatal(j)
		}
		w.must(w.do("GET", "/admin/v1/jobs/"+j.ID, w.root, ""), 200)
	}
	w.expect(w.do("GET", "/admin/v1/packs", "", ""), 401, "unauthorized")
	w.expect(w.do("GET", "/admin/v1/packs", w.bearer["owner"], ""), 403, "forbidden")
	w.expect(w.do("POST", "/admin/v1/packs/pack-a/missing/restore", w.root, `{}`), 404, "pack_not_found")
	for _, body := range []string{`{"days":0}`, `{"days":31}`, `{"tier":"unknown"}`, `{"days":1,"unknown":true}`} {
		w.must(w.do("POST", "/admin/v1/packs/pack-a/generation-1/restore", w.root, body), 400)
	}
	jobsPage, e := w.store.ListJobs(t.Context(), jobs.PageRequest{Limit: 10})
	if e != nil || len(jobsPage.Jobs) != 2 {
		t.Fatal(jobsPage, e)
	}
	var p lifecycle.Params
	if json.Unmarshal(jobsPage.Jobs[0].Params, &p) != nil || p.PackID != "pack-a" || p.Generation != "generation-1" {
		t.Fatal(p)
	}
}
