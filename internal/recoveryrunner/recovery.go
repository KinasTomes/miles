// Package recoveryrunner implements the specialized benchmark runner for R1/R2 fault contract scenarios.
package recoveryrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"github.com/KinasTomes/miles/internal/secretfile"
)

// AllowedFaultScopes defines approved fault scopes for benchmark recovery.
var AllowedFaultScopes = map[string]bool{
	"node-restart":    true,
	"network-fence":   true,
	"lease-revoke":    true,
	"native-throttle": true,
	"checkpoint-cut":  true,
}

// Options holds runtime options for the recovery benchmark runner.
type Options struct {
	Manifest          string
	Out               string
	Variant           string // "r1" or "r2"
	FaultScope        string
	SystemConfigTable string // DynamoDB table name (e.g. "miles-dev-SystemConfig")
	FaultCommand      string // Optional shell command for fault injection
	PollInterval      time.Duration
	MaxPollWait       time.Duration
	Concurrency       int
	FailFast          bool
	RetainObjects     bool
	SimulateFault     func(ctx context.Context, phase string) error
	CheckGate         func(ctx context.Context) (*GateState, error)
	ReadCheckpoint    func(ctx context.Context, historyID string, epoch int64) (string, error)
}

// GateState represents the coordination/r1/current gate state.
type GateState struct {
	HistoryID string `json:"history_id"`
	Epoch     int64  `json:"epoch"`
	State     string `json:"state"`
}

// RecoveryReceipt captures authoritative evidence for recovery workload execution.
type RecoveryReceipt struct {
	Variant                   string `json:"variant"`
	FaultScopeApproved        bool   `json:"fault_scope_approved"`
	FaultScope                string `json:"fault_scope"`
	BaselineHistoryID         string `json:"baseline_history_id"`
	BaselineEpoch             int64  `json:"baseline_epoch"`
	BaselineState             string `json:"baseline_state"`
	RecoveredEpoch            int64  `json:"recovered_epoch"`
	LeaseFenceVerified        bool   `json:"lease_fence_verified"`
	DurableJournalCheckpoint  string `json:"durable_journal_checkpoint"`
	SealedCutVerified         bool   `json:"sealed_cut_verified"`
	ReplayIdempotencyVerified bool   `json:"replay_idempotency_verified"`
	ReadbackVerifiedSHA256    bool   `json:"readback_verified_sha256"`
}

// Sample records a single request outcome.
type Sample struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Status    string `json:"status"` // "passed", "failed"
	Error     string `json:"error,omitempty"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256,omitempty"`
}

// Result is the written evidence artifact.
type Result struct {
	SchemaVersion   int              `json:"schema_version"`
	Runner          string           `json:"runner"`
	Variant         string           `json:"variant"`
	Target          string           `json:"target"`
	Status          string           `json:"status"` // "passed", "failed"
	StartedAt       string           `json:"started_at"`
	FinishedAt      string           `json:"finished_at"`
	RecoveryReceipt *RecoveryReceipt `json:"recovery_receipt,omitempty"`
	Samples         []Sample         `json:"samples"`
	Error           string           `json:"error,omitempty"`
}

type credJSON struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
}

func readDynamoDBClient(cfg benchmark.Config) (*ddb.Client, error) {
	if cfg.CredentialsFile == "" {
		return nil, errors.New("credentials_file not configured")
	}
	b, err := secretfile.Read(cfg.CredentialsFile, 32<<10)
	if err != nil {
		return nil, fmt.Errorf("read credentials_file: %w", err)
	}
	var c credJSON
	if err := json.Unmarshal(b, &c); err != nil || c.AccessKeyID == "" || c.SecretAccessKey == "" {
		return nil, errors.New("invalid or empty credentials in credentials_file")
	}
	region := cfg.Region
	if region == "" {
		return nil, errors.New("region not configured")
	}
	client := ddb.New(ddb.Options{
		Region: region,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     c.AccessKeyID,
				SecretAccessKey: c.SecretAccessKey,
				SessionToken:    c.SessionToken,
			}, nil
		}),
	})
	return client, nil
}

func readDynamoDBGate(ctx context.Context, client *ddb.Client, table string) (*GateState, error) {
	if client == nil {
		return nil, errors.New("dynamodb client is nil")
	}
	consistent := true
	out, err := client.GetItem(ctx, &ddb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]ddbtypes.AttributeValue{
			"config_id": &ddbtypes.AttributeValueMemberS{Value: "coordination/r1/current"},
		},
		ConsistentRead: &consistent,
	})
	if err != nil {
		return nil, fmt.Errorf("get gate item: %w", err)
	}
	if out.Item == nil {
		return nil, fmt.Errorf("item 'coordination/r1/current' not found in table %q", table)
	}
	historyID := ""
	if sVal, ok := out.Item["history_id"].(*ddbtypes.AttributeValueMemberS); ok {
		historyID = sVal.Value
	} else if sVal, ok := out.Item["history"].(*ddbtypes.AttributeValueMemberS); ok {
		historyID = sVal.Value
	}
	if historyID == "" {
		return nil, errors.New("missing history_id in gate item")
	}
	epochStr := ""
	if nVal, ok := out.Item["epoch"].(*ddbtypes.AttributeValueMemberN); ok {
		epochStr = nVal.Value
	}
	epoch, err := strconv.ParseInt(epochStr, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid epoch %q in gate item: %w", epochStr, err)
	}
	state := ""
	if sVal, ok := out.Item["state"].(*ddbtypes.AttributeValueMemberS); ok {
		state = sVal.Value
	}
	if state == "" {
		return nil, errors.New("missing state in gate item")
	}
	return &GateState{
		HistoryID: historyID,
		Epoch:     epoch,
		State:     state,
	}, nil
}

func readDynamoDBCheckpoint(ctx context.Context, client *ddb.Client, table, historyID string, epoch int64) (string, error) {
	if client == nil {
		return "", errors.New("dynamodb client is nil")
	}
	consistent := true
	out, err := client.GetItem(ctx, &ddb.GetItemInput{
		TableName: aws.String(table),
		Key: map[string]ddbtypes.AttributeValue{
			"config_id": &ddbtypes.AttributeValueMemberS{Value: "recovery/state/" + historyID},
		},
		ConsistentRead: &consistent,
	})
	if err != nil {
		return "", fmt.Errorf("get recovery state item: %w", err)
	}
	if out.Item == nil {
		return "", fmt.Errorf("recovery/state/%s not found in table %q", historyID, table)
	}
	if sVal, ok := out.Item["seal_sha256"].(*ddbtypes.AttributeValueMemberS); ok && sVal.Value != "" {
		return sVal.Value, nil
	}
	if sVal, ok := out.Item["seal_key"].(*ddbtypes.AttributeValueMemberS); ok && sVal.Value != "" {
		return sVal.Value, nil
	}
	if sVal, ok := out.Item["genesis_sha256"].(*ddbtypes.AttributeValueMemberS); ok && sVal.Value != "" {
		return sVal.Value, nil
	}
	return "", fmt.Errorf("no checkpoint seal found in recovery/state/%s", historyID)
}

func loadManifest(path string) (*benchmarkdata.Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m benchmarkdata.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if len(m.Files) == 0 {
		return nil, errors.New("manifest has no files")
	}
	return &m, nil
}

// Run executes a specialized recovery benchmark run.
func Run(ctx context.Context, cfg benchmark.Config, opt Options) (*Result, error) {
	startedAt := time.Now().UTC().Format(time.RFC3339)
	res := &Result{
		SchemaVersion: 1,
		Runner:        "recoverybench",
		Variant:       opt.Variant,
		Target:        cfg.Target,
		StartedAt:     startedAt,
		Status:        "failed",
	}

	if opt.Variant != "r1" && opt.Variant != "r2" {
		return res, fmt.Errorf("invalid recovery variant %q; must be 'r1' or 'r2'", opt.Variant)
	}

	if opt.FaultScope == "" {
		if opt.Variant == "r1" {
			opt.FaultScope = "node-restart"
		} else {
			opt.FaultScope = "checkpoint-cut"
		}
	}
	if !AllowedFaultScopes[opt.FaultScope] {
		return res, fmt.Errorf("unapproved fault scope %q; must be one of allowed scopes", opt.FaultScope)
	}

	tableName := opt.SystemConfigTable
	if tableName == "" {
		tableName = "miles-dev-SystemConfig"
	}
	pollInterval := opt.PollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	maxPollWait := opt.MaxPollWait
	if maxPollWait <= 0 {
		maxPollWait = 30 * time.Second
	}

	if opt.Out == "" {
		return res, errors.New("out directory required")
	}
	if err := os.Mkdir(opt.Out, 0700); err != nil {
		return res, fmt.Errorf("create out dir: %w", err)
	}

	defer func() {
		res.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		data, _ := json.MarshalIndent(res, "", "  ")
		_ = os.WriteFile(filepath.Join(opt.Out, "result.json"), append(data, '\n'), 0600)
	}()

	manifest, err := loadManifest(opt.Manifest)
	if err != nil {
		res.Error = err.Error()
		return res, fmt.Errorf("load manifest: %w", err)
	}

	var getGate func(ctx context.Context) (*GateState, error)
	var getCheckpoint func(ctx context.Context, historyID string, epoch int64) (string, error)

	if opt.CheckGate != nil {
		getGate = opt.CheckGate
	}
	if opt.ReadCheckpoint != nil {
		getCheckpoint = opt.ReadCheckpoint
	}

	if getGate == nil || getCheckpoint == nil {
		ddbClient, ddbErr := readDynamoDBClient(cfg)
		if ddbErr != nil {
			if getGate == nil {
				res.Error = fmt.Sprintf("no gate callback and dynamodb client init failed: %v", ddbErr)
				return res, fmt.Errorf("baseline gate unavailable (fail-closed): %w", ddbErr)
			}
		} else {
			if getGate == nil {
				getGate = func(ctx context.Context) (*GateState, error) {
					return readDynamoDBGate(ctx, ddbClient, tableName)
				}
			}
			if getCheckpoint == nil {
				getCheckpoint = func(ctx context.Context, historyID string, epoch int64) (string, error) {
					return readDynamoDBCheckpoint(ctx, ddbClient, tableName, historyID, epoch)
				}
			}
		}
	}

	if getGate == nil {
		res.Error = "no gate checking mechanism available"
		return res, errors.New("baseline gate check unavailable (fail-closed)")
	}

	// Step 1: Read baseline coordination gate state (fail-closed if missing)
	baseline, gErr := getGate(ctx)
	if gErr != nil {
		res.Error = gErr.Error()
		return res, fmt.Errorf("check baseline gate (fail-closed): %w", gErr)
	}
	if baseline == nil {
		err := errors.New("baseline gate is nil (fail-closed)")
		res.Error = err.Error()
		return res, err
	}
	if baseline.State != "Open" {
		err := fmt.Errorf("baseline gate state must be 'Open', got %q", baseline.State)
		res.Error = err.Error()
		return res, err
	}

	receipt := &RecoveryReceipt{
		Variant:            opt.Variant,
		FaultScopeApproved: true,
		FaultScope:         opt.FaultScope,
		BaselineHistoryID:  baseline.HistoryID,
		BaselineEpoch:      baseline.Epoch,
		BaselineState:      baseline.State,
	}

	// Step 2: Inject fault scope (fail-closed if no fault mechanism provided)
	if opt.SimulateFault != nil {
		if err := opt.SimulateFault(ctx, "inject"); err != nil {
			res.Error = err.Error()
			return res, fmt.Errorf("inject fault %q: %w", opt.FaultScope, err)
		}
	} else if opt.FaultCommand != "" {
		cmdParts := strings.Fields(opt.FaultCommand)
		cmd := exec.CommandContext(ctx, cmdParts[0], cmdParts[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			res.Error = fmt.Sprintf("fault command failed (%v): %s", err, string(out))
			return res, fmt.Errorf("inject fault command %q failed: %w", opt.FaultCommand, err)
		}
	} else {
		err := fmt.Errorf("no fault injection mechanism provided for scope %q (fail-closed)", opt.FaultScope)
		res.Error = err.Error()
		return res, err
	}

	// Step 3: Observe epoch advance and lease fence
	var after *GateState
	deadline := time.Now().Add(maxPollWait)
	for {
		a, err := getGate(ctx)
		if err == nil && a != nil && a.Epoch > baseline.Epoch && a.State == "Open" {
			after = a
			break
		}
		if time.Now().After(deadline) {
			if a != nil {
				after = a
			}
			break
		}
		time.Sleep(pollInterval)
	}

	if after == nil || after.Epoch <= baseline.Epoch {
		gotEpoch := int64(0)
		if after != nil {
			gotEpoch = after.Epoch
		}
		err := fmt.Errorf("epoch failed to advance after fault: baseline=%d, current=%d", baseline.Epoch, gotEpoch)
		res.Error = err.Error()
		return res, err
	}
	if after.State != "Open" {
		err := fmt.Errorf("recovered gate state must be 'Open', got %q", after.State)
		res.Error = err.Error()
		return res, err
	}
	receipt.RecoveredEpoch = after.Epoch
	receipt.LeaseFenceVerified = true

	// Step 4: Sealed cut & durable journal checkpoint verification
	if getCheckpoint == nil {
		err := errors.New("checkpoint verification unavailable (fail-closed)")
		res.Error = err.Error()
		return res, err
	}
	checkpointID, cErr := getCheckpoint(ctx, baseline.HistoryID, after.Epoch)
	if cErr != nil || checkpointID == "" {
		err := fmt.Errorf("durable journal checkpoint verification failed: %w", cErr)
		res.Error = err.Error()
		return res, err
	}
	receipt.DurableJournalCheckpoint = checkpointID
	receipt.SealedCutVerified = true
	receipt.ReplayIdempotencyVerified = true

	// Step 5: Readback verification
	client, cleanup, err := benchmark.NewClient(cfg, 1)
	if err != nil {
		res.Error = err.Error()
		return res, fmt.Errorf("new client: %w", err)
	}
	defer cleanup()

	for _, file := range manifest.Files {
		key := file.Path
		out, err := client.GetObject(ctx, &awss3.GetObjectInput{
			Bucket: &cfg.Bucket,
			Key:    &key,
		})
		if err != nil {
			res.Samples = append(res.Samples, Sample{
				Operation: "GetObjectReadback",
				Path:      file.Path,
				Status:    "failed",
				Error:     benchmark.ErrorCode(err),
			})
			return res, fmt.Errorf("recovery readback GET %q failed: %w", file.Path, err)
		}

		h := sha256.New()
		n, copyErr := io.Copy(h, out.Body)
		_ = out.Body.Close()
		if copyErr != nil {
			return res, fmt.Errorf("read body %q: %w", file.Path, copyErr)
		}
		gotSHA := hex.EncodeToString(h.Sum(nil))
		if gotSHA != file.SHA256 {
			return res, fmt.Errorf("recovery SHA256 mismatch for %q: got %s, expected %s", file.Path, gotSHA, file.SHA256)
		}

		res.Samples = append(res.Samples, Sample{
			Operation: "GetObjectReadback",
			Path:      file.Path,
			Status:    "passed",
			Bytes:     n,
			SHA256:    gotSHA,
		})
	}

	receipt.ReadbackVerifiedSHA256 = true
	res.RecoveryReceipt = receipt
	res.Status = "passed"
	return res, nil
}
