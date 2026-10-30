package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/app"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestAdminCreatedCredentialSignsS3(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer backend.Close()
	c := config.DefaultStartup()
	c.Mode = "local"
	c.LocalDataPath = filepath.Join(t.TempDir(), "local")
	c.PackEndpoint = backend.URL
	c.PackBucket = "packs"
	c.AdminAddress = "127.0.0.1:0"
	d, err := app.Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
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
	var logs bytes.Buffer
	handler, err := adminHandler(*d.Admin, &logs)
	if err != nil {
		t.Fatal(err)
	}
	a := new(app.Admission)
	server := httptest.NewServer(handler(a))
	defer server.Close()
	req, _ := http.NewRequest("POST", server.URL+"/admin/v1/credentials", strings.NewReader(`{"user_id":"ops"}`))
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		AccessKeyID string `json:"access_key_id"`
		Secret      string `json:"secret_access_key"`
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 201 || json.Unmarshal(body, &created) != nil || created.Secret == "" {
		t.Fatal(response.StatusCode, string(body))
	}
	got, err := devCredentials{d}.Credential(context.Background(), created.AccessKeyID)
	if err != nil || got.Secret != created.Secret || got.UserID != "ops" || !got.Active {
		t.Fatal("sealed secret lookup", err)
	}
	if strings.Contains(logs.String(), created.Secret) || strings.Contains(logs.String(), secret) {
		t.Fatal("audit log leaked a secret")
	}
	if dev, err := (devCredentials{d}).Credential(context.Background(), d.DevCredentials[0].AccessKeyID); err != nil || dev.Secret != d.DevCredentials[0].Secret {
		t.Fatal("dev credential must still win", err)
	}
	a.Drain()
	drained, err := server.Client().Get(server.URL + "/admin/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(drained.Body)
	drained.Body.Close()
	if drained.StatusCode != 503 || !strings.Contains(string(b), `"code":"unavailable"`) {
		t.Fatal("drained Admin must use the JSON envelope", drained.StatusCode, string(b))
	}
}
