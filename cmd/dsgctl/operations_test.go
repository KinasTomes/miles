package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/jobs"
)

func TestOperationalCommandsUseBearerMethodAndWireSchema(t *testing.T) {
	_, _, token, e := admin.NewToken()
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(t.TempDir(), "token")
	if e = os.WriteFile(file, []byte(token), 0600); e != nil {
		t.Fatal(e)
	}
	params := filepath.Join(t.TempDir(), "params.json")
	if e = os.WriteFile(params, []byte(`{"limit":8}`), 0600); e != nil {
		t.Fatal(e)
	}
	tests := []struct {
		args         []string
		method, path string
		body         map[string]any
	}{
		{[]string{"job", "ls", "--limit", "7", "--cursor", "opaque.token"}, "GET", "/admin/v1/jobs?cursor=opaque.token&limit=7", nil},
		{[]string{"job", "show", "job-a"}, "GET", "/admin/v1/jobs/job-a", nil},
		{[]string{"job", "cancel", "job-a"}, "DELETE", "/admin/v1/jobs/job-a", nil},
		{[]string{"node", "drain", "edge-a"}, "POST", "/admin/v1/cluster/nodes/edge-a/drain", map[string]any{}},
		{[]string{"job", "run", "scrub", "--dry-run", "--policy-revision", "4", "--params-file", params}, "POST", "/admin/v1/jobs", map[string]any{"type": "scrub", "dry_run": true, "policy_revision": float64(4), "params": map[string]any{"limit": float64(8)}}},
		{[]string{"job", "ls"}, "GET", "/admin/v1/jobs", nil},
		{[]string{"node", "ls", "--cursor", "k.p.t"}, "GET", "/admin/v1/cluster/nodes?cursor=k.p.t", nil},
		{[]string{"stats"}, "GET", "/admin/v1/stats", nil},
		{[]string{"stats", "--bucket", "demo.bucket"}, "GET", "/admin/v1/buckets/demo.bucket/stats", nil},
		// One query encoding: space, slash, plus, percent, hash and non-ASCII stay in key.
		{[]string{"layout", "demo", "dir/a b+c%#é"}, "GET", "/admin/v1/objects/layout?bucket=demo&key=dir%2Fa+b%2Bc%25%23%C3%A9", nil},
		{[]string{"layout", "demo", "--not-a-flag"}, "GET", "/admin/v1/objects/layout?bucket=demo&key=--not-a-flag", nil},
		{[]string{"pack", "ls"}, "GET", "/admin/v1/packs", nil},
		{[]string{"pack", "ls", "--state", "Cold", "--storage-class", "GLACIER_IR", "--live-ratio-lt", "0.5", "--limit", "2", "--cursor", "k.p.t"}, "GET", "/admin/v1/packs?cursor=k.p.t&limit=2&live_ratio_lt=0.5&state=Cold&storage_class=GLACIER_IR", nil},
		{[]string{"pack", "show", "pack-a", "--limit", "1"}, "GET", "/admin/v1/packs/pack-a?limit=1", nil},
		{[]string{"pack", "restore", "pack-a", "generation-1", "--days", "3", "--tier", "Bulk"}, "POST", "/admin/v1/packs/pack-a/generation-1/restore", map[string]any{"days": float64(3), "tier": "Bulk"}},
		{[]string{"pack", "restore", "pack-a", "generation-1"}, "POST", "/admin/v1/packs/pack-a/generation-1/restore", map[string]any{}},
		{[]string{"pack", "promote", "pack-a", "generation-1"}, "POST", "/admin/v1/packs/pack-a/generation-1/promote", map[string]any{}},
		{[]string{"cache", "stats", "--scope", "cluster"}, "GET", "/admin/v1/cache/stats?scope=cluster", nil},
		{[]string{"cache", "stats", "--scope", "node", "--node", "edge-a"}, "GET", "/admin/v1/cache/stats?node_id=edge-a&scope=node", nil},
		{[]string{"cache", "purge", "--scope", "cluster"}, "POST", "/admin/v1/cache/purge", map[string]any{"scope": "cluster"}},
		{[]string{"cache", "purge", "--scope", "node", "--node", "edge-a"}, "POST", "/admin/v1/cache/purge", map[string]any{"scope": "node", "node_id": "edge-a"}},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != test.method || r.URL.RequestURI() != test.path || r.Header.Get("Authorization") != "Bearer "+token {
					t.Errorf("wrong transport %s %s", r.Method, r.URL.RequestURI())
				}
				body, e := io.ReadAll(r.Body)
				if e != nil {
					t.Error(e)
				}
				if test.body != nil {
					var got map[string]any
					if json.Unmarshal(body, &got) != nil {
						t.Fatal("invalid body")
					}
					want, _ := json.Marshal(test.body)
					actual, _ := json.Marshal(got)
					if string(actual) != string(want) {
						t.Errorf("body %s != %s", actual, want)
					}
					if r.Header.Get("Content-Type") != "application/json" {
						t.Error("missing Content-Type")
					}
				}
				w.WriteHeader(202)
				w.Write([]byte(`{"job_id":"job-a","state":"queued"}`))
			}))
			defer server.Close()
			var out, diagnostics bytes.Buffer
			args := append([]string{"--endpoint", server.URL, "--token-file", file}, test.args...)
			if e := run(context.Background(), args, &out, &diagnostics); e != nil || calls != 1 || !strings.Contains(out.String(), "job-a") || strings.Contains(out.String(), token) {
				t.Fatal(e, calls, out.String())
			}
		})
	}
}
func TestOperationalInvalidArgumentsNeverReachTransport(t *testing.T) {
	for _, args := range [][]string{
		{"job", "show", "../private"}, {"node", "drain", "node?other"}, {"job", "run", "unknown"}, {"job", "ls", "--limit", "0"},
		{"node", "ls", "--limit", "1001"}, {"job", "run", "gc", "--policy-revision", "9007199254740992"}, {"job", "cancel"},
		{"node", "show", "node-a"}, {"stats", "--bucket", "a..b"}, {"stats", "demo"}, {"layout", "demo"}, {"layout", "demo", ""},
		{"layout", "demo", strings.Repeat("k", 1025)}, {"layout", "demo", "\xff"}, {"layout", "A_B", "key"}, {"layout", "demo", "a", "b"},
		{"pack"}, {"pack", "show", "../x"}, {"pack", "show", "."}, {"pack", "restore", "pack-a"}, {"pack", "promote", "pack-a", "g/1"},
		{"pack", "promote", "pack-a", "g", "--days", "1"}, {"pack", "restore", "pack-a", "g", "--days", "31"},
		{"pack", "restore", "pack-a", "g", "--tier", "Fast"}, {"pack", "ls", "--limit", "0"}, {"pack", "rm", "pack-a"},
		// Cache scope must be explicit and coherent before any request.
		{"cache", "stats"}, {"cache", "purge"}, {"cache", "stats", "--scope", "node"}, {"cache", "purge", "--scope", "cluster", "--node", "edge-a"},
		{"cache", "stats", "--scope", "cluster", "--node", ""}, {"cache", "purge", "--scope", "all"}, {"cache", "stats", "--scope", "node", "--node", "../x"},
		{"cache", "flush", "--scope", "cluster"},
	} {
		var out bytes.Buffer
		e := run(context.Background(), args, &out, &bytes.Buffer{})
		if e == nil || e.Error() == "--token-file or DSGCTL_TOKEN_FILE is required" || out.Len() != 0 {
			t.Fatal(args, e)
		}
	}
}

func TestJobRunTypeErrorListsSupportedTypes(t *testing.T) {
	for _, args := range [][]string{{"job", "run"}, {"job", "run", "unknown"}} {
		var out bytes.Buffer
		e := run(context.Background(), args, &out, &bytes.Buffer{})
		if e == nil || !strings.Contains(e.Error(), "supported types:") || out.Len() != 0 {
			t.Fatalf("args %v: error %v, output %q", args, e, out.String())
		}
		for _, kind := range jobs.Types() {
			if !strings.Contains(e.Error(), string(kind)) {
				t.Fatalf("missing supported type %q: %v", kind, e)
			}
		}
	}
}
