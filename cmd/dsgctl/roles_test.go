package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/adminclient"
	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/jobs"
	opstest "github.com/KinasTomes/miles/internal/jobs/contracttest"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/stats"
	"github.com/KinasTomes/miles/internal/storage"
)

const rolesBucketID = "bkt-roles-0001"

type emptyInventory struct{}

func (emptyInventory) CollectInventory(context.Context, int) (storage.PhysicalInventory, error) {
	return storage.PhysicalInventory{Complete: true, VersionScope: "all-retained"}, nil
}

// rolesGateway is the real Admin server over Badger, with a bucket of owner,
// a user without rights, one pack and one node. The system token comes from
// the offline bootstrap command; owner and stranger tokens from the CLI.
func rolesGateway(t *testing.T) (endpoint string, tokens map[string]string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC()
	store, err := badger.Open(badger.Options{Path: filepath.Join(dir, "metadata"), RequireProducers: true})
	if err != nil {
		t.Fatal(err)
	}
	seed := badger.Bootstrap{
		Buckets: []storage.Bucket{{Name: "demo", ID: rolesBucketID, OwnerID: "owner", Domain: "default", Revision: 1, Active: true, CreatedAt: now}},
		Packs:   []storage.PackInfo{{Version: 1, Ref: storage.PackRef{ID: "pack-a", Generation: "generation-1"}, Size: 100, State: "Cold", Class: storage.GlacierIR, StateRevision: 1, ProtectedUntil: now, MinRetentionUntil: now}},
	}
	for _, id := range []string{"owner", "stranger"} {
		seed.Users = append(seed.Users, storage.User{ID: id, Active: true, Revision: 1, DefaultDomain: "default"})
		seed.Quotas = append(seed.Quotas, storage.LogicalQuota{OwnerID: id, Limit: 1 << 40})
	}
	if err = store.Bootstrap(ctx, seed); err != nil {
		t.Fatal(err)
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	tokens = map[string]string{"admin": filepath.Join(dir, "admin-token")}
	if err = run(ctx, []string{"admin-token", "bootstrap", "--user", "ops", "--token-file", tokens["admin"], "--local-data-path", dir}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if store, err = badger.Open(badger.Options{Path: filepath.Join(dir, "metadata"), RequireProducers: true}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	authority, err := store.StartLocalRuntime(ctx, "ops", 1<<40)
	if err != nil {
		t.Fatal(err)
	}
	clk := clock.NewReal()
	node := opstest.Node("node-a")
	node.HeartbeatAt = now
	if _, err = store.PutNode(ctx, 0, node); err != nil {
		t.Fatal(err)
	}
	cluster := &jobs.Cluster{Runner: &jobs.Runner{Service: jobs.Service{Store: store, Policies: store, Now: time.Now}}, Registry: &jobs.Registry{Store: store, Clock: clk, Identity: node}}
	cluster.Runner.Executors = map[jobs.Type]jobs.Executor{"cache_purge": cluster.CacheExecutor(nil)}
	statistics, err := stats.New(stats.Options{Store: store, Inventory: emptyInventory{}, Now: time.Now, MaxRecords: 100, MaxReferences: 100, MaxPhysicalObjects: 100, Timeout: time.Minute, MaxAge: time.Hour, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err = statistics.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	sealer, err := admin.NewAESSealer("test", bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	handler, err := admin.NewServer(admin.Options{
		Store: store,
		Admit: func(ctx context.Context, tokenID string) (storage.MutationContext, error) {
			var op [16]byte
			rand.Read(op[:])
			return authority.AdmitAdmin(ctx, tokenID, hex.EncodeToString(op[:]), time.Now().Add(30*time.Second))
		},
		Sealer:          sealer,
		Cursors:         admin.Keyring{Current: "k1", Keys: map[string][]byte{"k1": bytes.Repeat([]byte{1}, 32)}},
		Clock:           clk,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Statistics:      statistics,
		Operations:      cluster,
		CacheStatistics: cluster,
		PackInventory:   &lifecycle.Catalog{Store: store, Now: time.Now, MaxRecords: 100, MaxReferences: 100},
	})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	for _, user := range []string{"owner", "stranger"} {
		tokens[user] = filepath.Join(dir, user+"-token")
		var stdout bytes.Buffer
		if err = run(ctx, []string{"--endpoint", server.URL, "--token-file", tokens["admin"], "admin-token", "create", "--user", user, "--ttl", "1h", "--out", tokens[user]}, &stdout, io.Discard); err != nil {
			t.Fatal(err)
		}
		secret, _ := os.ReadFile(tokens[user])
		info, _ := os.Stat(tokens[user])
		if len(secret) == 0 || bytes.Contains(stdout.Bytes(), bytes.TrimSpace(secret)) || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
			t.Fatal("minted token not private", stdout.String())
		}
	}
	return server.URL, tokens
}

// outcome is "ok", or the HTTP status and code of the Admin error.
func outcome(err error) string {
	var e *adminclient.APIError
	switch {
	case err == nil:
		return "ok"
	case errors.As(err, &e):
		return strings.TrimSpace(strconv.Itoa(e.Status) + " " + e.Code)
	}
	return err.Error()
}

// TestCLIRolesAgainstAdminServer: every wired command reaches its real
// handler, and the token's scope decides the outcome. System-admin commands
// deny owner and stranger; owner commands deny the stranger.
func TestCLIRolesAgainstAdminServer(t *testing.T) {
	endpoint, tokens := rolesGateway(t)
	do := func(role string, args ...string) (string, string) {
		t.Helper()
		var stdout bytes.Buffer
		err := run(context.Background(), append([]string{"--endpoint", endpoint, "--token-file", tokens[role]}, args...), &stdout, io.Discard)
		if err != nil && stdout.Len() != 0 {
			t.Fatalf("%s %v: output on failure", role, args)
		}
		return outcome(err), stdout.String()
	}
	const denied = "403 forbidden"
	for _, row := range []struct {
		args                   []string
		admin, owner, stranger string
	}{
		{[]string{"stats"}, "ok", denied, denied},
		{[]string{"stats", "--bucket", "demo"}, "ok", denied, denied},
		{[]string{"layout", "demo", "missing key"}, "404 object_not_found", denied, denied},
		{[]string{"pack", "ls", "--storage-class", "GLACIER_IR"}, "ok", denied, denied},
		{[]string{"pack", "show", "pack-a"}, "ok", denied, denied},
		{[]string{"pack", "restore", "pack-a", "generation-1", "--days", "2"}, "501 job_type_unavailable", denied, denied},
		{[]string{"pack", "promote", "pack-a", "generation-9"}, "404 pack_not_found", denied, denied},
		{[]string{"cache", "stats", "--scope", "cluster"}, "ok", denied, denied},
		{[]string{"cache", "stats", "--scope", "node", "--node", "node-a"}, "ok", denied, denied},
		{[]string{"cache", "purge", "--scope", "node", "--node", "node-a"}, "ok", denied, denied},
		{[]string{"job", "ls", "--limit", "1"}, "ok", denied, denied},
		{[]string{"node", "ls"}, "ok", denied, denied},
		{[]string{"user", "ls"}, "ok", denied, denied},
		{[]string{"policy", "get"}, "ok", denied, denied},
		{[]string{"credentials", "create", "--user", "owner", "--out", "unused"}, "", denied, denied},
		{[]string{"usage", "owner"}, "ok", "ok", denied},
		{[]string{"bucket", "show", "demo"}, "ok", "ok", denied},
		{[]string{"grant", "ls", "demo"}, "ok", "ok", denied},
		{[]string{"bucket-policy", "get", "demo"}, "ok", "ok", denied},
	} {
		for role, want := range map[string]string{"admin": row.admin, "owner": row.owner, "stranger": row.stranger} {
			if want == "" {
				continue
			}
			args := row.args
			if args[0] == "credentials" {
				args = append(append([]string{}, args[:len(args)-1]...), filepath.Join(t.TempDir(), "secret"))
			}
			if got, _ := do(role, args...); got != want {
				t.Errorf("%s %v: got %q, want %q", role, row.args, got, want)
			}
			if args[0] == "credentials" {
				if _, err := os.Lstat(args[len(args)-1]); !os.IsNotExist(err) {
					t.Errorf("%s: denied create left a secret file", role)
				}
			}
		}
	}

	// Owner manages grants with the bucket_id it read; a stale id or
	// revision is refused and a grant gives no Admin scope.
	set := func(role, bucketID, revision string) string {
		got, _ := do(role, "grant", "set", "demo", "stranger", "--read=true", "--expected-revision", revision, "--expected-bucket-id", bucketID)
		return got
	}
	if got := set("stranger", rolesBucketID, "0"); got != denied {
		t.Fatal("stranger granted itself", got)
	}
	if got := set("owner", "bkt-stale-0001", "0"); got != "409 bucket_id_mismatch" {
		t.Fatal(got)
	}
	if got, out := do("owner", "grant", "set", "demo", "stranger", "--read=true", "--expected-revision", "0", "--expected-bucket-id", rolesBucketID); got != "ok" || !strings.Contains(out, `"revision":1`) {
		t.Fatal(got, out)
	}
	if got := set("owner", rolesBucketID, "0"); got != "409 revision_conflict" {
		t.Fatal(got)
	}
	if got, _ := do("stranger", "grant", "ls", "demo"); got != denied {
		t.Fatal("read grant became bucket administration", got)
	}
	// Owner cannot raise its own scope.
	if got, _ := do("owner", "user", "set", "owner", "--status", "active", "--expected-revision", "1"); got != denied {
		t.Fatal(got)
	}
	out := filepath.Join(t.TempDir(), "elevated")
	if got, _ := do("owner", "admin-token", "create", "--user", "owner", "--system-admin", "--out", out); got != denied {
		t.Fatal(got)
	}
	if _, err := os.Lstat(out); !os.IsNotExist(err) {
		t.Fatal("denied token create left a file")
	}
	if got, _ := do("owner", "grant", "rm", "demo", "stranger", "--expected-bucket-id", rolesBucketID); got != "ok" {
		t.Fatal(got)
	}
}
