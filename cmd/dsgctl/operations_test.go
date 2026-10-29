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
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args[:2], "-"), func(t *testing.T) {
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
	for _, args := range [][]string{{"job", "show", "../private"}, {"node", "drain", "node?other"}, {"job", "run", "unknown"}, {"job", "ls", "--limit", "0"}, {"node", "ls", "--limit", "1001"}, {"job", "run", "gc", "--policy-revision", "9007199254740992"}, {"job", "cancel"}} {
		var out bytes.Buffer
		e := run(context.Background(), args, &out, &bytes.Buffer{})
		if e == nil || e.Error() == "--token-file is required" || out.Len() != 0 {
			t.Fatal(args, e)
		}
	}
}
