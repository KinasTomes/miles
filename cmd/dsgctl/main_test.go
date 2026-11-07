package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/adminclient"
	"github.com/KinasTomes/miles/internal/api/admin"
)

func TestCLICommandsAndErrors(t *testing.T) {
	_, _, token, err := admin.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte(token), 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("missing bearer")
		}
		if r.URL.Path == "/admin/v1/jobs" {
			w.WriteHeader(501)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	for _, command := range [][]string{{"stats"}, {"cost", "report"}, {"policy", "get"}, {"user", "ls"}, {"pack", "ls"}, {"node", "ls"}} {
		var out, diagnostics bytes.Buffer
		args := append([]string{"--endpoint", server.URL, "--token-file", file}, command...)
		if err := run(context.Background(), args, &out, &diagnostics); err != nil || !strings.Contains(out.String(), `"ok":true`) {
			t.Fatal("read command failed", command, err)
		}
	}
	for _, command := range [][]string{{"job", "ls"}, {"job", "run", "scrub"}} {
		var out, diagnostics bytes.Buffer
		args := append([]string{"--endpoint", server.URL, "--token-file", file}, command...)
		if err := run(context.Background(), args, &out, &diagnostics); !errors.Is(err, adminclient.ErrUnsupported) || out.Len() != 0 {
			t.Fatal("unsupported command claimed success", command, err)
		}
	}
	var out, diagnostics bytes.Buffer
	if err := run(context.Background(), []string{"--timeout", token, "stats"}, &out, &diagnostics); err == nil || strings.Contains(diagnostics.String(), token) {
		t.Fatal("invalid flags leaked secret")
	}
	if err := run(context.Background(), []string{"--version"}, &out, &diagnostics); err != nil {
		t.Fatal(err)
	}
}

func TestCLIRequiresTokenOnlyForSupportedReadCommands(t *testing.T) {
	t.Setenv("DSGCTL_TOKEN_FILE", "")
	for _, args := range [][]string{{"stats"}, {"cost", "report"}, {"policy", "get"}, {"node", "ls"}, {"job", "run", "scrub"}, {"job", "cancel", "job-fixture"}, {"node", "drain", "node-a"}} {
		var out, diagnostics bytes.Buffer
		err := run(context.Background(), args, &out, &diagnostics)
		if err == nil || err.Error() != "--token-file or DSGCTL_TOKEN_FILE is required" || out.Len() != 0 {
			t.Fatalf("missing required flag was not explained for %v: %v", args, err)
		}
	}
	for _, args := range [][]string{{"--version"}, {"--help"}} {
		if err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); err != nil {
			t.Fatal("help/version must not require a token", err)
		}
	}
	for _, args := range [][]string{{"fault", "ls"}, {"rebuild-index"}} {
		if err := run(context.Background(), args, &bytes.Buffer{}, &bytes.Buffer{}); !errors.Is(err, adminclient.ErrUnsupported) {
			t.Fatal("unsupported commands must remain explicit", args, err)
		}
	}
}

// TestEnvironmentSelectsTunnelEndpointAndTokenFile: an SSH tunnel origin and
// token path come from the environment; explicit flags win.
func TestEnvironmentSelectsTunnelEndpointAndTokenFile(t *testing.T) {
	file, token := bearerFile(t)
	hits := map[string]int{}
	handler := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("missing bearer")
			}
			hits[name]++
			w.Write([]byte(`{}`))
		}))
	}
	tunnel, direct := handler("tunnel"), handler("direct")
	defer tunnel.Close()
	defer direct.Close()
	t.Setenv("DSGCTL_ENDPOINT", tunnel.URL)
	t.Setenv("DSGCTL_TOKEN_FILE", file)
	if err := run(context.Background(), []string{"stats"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err := run(context.Background(), []string{"--endpoint", direct.URL, "stats"}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if hits["tunnel"] != 1 || hits["direct"] != 1 {
		t.Fatalf("endpoint precedence: %v", hits)
	}
	t.Setenv("DSGCTL_ENDPOINT", "http://10.0.0.5:9000")
	if err := run(context.Background(), []string{"stats"}, io.Discard, io.Discard); err == nil {
		t.Fatal("plaintext non-loopback endpoint from the environment accepted")
	}
	t.Setenv("DSGCTL_TOKEN_FILE", file+"-missing")
	if err := run(context.Background(), []string{"--endpoint", direct.URL, "stats"}, io.Discard, io.Discard); err == nil || hits["direct"] != 1 {
		t.Fatal("missing token file from the environment reached the gateway", err)
	}
}
