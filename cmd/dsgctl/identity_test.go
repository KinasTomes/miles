package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
)

func bearerFile(t *testing.T) (string, string) {
	t.Helper()
	_, _, token, err := admin.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(file, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	return file, token
}

func TestIdentityCommandsWireSchema(t *testing.T) {
	file, token := bearerFile(t)
	patch := filepath.Join(t.TempDir(), "patch.json")
	if err := os.WriteFile(patch, []byte(`{"tiering":{"enabled":false}}`), 0600); err != nil {
		t.Fatal(err)
	}
	const id = "0123456789abcdef0123456789abcdef"
	tests := []struct {
		args         []string
		method, path string
		body         string
	}{
		{[]string{"user", "create", "alice", "--logical-limit", "1024"}, "POST", "/admin/v1/users", `{"logical_limit":1024,"user_id":"alice"}`},
		{[]string{"user", "create", "alice"}, "POST", "/admin/v1/users", `{"user_id":"alice"}`},
		{[]string{"user", "ls", "--limit", "5", "--cursor", "k.p.t"}, "GET", "/admin/v1/users?cursor=k.p.t&limit=5", ""},
		{[]string{"user", "ls"}, "GET", "/admin/v1/users", ""},
		{[]string{"user", "show", "alice"}, "GET", "/admin/v1/users/alice", ""},
		{[]string{"user", "set", "alice", "--status", "disabled", "--default-domain", "team-a", "--expected-revision", "3"}, "PATCH", "/admin/v1/users/alice", `{"default_domain":"team-a","expected_revision":3,"status":"disabled"}`},
		{[]string{"usage", "alice", "--detail"}, "GET", "/admin/v1/users/alice/usage?detail=true", ""},
		{[]string{"bucket", "show", "demo.bucket"}, "GET", "/admin/v1/buckets/demo.bucket", ""},
		{[]string{"grant", "ls", "demo"}, "GET", "/admin/v1/buckets/demo/grants", ""},
		{[]string{"grant", "set", "demo", "bob", "--write=true", "--expected-revision", "0", "--expected-bucket-id", "b-1"}, "PUT", "/admin/v1/buckets/demo/grants/bob", `{"expected_bucket_id":"b-1","expected_revision":0,"write":true}`},
		{[]string{"grant", "rm", "demo", "bob", "--expected-bucket-id", "b-1"}, "DELETE", "/admin/v1/buckets/demo/grants/bob?expected_bucket_id=b-1", ""},
		{[]string{"policy", "get"}, "GET", "/admin/v1/policy", ""},
		{[]string{"policy", "set", "--file", patch, "--expected-revision", "2"}, "PUT", "/admin/v1/policy", `{"expected_revision":2,"tiering":{"enabled":false}}`},
		{[]string{"policy", "set", "--file", patch}, "PUT", "/admin/v1/policy", `{"tiering":{"enabled":false}}`},
		{[]string{"bucket-policy", "get", "demo"}, "GET", "/admin/v1/buckets/demo/policy", ""},
		{[]string{"bucket-policy", "set", "demo", "--readahead-chunks", "4", "--expected-revision", "1", "--expected-bucket-id", "b-1"}, "PUT", "/admin/v1/buckets/demo/policy", `{"cache":{"readahead_chunks":4},"expected_bucket_id":"b-1","expected_revision":1}`},
		{[]string{"credentials", "ls", "--user", "alice"}, "GET", "/admin/v1/credentials?user_id=alice", ""},
		{[]string{"credentials", "rm", "AKIAABCDEFGHIJKLMNOP"}, "DELETE", "/admin/v1/credentials/AKIAABCDEFGHIJKLMNOP", ""},
		{[]string{"admin-token", "ls"}, "GET", "/admin/v1/admin-tokens", ""},
		{[]string{"admin-token", "revoke", id}, "DELETE", "/admin/v1/admin-tokens/" + id, ""},
	}
	for _, test := range tests {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				body, _ := io.ReadAll(r.Body)
				if r.Method != test.method || r.URL.RequestURI() != test.path || r.Header.Get("Authorization") != "Bearer "+token || string(body) != test.body {
					t.Errorf("wrong request %s %s %s", r.Method, r.URL.RequestURI(), body)
				}
				if r.Method == http.MethodDelete {
					w.WriteHeader(http.StatusNoContent)
					return
				}
				w.Write([]byte(`{"ok":true}`))
			}))
			defer server.Close()
			var out bytes.Buffer
			args := append([]string{"--endpoint", server.URL, "--token-file", file}, test.args...)
			if err := run(context.Background(), args, &out, io.Discard); err != nil || calls != 1 {
				t.Fatal(err, calls)
			}
		})
	}
}

func TestIdentityInvalidArgumentsNeverReachTransport(t *testing.T) {
	for _, args := range [][]string{
		{"user", "show", "Alice"}, {"user", "show", "../x"}, {"user", "set", "alice", "--status", "active"},
		{"user", "set", "alice", "--expected-revision", "1"}, {"user", "set", "alice", "--status", "gone", "--expected-revision", "1"},
		{"usage"}, {"bucket", "show", "a"}, {"bucket", "show", "a..b"}, {"grant", "set", "demo", "bob", "--read"},
		{"grant", "set", "demo", "bob", "--read", "--expected-revision", "0"}, {"grant", "rm", "demo", "bob"},
		{"grant", "rm", "demo", "bob", "--expected-bucket-id", "B-1"},
		{"bucket-policy", "set", "demo", "--expected-revision", "0", "--expected-bucket-id", "b"},
		{"bucket-policy", "set", "demo", "--readahead-chunks", "65", "--expected-revision", "1", "--expected-bucket-id", "b"},
		{"policy", "set"}, {"credentials", "rm", "akiaabcdefghijklmnop"}, {"credentials", "create", "--user", "a"},
		{"credentials", "create", "--user", "a", "--out", "x", "--debug-allowed"}, {"admin-token", "revoke", "XYZ"},
		{"admin-token", "create", "--user", "a", "--out", "x", "--ttl", "1m"}, {"user", "ls", "--limit", "0"},
	} {
		var out bytes.Buffer
		err := run(context.Background(), args, &out, io.Discard)
		if err == nil || err.Error() == "--token-file is required" || out.Len() != 0 {
			t.Fatal(args, err)
		}
	}
}

func TestCreateWritesSecretOnlyToNewPrivateFile(t *testing.T) {
	file, _ := bearerFile(t)
	const secret = "c2VjcmV0LXNlY3JldC1zZWNyZXQtc2VjcmV0LXNlYw=="
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(b))
		if r.URL.Path == "/admin/v1/admin-tokens" {
			w.WriteHeader(http.StatusConflict)
			w.Write([]byte(`{"error":{"code":"user_not_found","message":"m","request_id":"r"}}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"access_key_id":"AKIAABCDEFGHIJKLMNOP","secret_access_key":"` + secret + `","user_id":"alice"}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	out := filepath.Join(dir, "credential")
	base := []string{"--endpoint", server.URL, "--token-file", file}
	var stdout bytes.Buffer
	if err := run(context.Background(), append(base, "credentials", "create", "--user", "alice", "--system-admin", "--debug-allowed", "--out", out), &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), secret) || !strings.Contains(stdout.String(), "[REDACTED]") || !strings.Contains(stdout.String(), "AKIAABCDEFGHIJKLMNOP") {
		t.Fatal("secret leaked to stdout:", stdout.String())
	}
	if bodies[0] != `{"debug_allowed":true,"system_admin":true,"user_id":"alice"}` {
		t.Fatal(bodies[0])
	}
	got, err := os.ReadFile(out)
	info, _ := os.Stat(out)
	if err != nil || string(got) != secret+"\n" || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("secret file", string(got), err)
	}
	if err := run(context.Background(), append(base, "credentials", "create", "--user", "alice", "--out", out), io.Discard, io.Discard); err == nil || len(bodies) != 1 {
		t.Fatal("existing --out must be refused before the request", err)
	}
	tokenOut := filepath.Join(dir, "token")
	err = run(context.Background(), append(base, "admin-token", "create", "--user", "alice", "--ttl", "1h", "--out", tokenOut), io.Discard, io.Discard)
	if err == nil || err.Error() != "Admin request failed (HTTP 409 user_not_found)" || bodies[1] != `{"expires_in_seconds":3600,"system_admin":false,"user_id":"alice"}` {
		t.Fatal(err, bodies)
	}
	if _, err := os.Lstat(tokenOut); !os.IsNotExist(err) {
		t.Fatal("failed create left a secret file")
	}
}

func TestBootstrapBadgerOnceOffline(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "admin-token")
	var stdout bytes.Buffer
	args := []string{"admin-token", "bootstrap", "--user", "ops", "--ttl", "2h", "--token-file", tokenFile, "--local-data-path", filepath.Join(dir, "data")}
	if err := run(context.Background(), args, &stdout, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(tokenFile)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.TrimSpace(string(raw))
	id, secret, ok := admin.ParseBearer([]string{"Bearer " + token})
	if !ok || strings.Contains(stdout.String(), secret) {
		t.Fatal("bad token file or secret printed")
	}
	var printed struct {
		TokenID   string `json:"token_id"`
		ExpiresAt string `json:"expires_at"`
	}
	if json.Unmarshal(stdout.Bytes(), &printed) != nil || printed.TokenID != id || printed.ExpiresAt == "" {
		t.Fatal(stdout.String())
	}
	second := filepath.Join(dir, "second")
	args[7] = second
	if err := run(context.Background(), args, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "already bootstrapped") {
		t.Fatal("second bootstrap must be refused", err)
	}
	if _, err := os.Lstat(second); !os.IsNotExist(err) {
		t.Fatal("refused bootstrap left a token file")
	}
	m, err := badger.Open(badger.Options{Path: filepath.Join(dir, "data", "metadata"), RequireProducers: true})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	stored, err := m.GetAdminToken(context.Background(), id)
	if err != nil || stored.Scope != storage.TokenScopeSystem || stored.UserID != "ops" || !admin.VerifySecret(hex.EncodeToString(stored.Hash), secret) {
		t.Fatal(stored, err)
	}
	if user, err := m.GetUser(context.Background(), "ops"); err != nil || !user.Active {
		t.Fatal(user, err)
	}
}

func TestBootstrapFlagValidation(t *testing.T) {
	base := []string{"--user", "ops", "--token-file", "t"}
	ddb := append(append([]string{}, base...), "--ddb-table-prefix", "prod-", "--region", "ap-southeast-1")
	if o, err := parseBootstrap(append(ddb, "--ddb-endpoint", "http://127.0.0.1:8000")); err != nil || o.prefix != "prod-" || o.dataPath != "" {
		t.Fatal(o, err)
	}
	for _, args := range [][]string{
		base,
		append(append([]string{}, ddb...), "--local-data-path", "d"),
		append(append([]string{}, ddb...), "--ddb-endpoint", "https://dynamodb.amazonaws.com"),
		append(append([]string{}, ddb...), "--ddb-endpoint", "http://10.0.0.1:8000"),
		{"--user", "ops", "--token-file", "t", "--ddb-table-prefix", "p"},
		{"--user", "ops", "--token-file", "t", "--region", "r"},
		{"--user", "ops", "--token-file", "t", "--ddb-table-prefix", "bad prefix", "--region", "r"},
		{"--user", "Ops", "--token-file", "t", "--local-data-path", "d"},
		{"--user", "ops", "--local-data-path", "d"},
		{"--user", "ops", "--token-file", "t", "--local-data-path", "d", "--ttl", "200h"},
	} {
		if _, err := parseBootstrap(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	if _, err := parseBootstrap([]string{"--recover"}); err == nil || !strings.Contains(err.Error(), "R2 recovery gate not implemented") {
		t.Fatal(err)
	}
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	if _, _, err := openBootstrapStore(bootstrapOptions{prefix: "p-", region: "r"}); err == nil || !strings.Contains(err.Error(), "AWS_ACCESS_KEY_ID") {
		t.Fatal("DynamoDB bootstrap must require explicit provisioning credentials", err)
	}
}

type outcomeStore struct {
	storage.IdentityStore
	err error
}

func (s outcomeStore) BootstrapAdmin(context.Context, storage.AdminBootstrap) error { return s.err }

func TestBootstrapKeepsTokenFileWhenOutcomeUnknown(t *testing.T) {
	original := openBootstrapStore
	defer func() { openBootstrapStore = original }()
	for _, test := range []struct {
		err  error
		kept bool
	}{{storage.ErrUnavailable, true}, {context.Canceled, true}, {storage.ErrConflict, false}, {storage.ErrInvalid, false}} {
		openBootstrapStore = func(bootstrapOptions) (storage.IdentityStore, func() error, error) {
			return outcomeStore{err: test.err}, func() error { return nil }, nil
		}
		file := filepath.Join(t.TempDir(), "token")
		err := run(context.Background(), []string{"admin-token", "bootstrap", "--user", "ops", "--token-file", file, "--local-data-path", "unused"}, io.Discard, io.Discard)
		_, statErr := os.Lstat(file)
		if err == nil || (statErr == nil) != test.kept {
			t.Fatal(test.err, err, statErr)
		}
		if test.kept && (!errors.Is(err, test.err) || !strings.Contains(err.Error(), "outcome unknown")) {
			t.Fatal("unknown outcome must say so", err)
		}
	}
}

func TestPolicySetSplicesRevisionWithoutReencoding(t *testing.T) {
	dir := t.TempDir()
	for content, want := range map[string]string{
		` {"tiering":{"enabled":false},"tiering":{"enabled":true}}`: ` {"expected_revision":7,"tiering":{"enabled":false},"tiering":{"enabled":true}}`,
		"{ }": `{"expected_revision":7 }`,
	} {
		file := filepath.Join(dir, "patch.json")
		if err := os.WriteFile(file, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		cmd, err := identityCommand([]string{"policy", "set", "--file", file, "--expected-revision", "7"})
		if err != nil || string(cmd.body) != want {
			t.Fatal(string(cmd.body), err)
		}
	}
}
