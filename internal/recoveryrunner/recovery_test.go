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
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		VerifyNativeProof: func(ctx context.Context, historyID string, baselineEpoch, recoveredEpoch int64) (NativeProof, error) {
			return NativeProof{LeaseFenceVerified: true, SealedCutVerified: true, ReplayIdempotencyVerified: true}, nil
		},
		ReadCheckpoint: func(ctx context.Context, historyID string, epoch int64) (string, error) {
			return "chk-seal-" + historyID, nil
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

	var r2Epoch atomic.Int64
	r2Epoch.Store(20)

	opt := Options{
		Manifest:    manifestPath,
		Out:         filepath.Join(temp, "artifact-r2"),
		Variant:     "r2",
		FaultScope:  "checkpoint-cut",
		Concurrency: 1,
		CheckGate: func(ctx context.Context) (*GateState, error) {
			return &GateState{
				HistoryID: "hist-r2-test",
				Epoch:     r2Epoch.Load(),
				State:     "Open",
			}, nil
		},
		SimulateFault: func(ctx context.Context, phase string) error {
			r2Epoch.Add(1)
			return nil
		},
		VerifyNativeProof: func(ctx context.Context, historyID string, baselineEpoch, recoveredEpoch int64) (NativeProof, error) {
			return NativeProof{LeaseFenceVerified: true, SealedCutVerified: true, ReplayIdempotencyVerified: true}, nil
		},
		ReadCheckpoint: func(ctx context.Context, historyID string, epoch int64) (string, error) {
			return "chk-cut-" + historyID, nil
		},
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
			Manifest:     manifestPath,
			Out:          filepath.Join(temp, "stale-epoch"),
			Variant:      "r1",
			FaultScope:   "node-restart",
			PollInterval: 5 * time.Millisecond,
			MaxPollWait:  50 * time.Millisecond,
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

	t.Run("nil callbacks and unconfigured dynamodb fails closed", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		// Point credentials_file to non-existent file
		cfg.CredentialsFile = filepath.Join(temp, "non-existent-creds.json")
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "no-cb"),
			Variant:    "r1",
			FaultScope: "node-restart",
		}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected fail-closed error when callbacks are nil and dynamodb credentials unavailable")
		}
	})

	t.Run("missing fault mechanism fails closed", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "no-fault"),
			Variant:    "r1",
			FaultScope: "node-restart",
			CheckGate: func(ctx context.Context) (*GateState, error) {
				return &GateState{HistoryID: "h1", Epoch: 1, State: "Open"}, nil
			},
			VerifyNativeProof: func(ctx context.Context, historyID string, baselineEpoch, recoveredEpoch int64) (NativeProof, error) {
				return NativeProof{LeaseFenceVerified: true, SealedCutVerified: true, ReplayIdempotencyVerified: true}, nil
			},
			ReadCheckpoint: func(ctx context.Context, historyID string, epoch int64) (string, error) {
				return "chk-1", nil
			},
		}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error when no fault injection mechanism provided")
		}
	})

	t.Run("checkpoint verification failure fails closed", func(t *testing.T) {
		cfg, temp := setupFixture(t, nil)
		var epoch atomic.Int64
		epoch.Store(1)
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "bad-chk"),
			Variant:    "r1",
			FaultScope: "node-restart",
			CheckGate: func(ctx context.Context) (*GateState, error) {
				return &GateState{HistoryID: "h1", Epoch: epoch.Load(), State: "Open"}, nil
			},
			SimulateFault: func(ctx context.Context, phase string) error {
				epoch.Add(1)
				return nil
			},
			VerifyNativeProof: func(ctx context.Context, historyID string, baselineEpoch, recoveredEpoch int64) (NativeProof, error) {
				return NativeProof{LeaseFenceVerified: true, SealedCutVerified: true, ReplayIdempotencyVerified: true}, nil
			},
			ReadCheckpoint: func(ctx context.Context, historyID string, epoch int64) (string, error) {
				return "", errors.New("checkpoint missing from durable state")
			},
		}
		if _, err := Run(context.Background(), cfg, opt); err == nil {
			t.Fatal("expected error when durable checkpoint verification fails")
		}
	})

	t.Run("readback SHA mismatch fails closed", func(t *testing.T) {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("corrupted-recovered-bytes"))
		})
		cfg, temp := setupFixture(t, handler)
		var epoch atomic.Int64
		epoch.Store(1)
		opt := Options{
			Manifest:   manifestPath,
			Out:        filepath.Join(temp, "bad-sha"),
			Variant:    "r1",
			FaultScope: "node-restart",
			CheckGate: func(ctx context.Context) (*GateState, error) {
				return &GateState{HistoryID: "h1", Epoch: epoch.Load(), State: "Open"}, nil
			},
			SimulateFault: func(ctx context.Context, phase string) error {
				epoch.Add(1)
				return nil
			},
			VerifyNativeProof: func(ctx context.Context, historyID string, baselineEpoch, recoveredEpoch int64) (NativeProof, error) {
				return NativeProof{LeaseFenceVerified: true, SealedCutVerified: true, ReplayIdempotencyVerified: true}, nil
			},
			ReadCheckpoint: func(ctx context.Context, historyID string, epoch int64) (string, error) {
				return "chk-h1-e2", nil
			},
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

func TestRecoveryRejectsNativeProofAbsentAfterEpochAndCheckpoint(t *testing.T) {
	manifestPath, content, _ := createTestManifest(t, t.TempDir())
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(content))
	})
	cfg, temp := setupFixture(t, handler)
	var epoch atomic.Int64
	epoch.Store(8)
	opt := Options{
		Manifest:     manifestPath,
		Out:          filepath.Join(temp, "native-proof-missing"),
		Variant:      "r2",
		FaultScope:   "checkpoint-cut",
		PollInterval: time.Millisecond,
		MaxPollWait:  25 * time.Millisecond,
		CheckGate: func(context.Context) (*GateState, error) {
			return &GateState{HistoryID: "abcd", Epoch: epoch.Load(), State: "Open"}, nil
		},
		SimulateFault: func(context.Context, string) error {
			epoch.Add(1)
			return nil
		},
		ReadCheckpoint: func(context.Context, string, int64) (string, error) {
			return "journal/epochs/abcd/8/seal-synthetic.json", nil
		},
	}
	res, err := Run(context.Background(), cfg, opt)
	if err == nil || !strings.Contains(err.Error(), "native lease-fence") {
		t.Fatalf("expected fail-closed independent native proof, got %v", err)
	}
	if res.Status != "failed" || res.RecoveryReceipt == nil ||
		res.RecoveryReceipt.LeaseFenceVerified ||
		res.RecoveryReceipt.SealedCutVerified ||
		res.RecoveryReceipt.ReplayIdempotencyVerified {
		t.Fatalf("must not fabricate native proof on valid-looking checkpoint: %+v", res)
	}
}

func TestRecoveryRefusesDSGCredentialAsDynamoDBSession(t *testing.T) {
	cfg, temp := setupFixture(t, nil)
	for _, opt := range []Options{
		{AWSCredentialsFile: "", ExpectedAWSAccount: "123456789012"},
		{AWSCredentialsFile: cfg.CredentialsFile, ExpectedAWSAccount: "123456789012"},
		{AWSCredentialsFile: filepath.Join(temp, "missing"), ExpectedAWSAccount: "123456789012"},
	} {
		if _, err := readDynamoDBClient(context.Background(), cfg, opt); err == nil {
			t.Fatalf("unsafe DynamoDB credentials accepted: %+v", opt)
		}
	}
}
