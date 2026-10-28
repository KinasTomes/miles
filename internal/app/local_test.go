package app

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestLocalPersistenceAndBackendProbe(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "HEAD" || r.URL.Path != "/packs" {
			t.Errorf("unexpected S3 probe: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer backend.Close()
	c := config.DefaultStartup()
	c.Mode = "local"
	c.LocalDataPath = filepath.Join(t.TempDir(), "local")
	c.PackEndpoint = backend.URL
	c.PackBucket = "packs"
	first, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := filepath.Join(c.LocalDataPath, "credentials.json")
	original, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	cred := first.DevCredentials[0]
	ref := storage.PackRef{ID: "local-probe", Generation: "1"}
	if err := first.Packs.DeletePack(context.Background(), ref); !errors.Is(err, coordination.ErrMaintenanceDisabled) {
		t.Fatal("local wiring exposed destructive delete", err)
	}
	if err := first.Packs.ChangeStorageClass(context.Background(), ref, storage.Glacier); !errors.Is(err, coordination.ErrMaintenanceDisabled) {
		t.Fatal("local wiring exposed destructive tiering", err)
	}
	if err := first.Probe(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, created, err := first.Metadata.CreateBucket(context.Background(), storage.BucketCreate{Name: "persistent-bucket", CredentialID: cred.AccessKeyID, UserID: DevUser})
	if err != nil || !created {
		t.Fatalf("create: %v %v", created, err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if second.DevCredentials[0] != cred {
		t.Fatal("restart rotated credential")
	}
	got, err := second.Metadata.GetBucket(context.Background(), b.Name)
	if err != nil || got.ID != b.ID || got.OwnerID != DevUser {
		t.Fatalf("persisted bucket lost: %v", err)
	}
	backend.Close()
	if second.Probe(context.Background()) == nil {
		t.Fatal("backend loss hidden")
	}
	// Deleting/changing the local credential must never reseed the existing database.
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(credentialPath); err != nil {
		t.Fatal(err)
	}
	if d, err := Bootstrap(c); err == nil {
		d.Close()
		t.Fatal("missing credential silently reseeded existing metadata")
	}
	if _, err := os.Lstat(credentialPath); !os.IsNotExist(err) {
		t.Fatal("failed startup must not create a mismatched credential file")
	}
	if err := os.WriteFile(credentialPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	recovered, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	got, err = recovered.Metadata.GetBucket(context.Background(), b.Name)
	if err != nil || got.ID != b.ID || recovered.DevCredentials[0] != cred {
		t.Fatal("restoring original credential must recover the same installation")
	}
}

func TestLocalBootstrapReusesPreparedCredential(t *testing.T) {
	c := config.DefaultStartup()
	c.Mode, c.LocalDataPath, c.PackEndpoint, c.PackBucket = "local", filepath.Join(t.TempDir(), "local"), "http://127.0.0.1:1", "packs"
	if err := os.Mkdir(c.LocalDataPath, 0700); err != nil {
		t.Fatal(err)
	}
	// Model restart after secret persistence but before the metadata transaction.
	prepared, err := localCredential(filepath.Join(c.LocalDataPath, "credentials.json"), true)
	if err != nil {
		t.Fatal(err)
	}
	d, err := Bootstrap(c)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.DevCredentials[0] != prepared {
		t.Fatal("restart must reuse the durably prepared secret")
	}
	stored, err := d.Metadata.GetCredential(context.Background(), prepared.AccessKeyID)
	if err != nil || !stored.Active || stored.UserID != prepared.UserID {
		t.Fatal("prepared credential was not provisioned")
	}
}

func TestBenchmarkCredentialsRequirePrivateTemporarySession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	for _, data := range []string{`{}`, `{"access_key_id":"key","secret_access_key":"secret"}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := benchmarkCredential(path); err == nil {
			t.Fatal("accepted incomplete or persistent credential")
		}
	}
	if err := os.WriteFile(path, []byte(`{"access_key_id":"key","secret_access_key":"secret","session_token":"temporary"}`), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := benchmarkCredential(path)
	if err != nil || c.SessionToken != "temporary" {
		t.Fatal("temporary session rejected", err)
	}
	os.Chmod(path, 0644)
	if _, err := benchmarkCredential(path); err == nil {
		t.Fatal("shared credential file accepted")
	}
}
