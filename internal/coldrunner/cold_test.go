package coldrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
)

func createTestManifest(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	content := []byte("cold-benchmark-payload-bytes")
	h := sha256.Sum256(content)
	hash := hex.EncodeToString(h[:])

	manifest := benchmarkdata.Manifest{
		SchemaVersion: 1,
		Generator:     "test",
		Profile:       "smoke",
		TotalBytes:    int64(len(content)),
		Files: []benchmarkdata.File{
			{
				Path:   "cold-file.bin",
				Bytes:  len(content),
				SHA256: hash,
			},
		},
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifestPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	return manifestPath, string(content), hash
}

func setupFixture(t *testing.T, handler http.HandlerFunc) (benchmark.Config, string) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	temp := t.TempDir()
	credPath := filepath.Join(temp, "credentials.json")
	if err := os.WriteFile(credPath, []byte(`{"access_key_id":"test-id","secret_access_key":"test-secret"}`), 0600); err != nil {
		t.Fatal(err)
	}

	cfg := benchmark.Config{
		Target:          "local-s3-smoke",
		Endpoint:        server.URL,
		Region:          "ap-southeast-1",
		Bucket:          "cold-bucket",
		CredentialsFile: credPath,
		Environment: benchmark.Environment{
			Topology:   "local",
			Host:       "t4g.small",
			CPU:        "2vCPU",
			Disk:       "20G",
			Network:    "loopback",
			Metadata:   "badger",
			Encryption: "none",
			CacheState: "fresh",
			Isolation:  "test",
			AsyncWork:  "test",
		},
	}
	return cfg, temp
}

func TestColdRunnerIR(t *testing.T) {
	manifestPath, content, hash := createTestManifest(t, t.TempDir())

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("x-amz-storage-class", "GLACIER_IR")
			w.Header().Set("x-amz-version-id", "v1-ir")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	cfg, temp := setupFixture(t, handler)
	cfg.StorageClass = "GLACIER_IR"

	opt := Options{
		Manifest:    manifestPath,
		Out:         filepath.Join(temp, "artifact-ir"),
		Variant:     "ir",
		Concurrency: 1,
	}

	res, err := Run(context.Background(), cfg, opt)
	if err != nil {
		t.Fatalf("Run IR failed: %v", err)
	}
	if res.Status != "passed" {
		t.Fatalf("expected passed, got %s", res.Status)
	}
	if res.ColdReceipt == nil || !res.ColdReceipt.RevivalVerified || !res.ColdReceipt.TotalGETVerifiedSHA256 {
		t.Fatalf("invalid ColdReceipt: %+v", res.ColdReceipt)
	}
	if res.ColdReceipt.ObservedBackendStorageClass != "GLACIER_IR" {
		t.Fatalf("expected observed class GLACIER_IR, got %s", res.ColdReceipt.ObservedBackendStorageClass)
	}
	if len(res.Samples) != 1 || res.Samples[0].SHA256 != hash {
		t.Fatalf("unexpected samples: %+v", res.Samples)
	}

	// Verify result.json was persisted
	resultPath := filepath.Join(opt.Out, "result.json")
	if _, err := os.Stat(resultPath); err != nil {
		t.Fatalf("missing result.json: %v", err)
	}
}

func TestColdRunnerFlexible(t *testing.T) {
	manifestPath, content, hash := createTestManifest(t, t.TempDir())

	var restoreState atomic.Int32 // 0: archived, 1: restoring, 2: restored

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := restoreState.Load()

		if r.URL.Query().Has("restore") && r.Method == http.MethodPost {
			restoreState.Store(1)
			w.WriteHeader(http.StatusAccepted)
			return
		}

		if r.Method == http.MethodHead {
			w.Header().Set("x-amz-storage-class", "GLACIER")
			w.Header().Set("x-amz-version-id", "v1-flex")
			if state == 1 {
				w.Header().Set("x-amz-restore", `ongoing-request="true"`)
			} else if state >= 2 {
				w.Header().Set("x-amz-restore", `ongoing-request="false", expiry-date="Fri, 23 Dec 2026 00:00:00 GMT"`)
			}
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method == http.MethodGet {
			if state < 2 {
				if state == 1 {
					restoreState.Store(2)
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`<Error><Code>InvalidObjectState</Code><Message>The operation is not valid for the object's storage class</Message></Error>`))
				return
			}
			w.Header().Set("x-amz-storage-class", "GLACIER")
			w.Header().Set("x-amz-restore", `ongoing-request="false", expiry-date="Fri, 23 Dec 2026 00:00:00 GMT"`)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	cfg, temp := setupFixture(t, handler)
	cfg.StorageClass = "GLACIER"

	opt := Options{
		Manifest:     manifestPath,
		Out:          filepath.Join(temp, "artifact-flexible"),
		Variant:      "flexible",
		Concurrency:  1,
		PollInterval: 10 * time.Millisecond,
		MaxPollWait:  2 * time.Second,
	}

	res, err := Run(context.Background(), cfg, opt)
	if err != nil {
		t.Fatalf("Run Flexible failed: %v", err)
	}
	if res.Status != "passed" {
		t.Fatalf("expected passed, got %s", res.Status)
	}
	if res.ColdReceipt == nil || !res.ColdReceipt.RevivalVerified || res.ColdReceipt.RestorationStatus != "completed" {
		t.Fatalf("invalid ColdReceipt: %+v", res.ColdReceipt)
	}
	if res.ColdReceipt.RestorationExpiry != "Fri, 23 Dec 2026 00:00:00 GMT" {
		t.Fatalf("expected expiry, got %s", res.ColdReceipt.RestorationExpiry)
	}
	if len(res.Samples) != 1 || res.Samples[0].SHA256 != hash {
		t.Fatalf("unexpected samples: %+v", res.Samples)
	}
}

func TestColdRunnerNegativeCases(t *testing.T) {
	manifestPath, _, _ := createTestManifest(t, t.TempDir())

	t.Run("invalid variant", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		opt := Options{Manifest: manifestPath, Out: filepath.Join(temp, "bad-var"), Variant: "unknown"}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error for unknown variant")
		}
	})

	t.Run("storage class mismatch", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		cfg.StorageClass = "STANDARD"
		opt := Options{Manifest: manifestPath, Out: filepath.Join(temp, "bad-class"), Variant: "ir"}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error for storage class mismatch")
		}
	})

	t.Run("flexible readable before restore fails closed", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("unexpected-immediate-payload"))
		})
		cfg, temp := setupFixture(t, handler)
		cfg.StorageClass = "GLACIER"
		opt := Options{Manifest: manifestPath, Out: filepath.Join(temp, "bad-read"), Variant: "flexible"}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error when flexible is readable before restore")
		}
	})

	t.Run("corrupted payload SHA fails closed", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("x-amz-storage-class", "GLACIER_IR")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("corrupted-bytes-different-hash"))
		})
		cfg, temp := setupFixture(t, handler)
		cfg.StorageClass = "GLACIER_IR"
		opt := Options{Manifest: manifestPath, Out: filepath.Join(temp, "corrupted"), Variant: "ir"}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected SHA mismatch error")
		}
	})
}
