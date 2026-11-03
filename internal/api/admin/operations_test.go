package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
)

type beforeOperationalWrite struct {
	jobs.Store
	before func()
}

func (s beforeOperationalWrite) CreateJob(ctx context.Context, j jobs.Job) error {
	s.before()
	return s.Store.CreateJob(ctx, j)
}
func (s beforeOperationalWrite) ChangeJob(ctx context.Context, c jobs.Change) (jobs.Job, error) {
	s.before()
	return s.Store.ChangeJob(ctx, c)
}

func TestOperationalMutationsRecheckActorAfterAdmission(t *testing.T) {
	for _, actorChange := range []string{"revoke", "disable_user"} {
		for _, action := range []string{"create", "cancel", "drain"} {
			t.Run(actorChange+"/"+action, func(t *testing.T) {
				w := newWorld(t, badger.Bootstrap{})
				actor := w.mintToken("user-writer", true)
				svc := jobs.Service{Store: w.store, Policies: w.store}
				job, err := svc.Create(t.Context(), jobs.Create{Type: "cache_purge"})
				if err != nil {
					t.Fatal(err)
				}
				if _, err = w.store.PutNode(t.Context(), 0, opstest.Node("node-a")); err != nil {
					t.Fatal(err)
				}
				svc.Store = beforeOperationalWrite{Store: w.store, before: func() {
					if actorChange == "revoke" {
						w.must(w.do("DELETE", "/admin/v1/admin-tokens/"+tokenID(actor), w.root, ""), 204)
					} else {
						w.must(w.do("PATCH", "/admin/v1/users/user-writer", w.root, `{"status":"disabled","expected_revision":1}`), 200)
					}
				}}
				w.opts.Operations = testOperations{w.store, svc}
				w.h, err = NewServer(w.opts)
				if err != nil {
					t.Fatal(err)
				}
				method, path, body := "POST", "/admin/v1/jobs", `{"type":"cache_purge"}`
				switch action {
				case "cancel":
					method, path, body = "DELETE", "/admin/v1/jobs/"+job.ID, ""
				case "drain":
					path, body = "/admin/v1/cluster/nodes/node-a/drain", "{}"
				}
				w.expect(w.do(method, path, actor, body), 401, "unauthorized")
				page, err := w.store.ListJobs(t.Context(), jobs.PageRequest{Limit: 100})
				if err != nil || len(page.Jobs) != 1 || page.Jobs[0].State != jobs.Queued {
					t.Fatal("denied actor changed jobs", page, err)
				}
			})
		}
	}
}

type testOperations struct {
	jobs.Store
	service jobs.Service
}

func (o testOperations) Create(ctx context.Context, r jobs.Create) (jobs.Job, error) {
	if r.Type != "cache_purge" {
		return jobs.Job{}, jobs.ErrUnsupported
	}
	return o.service.Create(ctx, r)
}
func (o testOperations) Cancel(ctx context.Context, id string) (jobs.Job, error) {
	return o.service.Cancel(ctx, id)
}
func (o testOperations) Drain(ctx context.Context, id string) (jobs.Job, error) {
	if _, err := o.GetNode(ctx, id); err != nil {
		return jobs.Job{}, err
	}
	b, _ := json.Marshal(map[string]string{"node_id": id})
	return o.service.Create(ctx, jobs.Create{Type: "drain", Params: b})
}

func TestOperationalRoutesShareStateAndBindCursors(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	w.opts.Operations = testOperations{w.store, jobs.Service{Store: w.store, Policies: w.store, Now: func() time.Time { return w.clk.Now().WallUTC }}}
	// A second HTTP server uses the same metadata and cursor key, without a
	// process-local job map or a cursor specific to the receiving gateway.
	var err error
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"type":"cache_purge","params":{}}`, `{"type":"cache_purge","dry_run":true}`} {
		w.must(w.do("POST", "/admin/v1/jobs", w.root, body), http.StatusAccepted)
	}
	first := w.must(w.do("GET", "/admin/v1/jobs?limit=1", w.root, ""), 200)
	var page struct {
		Items []jobs.Detail `json:"items"`
		Next  string        `json:"next_cursor"`
	}
	if json.Unmarshal(first.Body.Bytes(), &page) != nil || len(page.Items) != 1 || page.Next == "" {
		t.Fatal(first.Body)
	}
	w.h = second
	w.must(w.do("GET", "/admin/v1/jobs?limit=2&cursor="+page.Next, w.root, ""), 200)
	w.expect(w.do("GET", "/admin/v1/cluster/nodes?cursor="+page.Next, w.root, ""), 400, "invalid_cursor")
	id := page.Items[0].ID
	w.must(w.do("GET", "/admin/v1/jobs/"+id, w.root, ""), 200)
	response := w.must(w.do("DELETE", "/admin/v1/jobs/"+id, w.root, ""), 200)
	if !strings.Contains(response.Body.String(), `"state":"cancelled"`) {
		t.Fatal(response.Body)
	}
	for _, s := range []string{"Worker", "Fence", "Checkpoint", "Params", "Secret"} {
		if strings.Contains(response.Body.String(), s) {
			t.Fatal("private worker state leaked")
		}
	}
}

func TestOperationalRoutesValidateAfterAuthorization(t *testing.T) {
	w := newWorld(t, badger.Bootstrap{})
	w.opts.Operations = testOperations{w.store, jobs.Service{Store: w.store, Policies: w.store}}
	var err error
	w.h, err = NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ method, path, body string }{
		{"GET", "/admin/v1/jobs", ""}, {"POST", "/admin/v1/jobs", `{"type":"cache_purge"}`},
		{"GET", "/admin/v1/jobs/missing", ""}, {"DELETE", "/admin/v1/jobs/missing", ""},
		{"GET", "/admin/v1/cluster/nodes", ""}, {"POST", "/admin/v1/cluster/nodes/missing/drain", "{}"},
	} {
		w.expect(w.do(r.method, r.path, "", r.body), 401, "unauthorized")
		w.expect(w.do(r.method, r.path, w.bearer["owner"], r.body), 403, "forbidden")
	}
	for _, body := range []string{`{"type":"cache_purge","params":null}`, `{"type":"cache_purge","params":[]}`, `{"type":"cache_purge","worker":"x"}`, `{"type":"cache_purge","params":{"x":1,"x":2}}`, `{"type":"cache_purge","params":{"x":"` + strings.Repeat("x", 8192) + `"}}`} {
		w.expect(w.do("POST", "/admin/v1/jobs", w.root, body), 400, "invalid_request")
	}
	w.expect(w.do("POST", "/admin/v1/jobs", w.root, `{"type":"gc"}`), 501, "job_type_unavailable")
	w.expect(w.do("GET", "/admin/v1/jobs/missing", w.root, ""), 404, "job_not_found")
	w.expect(w.do("POST", "/admin/v1/cluster/nodes/missing/drain", w.root, "{}"), 404, "node_not_found")
	_, err = w.store.GetJob(context.Background(), "missing")
	if err != storage.ErrNotFound {
		t.Fatal(err)
	}
}
