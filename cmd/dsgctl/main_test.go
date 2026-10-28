package main

import (
	"bytes"
	"context"
	"errors"
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
	for _, command := range [][]string{{"stats"}, {"policy", "get"}, {"user", "ls"}, {"pack", "ls"}, {"node", "ls"}} {
		var out, diagnostics bytes.Buffer
		args := append([]string{"--endpoint", server.URL, "--token-file", file}, command...)
		if err := run(context.Background(), args, &out, &diagnostics); err != nil || !strings.Contains(out.String(), `"ok":true`) {
			t.Fatal("read command failed", command, err)
		}
	}
	for _, command := range [][]string{{"job", "ls"}, {"job", "run", "scrub"}, {"admin-token", "bootstrap"}} {
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
