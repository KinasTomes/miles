package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/KinasTomes/miles/internal/config"
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
	cred := first.DevCredentials[0]
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
	if err := os.Remove(filepath.Join(c.LocalDataPath, "credentials.json")); err != nil {
		t.Fatal(err)
	}
	if d, err := Bootstrap(c); err == nil {
		d.Close()
		t.Fatal("missing credential silently reseeded existing metadata")
	}
}
