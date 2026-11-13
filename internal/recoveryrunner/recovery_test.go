package recoveryrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
)

func createTestManifest(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	content := []byte("recovery-benchmark-test-payload")
	h := sha256.Sum256(content)
	hash := hex.EncodeToString(h[:])

	manifest := benchmarkdata.Manifest{
		SchemaVersion: 1,
		Generator:     "test",
		Profile:       "smoke",
		TotalBytes:    int64(len(content)),
		Files: []benchmarkdata.File{
			{
				Path:   "recovered-file.bin",
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
		Bucket:          "recovery-bucket",
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

func TestRecoveryRunnerR1(t *testing.T) {
	manifestPath, content, hash := createTestManifest(t, t.TempDir())

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	cfg, temp := setupFixture(t, handler)

	var currentEpoch atomic.Int64
	currentEpoch.Store(10)

	opt := Options{
		Manifest:    manifestPath,
		Out:         filepath.Join(temp, "artifact-r1"),
		Variant:     "r1",
		FaultScope:  "node-restart",
		Concurrency: 1,
		CheckGate: func(ctx context.Context) (*GateState, error) {
			return &GateState{
				HistoryID: "hist-r1-test",
				Epoch:     currentEpoch.Load(),
				State:     "Open",
			}, nil
		},
		SimulateFault: func(ctx context.Context, phase string) error {
			currentEpoch.Add(1) // epoch advances
			return nil
		},
	}

	res, err := Run(context.Background(), cfg, opt)
	if err != nil {
		t.Fatalf("Run R1 failed: %v", err)
	}
	if res.Status != "passed" {
		t.Fatalf("expected passed, got %s", res.Status)
	}
	if res.RecoveryReceipt == nil {
		t.Fatal("missing RecoveryReceipt")
	}
	r := res.RecoveryReceipt
	if !r.FaultScopeApproved || r.BaselineEpoch != 10 || r.RecoveredEpoch != 11 {
		t.Fatalf("invalid receipt epochs: %+v", r)
	}
	if !r.LeaseFenceVerified || !r.SealedCutVerified || !r.ReadbackVerifiedSHA256 {
		t.Fatalf("unverified recovery gates: %+v", r)
	}
	if len(res.Samples) != 1 || res.Samples[0].SHA256 != hash {
		t.Fatalf("unexpected samples: %+v", res.Samples)
	}
}

func TestRecoveryRunnerR2(t *testing.T) {
	manifestPath, content, hash := createTestManifest(t, t.TempDir())

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(content))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})

	cfg, temp := setupFixture(t, handler)

	opt := Options{
		Manifest:    manifestPath,
		Out:         filepath.Join(temp, "artifact-r2"),
		Variant:     "r2",
		FaultScope:  "checkpoint-cut",
		Concurrency: 1,
	}

	res, err := Run(context.Background(), cfg, opt)
	if err != nil {
		t.Fatalf("Run R2 failed: %v", err)
	}
	if res.Status != "passed" {
		t.Fatalf("expected passed, got %s", res.Status)
	}
	if res.RecoveryReceipt == nil || !res.RecoveryReceipt.ReplayIdempotencyVerified {
		t.Fatalf("invalid R2 receipt: %+v", res.RecoveryReceipt)
	}
	if len(res.Samples) != 1 || res.Samples[0].SHA256 != hash {
		t.Fatalf("unexpected samples: %+v", res.Samples)
	}
}

func TestRecoveryRunnerNegativeCases(t *testing.T) {
	manifestPath, _, _ := createTestManifest(t, t.TempDir())

	t.Run("unapproved fault scope", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "bad-scope"),
			Variant:    "r1",
			FaultScope: "unapproved-random-kill",
		}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error for unapproved fault scope")
		}
	})

	t.Run("baseline gate closed", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "bad-gate"),
			Variant:    "r1",
			FaultScope: "node-restart",
			CheckGate: func(ctx context.Context) (*GateState, error) {
				return &GateState{HistoryID: "h1", Epoch: 1, State: "Closed"}, nil
			},
		}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error when baseline gate is Closed")
		}
	})

	t.Run("epoch failed to advance", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "stale-epoch"),
			Variant:    "r1",
			FaultScope: "node-restart",
			CheckGate: func(ctx context.Context) (*GateState, error) {
				return &GateState{HistoryID: "h1", Epoch: 5, State: "Open"}, nil
			},
			SimulateFault: func(ctx context.Context, phase string) error {
				return nil // does not advance epoch
			},
		}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error when epoch fails to advance")
		}
	})

	t.Run("readback SHA mismatch fails closed", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("corrupted-recovered-bytes"))
		})
		cfg, temp := setupFixture(t, handler)
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "bad-sha"),
			Variant:    "r1",
			FaultScope: "node-restart",
		}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error on readback SHA mismatch")
		}
	})

	t.Run("fault injection error fails closed", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "fault-err"),
			Variant:    "r1",
			FaultScope: "node-restart",
			SimulateFault: func(ctx context.Context, phase string) error {
				return errors.New("simulated injection failure")
			},
		}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error when fault injection fails")
		}
	})
}
