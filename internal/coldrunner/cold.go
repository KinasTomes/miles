// Package coldrunner implements the specialized benchmark runner for S3 Cold storage classes (GLACIER_IR and GLACIER).
package coldrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
)

// Options holds runtime options for the cold benchmark runner.
type Options struct {
	Manifest      string
	Out           string
	Variant       string // "ir" or "flexible"
	Concurrency   int
	FailFast      bool
	RetainObjects bool
	PollInterval  time.Duration
	MaxPollWait   time.Duration
}

// ColdReceipt captures authoritative evidence for cold workload execution.
type ColdReceipt struct {
	StorageClass                string `json:"storage_class"`
	ObservedBackendStorageClass string `json:"observed_backend_storage_class"`
	ObservedVersion             string `json:"observed_version,omitempty"`
	RestorationStatus           string `json:"restoration_status"` // "immediate", "in_progress", "completed"
	RestorationStartedAt        string `json:"restoration_started_at,omitempty"`
	RestorationCompletedAt      string `json:"restoration_completed_at,omitempty"`
	RestorationExpiry           string `json:"restoration_expiry,omitempty"`
	RevivalVerified             bool   `json:"revival_verified"`
	RetainedPhysicalBytes       *int64 `json:"retained_physical_bytes,omitempty"`
	VerifiedLogicalGETBytes     int64  `json:"verified_logical_get_bytes"`
	TotalGETVerifiedSHA256      bool   `json:"total_get_verified_sha256"`
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
	SchemaVersion int          `json:"schema_version"`
	Runner        string       `json:"runner"`
	Variant       string       `json:"variant"`
	Target        string       `json:"target"`
	Status        string       `json:"status"` // "passed", "failed"
	StartedAt     string       `json:"started_at"`
	FinishedAt    string       `json:"finished_at"`
	ColdReceipt   *ColdReceipt `json:"cold_receipt,omitempty"`
	Samples       []Sample     `json:"samples"`
	Error         string       `json:"error,omitempty"`
}

var restoreExpiryRegex = regexp.MustCompile(`expiry-date="([^"]+)"`)

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

// Run executes a specialized cold benchmark run.
func Run(ctx context.Context, cfg benchmark.Config, opt Options) (*Result, error) {
	startedAt := time.Now().UTC().Format(time.RFC3339)
	res := &Result{
		SchemaVersion: 1,
		Runner:        "coldbench",
		Variant:       opt.Variant,
		Target:        cfg.Target,
		StartedAt:     startedAt,
		Status:        "failed",
	}

	if opt.Variant != "ir" && opt.Variant != "flexible" {
		return res, fmt.Errorf("invalid cold variant %q; must be 'ir' or 'flexible'", opt.Variant)
	}

	expectedClass := "GLACIER_IR"
	if opt.Variant == "flexible" {
		expectedClass = "GLACIER"
	}
	if cfg.StorageClass != "" && cfg.StorageClass != expectedClass {
		return res, fmt.Errorf("storage class %q mismatch for variant %q (expected %q)", cfg.StorageClass, opt.Variant, expectedClass)
	}
	cfg.StorageClass = expectedClass

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

	client, cleanup, err := benchmark.NewClient(cfg, 1)
	if err != nil {
		res.Error = err.Error()
		return res, fmt.Errorf("new client: %w", err)
	}
	defer cleanup()

	receipt := &ColdReceipt{
		StorageClass:           expectedClass,
		RetainedPhysicalBytes:  nil,
		TotalGETVerifiedSHA256: false,
	}

	if opt.Variant == "ir" {
		if err := runIR(ctx, client, cfg, manifest, receipt, res, opt); err != nil {
			res.Error = err.Error()
			return res, err
		}
	} else {
		if err := runFlexible(ctx, client, cfg, manifest, receipt, res, opt); err != nil {
			res.Error = err.Error()
			return res, err
		}
	}

	res.ColdReceipt = receipt
	// Logical GET size is not a backend physical-inventory certificate.
	if receipt.RetainedPhysicalBytes == nil {
		err := errors.New("authoritative retained physical inventory unavailable (fail-closed)")
		res.Error = err.Error()
		res.Status = "incomplete"
		return res, err
	}
	res.Status = "passed"
	return res, nil
}

func runIR(ctx context.Context, client *awss3.Client, cfg benchmark.Config, manifest *benchmarkdata.Manifest, receipt *ColdReceipt, res *Result, opt Options) error {
	receipt.RestorationStatus = "immediate"
	receipt.RevivalVerified = true

	for _, file := range manifest.Files {
		key := file.Path
		out, err := client.GetObject(ctx, &awss3.GetObjectInput{
			Bucket: &cfg.Bucket,
			Key:    &key,
		})
		if err != nil {
			res.Samples = append(res.Samples, Sample{
				Operation: "GetObject",
				Path:      file.Path,
				Status:    "failed",
				Error:     benchmark.ErrorCode(err),
			})
			return fmt.Errorf("cold IR GET %q failed: %w", file.Path, err)
		}

		observedClass := string(out.StorageClass)
		if observedClass != string(s3types.StorageClassGlacierIr) {
			return fmt.Errorf("cold IR GET %q observed unexpected storage class %q; expected %q", file.Path, observedClass, s3types.StorageClassGlacierIr)
		}
		receipt.ObservedBackendStorageClass = observedClass
		if out.VersionId != nil {
			receipt.ObservedVersion = *out.VersionId
		}

		h := sha256.New()
		n, copyErr := io.Copy(h, out.Body)
		_ = out.Body.Close()
		if copyErr != nil {
			return fmt.Errorf("read body %q: %w", file.Path, copyErr)
		}
		if out.ContentLength != nil && *out.ContentLength != n {
			return fmt.Errorf("content length mismatch for %q: header=%d, read=%d", file.Path, *out.ContentLength, n)
		}
		receipt.VerifiedLogicalGETBytes += n
		gotSHA := hex.EncodeToString(h.Sum(nil))
		if gotSHA != file.SHA256 {
			return fmt.Errorf("SHA256 mismatch for %q: got %s, expected %s", file.Path, gotSHA, file.SHA256)
		}

		res.Samples = append(res.Samples, Sample{
			Operation: "GetObject",
			Path:      file.Path,
			Status:    "passed",
			Bytes:     n,
			SHA256:    gotSHA,
		})
	}

	receipt.TotalGETVerifiedSHA256 = true
	return nil
}

func runFlexible(ctx context.Context, client *awss3.Client, cfg benchmark.Config, manifest *benchmarkdata.Manifest, receipt *ColdReceipt, res *Result, opt Options) error {
	pollInterval := opt.PollInterval
	if pollInterval <= 0 {
		pollInterval = 500 * time.Millisecond
	}
	maxPollWait := opt.MaxPollWait
	if maxPollWait <= 0 {
		maxPollWait = 30 * time.Second
	}

	// Step 1: Prove that immediate GET on archived object fails-closed before restore with InvalidObjectState.
	for _, file := range manifest.Files {
		key := file.Path
		_, err := client.GetObject(ctx, &awss3.GetObjectInput{
			Bucket: &cfg.Bucket,
			Key:    &key,
		})
		if err == nil {
			return fmt.Errorf("Glacier flexible object %q readable before restore; expected InvalidObjectState", file.Path)
		}
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidObjectState" {
			return fmt.Errorf("pre-restore GET %q failed with %v; expected strictly InvalidObjectState", file.Path, err)
		}
	}

	// Step 2: Request restoration.
	receipt.RestorationStartedAt = time.Now().UTC().Format(time.RFC3339)
	receipt.RestorationStatus = "in_progress"

	tier := s3types.TierStandard
	days := int32(1)
	for _, file := range manifest.Files {
		key := file.Path
		_, err := client.RestoreObject(ctx, &awss3.RestoreObjectInput{
			Bucket: &cfg.Bucket,
			Key:    &key,
			RestoreRequest: &s3types.RestoreRequest{
				Days: &days,
				GlacierJobParameters: &s3types.GlacierJobParameters{
					Tier: tier,
				},
			},
		})
		if err != nil {
			var apiErr smithy.APIError
			if errors.As(err, &apiErr) && apiErr.ErrorCode() == "RestoreAlreadyInProgress" {
				// Acceptable if idempotent retry
			} else {
				return fmt.Errorf("RestoreObject %q failed: %w", file.Path, err)
			}
		}
	}

	// Step 3: Wait / Poll for restore completion.
	deadline := time.Now().Add(maxPollWait)
	for _, file := range manifest.Files {
		key := file.Path
		completed := false

		for time.Now().Before(deadline) {
			head, err := client.HeadObject(ctx, &awss3.HeadObjectInput{
				Bucket: &cfg.Bucket,
				Key:    &key,
			})
			if err != nil {
				return fmt.Errorf("HeadObject %q during restore poll: %w", file.Path, err)
			}

			observedClass := string(head.StorageClass)
			if observedClass != string(s3types.StorageClassGlacier) {
				return fmt.Errorf("cold Flexible object %q observed storage class %q; expected %q", file.Path, observedClass, s3types.StorageClassGlacier)
			}
			receipt.ObservedBackendStorageClass = observedClass
			if head.VersionId != nil {
				receipt.ObservedVersion = *head.VersionId
			}

			restoreHeader := ""
			if head.Restore != nil {
				restoreHeader = *head.Restore
			}

			if strings.Contains(restoreHeader, `ongoing-request="false"`) {
				completed = true
				if m := restoreExpiryRegex.FindStringSubmatch(restoreHeader); len(m) > 1 {
					receipt.RestorationExpiry = m[1]
				}
				break
			}

			// While ongoing, verify that direct GET is strictly rejected with InvalidObjectState
			if strings.Contains(restoreHeader, `ongoing-request="true"`) {
				_, getErr := client.GetObject(ctx, &awss3.GetObjectInput{
					Bucket: &cfg.Bucket,
					Key:    &key,
				})
				if getErr == nil {
					return fmt.Errorf("Glacier flexible object %q accessible while ongoing-request=true", file.Path)
				}
				var apiErr smithy.APIError
				if !errors.As(getErr, &apiErr) || apiErr.ErrorCode() != "InvalidObjectState" {
					return fmt.Errorf("ongoing restore GET %q failed with %v; expected strictly InvalidObjectState", file.Path, getErr)
				}
			}

			time.Sleep(pollInterval)
		}

		if !completed {
			return fmt.Errorf("timed out waiting for restoration of %q", file.Path)
		}
	}

	receipt.RestorationCompletedAt = time.Now().UTC().Format(time.RFC3339)
	receipt.RestorationStatus = "completed"
	receipt.RevivalVerified = true

	// Step 4: GET restored object, verify full SHA-256.
	for _, file := range manifest.Files {
		key := file.Path
		out, err := client.GetObject(ctx, &awss3.GetObjectInput{
			Bucket: &cfg.Bucket,
			Key:    &key,
		})
		if err != nil {
			return fmt.Errorf("GET restored object %q failed: %w", file.Path, err)
		}

		h := sha256.New()
		n, copyErr := io.Copy(h, out.Body)
		_ = out.Body.Close()
		if copyErr != nil {
			return fmt.Errorf("read restored body %q: %w", file.Path, copyErr)
		}
		gotSHA := hex.EncodeToString(h.Sum(nil))
		if gotSHA != file.SHA256 {
			return fmt.Errorf("restored SHA256 mismatch for %q: got %s, expected %s", file.Path, gotSHA, file.SHA256)
		}

		if out.ContentLength != nil && *out.ContentLength != n {
			return fmt.Errorf("content length mismatch for restored %q: header=%d, read=%d", file.Path, *out.ContentLength, n)
		}
		receipt.VerifiedLogicalGETBytes += n

		res.Samples = append(res.Samples, Sample{
			Operation: "GetObjectRestored",
			Path:      file.Path,
			Status:    "passed",
			Bytes:     n,
			SHA256:    gotSHA,
		})
	}

	receipt.TotalGETVerifiedSHA256 = true
	return nil
}
