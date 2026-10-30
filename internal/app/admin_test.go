package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

func freeAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func TestLocalAdminListenerAndCredentialKey(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()
	c := config.DefaultStartup()
	c.Mode = "local"
	c.LocalDataPath = filepath.Join(t.TempDir(), "local")
	c.PackEndpoint = backend.URL
	c.PackBucket = "packs"
	c.AdminAddress = freeAddress(t)
	c.ShutdownSeconds = 5
	first, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	if first.Admin == nil || first.Admin.Store == nil || first.Admin.Admit == nil || first.Admin.Sealer == nil || first.Admin.Cursors.Validate() != nil {
		t.Fatal("local Admin options incomplete")
	}
	keyPath := filepath.Join(c.LocalDataPath, "credential-key")
	info, err := os.Stat(keyPath)
	if err != nil || info.Size() != 32 || (runtime.GOOS != "windows" && info.Mode().Perm() != 0600) {
		t.Fatal("credential-key", err)
	}
	sealed, err := first.Admin.Sealer.Seal(context.Background(), "AKIAABCDEFGHIJKLMNOP", []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	if opened, err := d.Admin.Sealer.Open(context.Background(), "AKIAABCDEFGHIJKLMNOP", sealed); err != nil || string(opened) != "secret" {
		t.Fatal("restart must reuse credential-key", err)
	}
	validator, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	global, _ := validator.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	policy, _ := global.StorageSnapshot()
	policy.Revision = 0
	id, secret, token, err := admin.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(secret))
	now := time.Now()
	if err := d.Admin.Store.BootstrapAdmin(context.Background(), storage.AdminBootstrap{Token: storage.AdminToken{ID: id, UserID: "ops", Scope: storage.TokenScopeSystem, Hash: hash[:], Active: true, IssuedAt: now, ExpiresAt: now.Add(time.Hour)}, QuotaLimit: 1 << 40, Policy: policy}); err != nil {
		t.Fatal(err)
	}
	server, err := admin.NewServer(*d.Admin)
	if err != nil {
		t.Fatal(err)
	}
	d.AdminHandler = func(*Admission) http.Handler { return server }
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	var logs bytes.Buffer
	go func() { done <- serve(ctx, ln, c, d, new(Admission), &logs) }()
	client := &http.Client{Timeout: time.Second}
	defer client.CloseIdleConnections()
	call := func(bearer string) int {
		deadline := time.Now().Add(3 * time.Second)
		for {
			req, _ := http.NewRequest("POST", "http://"+c.AdminAddress+"/admin/v1/users", bytes.NewReader([]byte(`{"user_id":"alice"}`)))
			if bearer != "" {
				req.Header.Set("Authorization", "Bearer "+bearer)
			}
			r, err := client.Do(req)
			if err == nil {
				_, _ = io.Copy(io.Discard, r.Body)
				r.Body.Close()
				return r.StatusCode
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if code := call(""); code != 401 {
		t.Fatal("unauthenticated Admin request", code)
	}
	if code := call(token); code != 201 {
		t.Fatal("bootstrapped token create user", code)
	}
	if user, err := d.Admin.Store.GetUser(context.Background(), "alice"); err != nil || !user.Active {
		t.Fatal(user, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err, logs.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not stop")
	}
	if !bytes.Contains(logs.Bytes(), []byte(`"admin server listening"`)) {
		t.Fatal(logs.String())
	}
}

func TestAdminListenerRequiresHandler(t *testing.T) {
	c := config.DefaultStartup()
	c.Mode = "local"
	c.AdminAddress = freeAddress(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := serve(context.Background(), ln, c, Dependencies{}, new(Admission), io.Discard); err != ErrRuntimeUnavailable {
		t.Fatal(err)
	}
}
