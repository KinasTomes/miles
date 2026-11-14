// Package benchmark runs the same bounded, single-PUT S3 workload against both
// targets. It never provisions infrastructure or substitutes a fake Miles server.
package benchmark

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"github.com/KinasTomes/miles/internal/secretfile"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const multipartThreshold = 8 * 1024 * 1024

type Options struct {
	Manifest      string
	Out           string
	Concurrency   int
	RetainObjects bool          // Retain only this run prefix for external statistics; caller owns cleanup.
	ReadRepeats   int           // additional full GET passes, separate from initial verification
	FailFast      bool          // Halt dispatching new requests immediately on first error; in-flight requests drain.
	DrainTimeout  time.Duration // Emergency deadline for in-flight requests to drain after interruption (0 uses requestTimeout)
	Scenario      string        // warm (default), overwrite, multipart, domain, edge-clone, cold, recovery
	Variant       string        // scenario variant selector (e.g. threshold-minus-one, overwrite-versions, etc.)
	Domain        string        // target dedup domain (default, shared-default, separated, cross-domain)
	SourcePrefix  string        // for edge-clone download: source prefix to download from
	SecondConfig  string        // for domain scenario: second tenant private config (0600) for cross-owner isolation
}

type Sample struct {
	Phase          string `json:"phase"`
	Operation      string `json:"operation"`
	File           string `json:"file,omitempty"`
	Group          string `json:"group,omitempty"`
	LatencyNS      int64  `json:"latency_ns"`
	PayloadBytes   int64  `json:"payload_bytes"`
	SHA256         string `json:"sha256,omitempty"`
	Error          string `json:"error,omitempty"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	Indeterminate  bool   `json:"indeterminate,omitempty"`
	Reconciliation string `json:"reconciliation,omitempty"`
	PartCount      int    `json:"part_count,omitempty"`
	DispatchedType string `json:"dispatched_type,omitempty"`
}

type Phase struct {
	Name            string  `json:"name"`
	Samples         int     `json:"samples"`
	Planned         int     `json:"planned"`
	Dispatched      int     `json:"dispatched"`
	Completed       int     `json:"completed"`
	Skipped         int     `json:"skipped"`
	Failed          int     `json:"failed"`
	ElapsedNS       int64   `json:"elapsed_ns"`
	SuccessfulBytes int64   `json:"successful_logical_bytes"`
	BytesPerSecond  float64 `json:"successful_logical_bytes_per_second"`
	P50NS           int64   `json:"p50_ns"`
	P95NS           int64   `json:"p95_ns"`
	MaxNS           int64   `json:"max_ns"`
	StopTrigger     string  `json:"stop_trigger,omitempty"`
}

type CloneReceipt struct {
	SourceDomain      string `json:"source_domain"`
	DestinationDomain string `json:"destination_domain"`
	SourcePrefix      string `json:"source_prefix,omitempty"`
	DestinationPrefix string `json:"destination_prefix,omitempty"`
	FilesCloned       int    `json:"files_cloned"`
	TotalBytesCloned  int64  `json:"total_bytes_cloned"`
	ReceiptSHA256     string `json:"receipt_sha256"`
}

type Result struct {
	SchemaVersion            int                    `json:"schema_version"`
	MVPThresholdApplied      bool                   `json:"mvp_threshold_applied"`
	Status                   string                 `json:"status"`
	Error                    string                 `json:"error,omitempty"`
	Target                   string                 `json:"target"`
	Region                   string                 `json:"region"`
	StartedAt                time.Time              `json:"started_at"`
	FinishedAt               time.Time              `json:"finished_at"`
	Prefix                   string                 `json:"prefix"`
	ManifestSHA256           string                 `json:"manifest_sha256"`
	Dataset                  benchmarkdata.Manifest `json:"dataset"`
	Concurrency              int                    `json:"concurrency"`
	ReadRepeats              int                    `json:"read_repeats"`
	FailFast                 bool                   `json:"fail_fast"`
	Scenario                 string                 `json:"scenario,omitempty"`
	Variant                  string                 `json:"variant,omitempty"`
	Domain                   string                 `json:"domain,omitempty"`
	Tenant1Domain            string                 `json:"tenant1_domain,omitempty"`
	Tenant2Domain            string                 `json:"tenant2_domain,omitempty"`
	Tenant1RuntimeDomain     string                 `json:"tenant1_runtime_domain,omitempty"`
	Tenant2RuntimeDomain     string                 `json:"tenant2_runtime_domain,omitempty"`
	DomainContrastVerified   bool                   `json:"domain_contrast_verified,omitempty"`
	DomainDedupObserved      bool                   `json:"domain_dedup_observed,omitempty"`
	DomainPhysicalIsolation  bool                   `json:"domain_physical_isolation,omitempty"`
	DomainCrossOwnerCheck    *Sample                `json:"domain_cross_owner_check,omitempty"`
	DomainCrossOwnerRejected bool                   `json:"domain_cross_owner_rejected,omitempty"`
	SecondTenantVerified     bool                   `json:"second_tenant_verified,omitempty"`
	SecondTenantPhases       []Phase                `json:"second_tenant_phases,omitempty"`
	SecondTenantSamples      []Sample               `json:"second_tenant_samples,omitempty"`
	SecondTenantCleanup      string                 `json:"second_tenant_cleanup,omitempty"`
	CloneReceipt             *CloneReceipt          `json:"clone_receipt,omitempty"`
	LiveLogicalBytes         int64                  `json:"live_logical_bytes,omitempty"`
	HistoricalBytes          int64                  `json:"historical_bytes,omitempty"`
	MultipartCount           int                    `json:"multipart_count,omitempty"`
	StopTrigger              string                 `json:"stop_trigger,omitempty"`
	NeedsReconciliation      bool                   `json:"needs_reconciliation,omitempty"`
	DeclaredEnvironment      Environment            `json:"declared_environment"`
	Client                   map[string]string      `json:"client"`
	Phases                   []Phase                `json:"phases"`
	Samples                  []Sample               `json:"samples"`
	Cleanup                  string                 `json:"cleanup"`
	HTTPBodyBytes            map[string]uint64      `json:"http_body_bytes"`
	Limitations              []string               `json:"limitations"`
}

// Run writes a private artifact even on request failure. A fresh random prefix
// owns only this run's keys; failures retain them for diagnosis, never bulk delete.
func Run(ctx context.Context, c Config, o Options) (result Result, err error) {
	if ctx == nil || o.Concurrency < 1 || o.Concurrency > 64 || o.ReadRepeats < 0 || o.ReadRepeats > 10 || o.Manifest == "" || o.Out == "" {
		return result, errors.New("require manifest, new out directory, concurrency 1..64 and read-repeats 0..10")
	}
	if o.Scenario == "" {
		o.Scenario = "warm"
	}
	if o.Domain == "" {
		o.Domain = "default"
	}
	if o.Scenario == "cold" || o.Scenario == "recovery" {
		return result, fmt.Errorf("scenario %q requires specialized benchmark runner (P2 restore/revival or R1/R2 fault contract); warmbench does not certify this scenario", o.Scenario)
	}
	root, manifest, raw, err := LoadDataset(ctx, o.Manifest)
	if err != nil {
		return result, err
	}
	defer root.Close()

	if o.Scenario == "multipart" && o.Variant != "" {
		var filtered []benchmarkdata.File
		var total int64
		for _, f := range manifest.Files {
			match := false
			switch o.Variant {
			case "threshold-minus-one":
				match = strings.Contains(f.Path, "minus-one")
			case "threshold-equal":
				match = strings.Contains(f.Path, "equal")
			case "threshold-plus-one":
				match = strings.Contains(f.Path, "plus-one")
			default:
				match = true
			}
			if match {
				filtered = append(filtered, f)
				total += int64(f.Bytes)
			}
		}
		if len(filtered) > 0 {
			manifest.Files = filtered
			manifest.TotalBytes = total
		}
	}

	traffic := new(bodyTraffic)
	client, closeClient, err := newClient(c, o.Concurrency, traffic)
	if err != nil {
		return result, err
	}
	defer closeClient()
	id := make([]byte, 16)
	if _, err = rand.Read(id); err != nil {
		return result, errors.New("cannot generate run identity")
	}
	if err = os.Mkdir(o.Out, 0700); err != nil {
		return result, errors.New("out must be a new directory with an existing parent")
	}
	sum := sha256.Sum256(raw)
	result = Result{
		SchemaVersion: 1, Status: "running", Target: c.Target, Region: c.Region, StartedAt: time.Now().UTC(),
		Prefix: "warmbench/" + hex.EncodeToString(id) + "/", ManifestSHA256: hex.EncodeToString(sum[:]), Dataset: manifest,
		Concurrency: o.Concurrency, ReadRepeats: o.ReadRepeats, DeclaredEnvironment: c.Environment, Client: clientVersions(),
		FailFast: o.FailFast, Scenario: o.Scenario, Variant: o.Variant, Domain: o.Domain,
		Cleanup: "not_started", Phases: []Phase{}, Samples: []Sample{},
		Limitations: []string{
			"Preliminary only. smoke validates the harness; it is not a performance or R1/R2 acceptance result.",
			"Single PutObject, SHA256 header, no multipart, no SDK retries; queue dispatch follows manifest order, completion order is concurrent.",
			"Latency includes SDK/signing/body I/O and GET hashing; throughput uses each phase wall time. Payload bytes are logical, not wire/backend/billed bytes.",
			"GET passes are separate; no cache purge is performed and no cold/warm hit ratio is inferred. Fresh keys do not reset domain dedup.",
			"Delete verifies logical absence only; versions, tombstones, retained packs and asynchronous work can remain. No physical/cost accounting.",
		},
	}
	if err = saveResult(o.Out, result); err != nil {
		return result, err
	}
	defer func() {
		result.FinishedAt = time.Now().UTC()
		result.HTTPBodyBytes = traffic.snapshot(c.Target)
		result.Status = "passed"
		if err != nil {
			result.Status, result.Error = "failed", errorCode(err)
		}
		if o.FailFast {
			for _, s := range result.Samples {
				if s.Indeterminate {
					result.NeedsReconciliation = true
					break
				}
			}
			if result.NeedsReconciliation {
				result.Cleanup = "objects_may_remain_indeterminate_outcome"
			}
		}
		if e := saveResult(o.Out, result); e != nil {
			err = errors.Join(err, e)
		}
	}()
	// Check absence before writing; never overwrite an existing run prefix.
	if err = listPhase(ctx, client, c.Bucket, "preflight-list", &result, nil); err != nil {
		return result, err
	}
	result.Cleanup = "objects_may_remain_on_failure"
	if o.Scenario == "overwrite" {
		if err = overwritePhase(ctx, client, root, c, o, &result); err != nil {
			return result, err
		}
		return result, nil
	}
	if o.Scenario == "edge-clone" {
		if o.Variant == "clone-download" || o.Variant == "clone-upload" {
			if o.SourcePrefix == "" {
				return result, fmt.Errorf("%s requires non-empty --source-prefix", o.Variant)
			}
			if !strings.HasSuffix(o.SourcePrefix, "/") {
				o.SourcePrefix += "/"
			}
		}
		if err = edgeClonePhase(ctx, client, root, c, o, &result, traffic); err != nil {
			return result, err
		}
		return result, nil
	}
	if o.Scenario == "domain" {
		if o.SecondConfig == "" {
			return result, errors.New("domain scenario requires --second-config to configure and contrast two tenants")
		}
		if err = domainPhase(ctx, client, root, c, o, &result, traffic); err != nil {
			return result, err
		}
		return result, nil
	}
	for _, phase := range []string{"put", "head", "verify-get", "list"} {
		if phase == "list" {
			err = listPhase(ctx, client, c.Bucket, phase, &result, manifest.Files)
		} else {
			err = filePhase(ctx, client, root, c, o, phase, &result)
		}
		if err != nil {
			return result, err
		}
	}
	for i := 1; i <= o.ReadRepeats; i++ {
		if err = filePhase(ctx, client, root, c, o, fmt.Sprintf("repeat-get-%d", i), &result); err != nil {
			return result, err
		}
	}
	if o.RetainObjects {
		result.Cleanup = "retained_owned_prefix_for_observation"
		return result, nil
	}
	if err = filePhase(ctx, client, root, c, o, "delete", &result); err != nil {
		return result, err
	}
	if err = listPhase(ctx, client, c.Bucket, "after-delete-list", &result, nil); err != nil {
		return result, err
	}
	result.Cleanup = "logical_delete_verified_physical_retention_unknown"
	return result, nil
}

func filePhase(ctx context.Context, client *awss3.Client, root *os.Root, c Config, o Options, phase string, r *Result) error {
	start := time.Now()
	total := len(r.Dataset.Files)
	samples := make([]Sample, total)
	var workers sync.WaitGroup

	keyPrefix := r.Prefix
	if o.SourcePrefix != "" {
		keyPrefix = o.SourcePrefix
	}
	allowMultipart := (o.Scenario == "multipart")

	if !o.FailFast {
		jobs := make(chan int)
		for i := 0; i < min(o.Concurrency, total); i++ {
			workers.Go(func() {
				for index := range jobs {
					entry := r.Dataset.Files[index]
					samples[index] = fileOperation(ctx, client, root, c, keyPrefix+entry.Path, phase, entry, false, allowMultipart)
				}
			})
		}
		for index := range samples {
			jobs <- index
		}
		close(jobs)
		workers.Wait()
		return finishPhase(r, phase, start, samples)
	}

	if ctx.Err() != nil {
		return ErrInterrupted
	}

	var coord struct {
		mu              sync.Mutex
		nextIndex       int
		dispatchedCount int
		stopped         bool
		interrupted     bool
		stopReason      string
		drainTimer      *time.Timer
	}

	drainCtx, drainCancel := context.WithCancel(context.Background())
	defer func() {
		drainCancel()
		coord.mu.Lock()
		if coord.drainTimer != nil {
			coord.drainTimer.Stop()
		}
		coord.mu.Unlock()
	}()

	scheduleDrainTimeoutLocked := func() {
		if o.DrainTimeout > 0 && coord.drainTimer == nil {
			coord.drainTimer = time.AfterFunc(o.DrainTimeout, func() {
				drainCancel()
			})
		}
	}

	triggerStop := func(reason string) {
		coord.mu.Lock()
		defer coord.mu.Unlock()
		if reason == "interrupted" {
			coord.interrupted = true
			coord.stopped = true
			scheduleDrainTimeoutLocked()
			if coord.stopReason == "" {
				coord.stopReason = "interrupted"
			}
			return
		}
		if !coord.stopped {
			coord.stopped = true
			coord.stopReason = sanitizeStopTrigger(reason)
		}
	}

	claimJob := func() (int, bool) {
		coord.mu.Lock()
		defer coord.mu.Unlock()
		if ctx.Err() != nil {
			coord.interrupted = true
			coord.stopped = true
			scheduleDrainTimeoutLocked()
			if coord.stopReason == "" {
				coord.stopReason = "interrupted"
			}
			return -1, false
		}
		if coord.stopped || coord.interrupted || coord.nextIndex >= total {
			return -1, false
		}
		idx := coord.nextIndex
		coord.nextIndex++
		coord.dispatchedCount++
		return idx, true
	}

	stopWatcher := make(chan struct{})
	defer close(stopWatcher)
	go func() {
		select {
		case <-ctx.Done():
			triggerStop("interrupted")
		case <-stopWatcher:
			return
		}
	}()

	for i := 0; i < min(o.Concurrency, total); i++ {
		workers.Go(func() {
			for {
				index, ok := claimJob()
				if !ok {
					return
				}
				entry := r.Dataset.Files[index]
				s := fileOperation(drainCtx, client, root, c, keyPrefix+entry.Path, phase, entry, true, allowMultipart)
				samples[index] = s

				if s.Error != "" {
					triggerStop(s.Error)
					return
				}
			}
		})
	}

	workers.Wait()

	coord.mu.Lock()
	dispatchedCount := coord.dispatchedCount
	stopReason := coord.stopReason
	interruptedVal := coord.interrupted || (ctx.Err() != nil)
	coord.mu.Unlock()

	completedSamples := make([]Sample, 0, dispatchedCount)
	for _, s := range samples {
		if s.Phase != "" {
			completedSamples = append(completedSamples, s)
		}
	}

	skipped := total - dispatchedCount
	if skipped < 0 {
		skipped = 0
	}
	trigger := stopReason
	if trigger == "" && interruptedVal {
		trigger = "interrupted"
	}
	err := finishPhaseWithStats(r, phase, start, completedSamples, total, dispatchedCount, skipped, trigger)
	if interruptedVal {
		return ErrInterrupted
	}
	return err
}

func fileOperation(ctx context.Context, client *awss3.Client, root *os.Root, c Config, key, phase string, f benchmarkdata.File, failFast, allowMultipart bool) (sample Sample) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	sample = Sample{Phase: phase, Operation: "GetObject", File: f.Path, Group: f.Group}
	var err error
	defer func() {
		sample.LatencyNS = time.Since(start).Nanoseconds()
		if err != nil {
			sample.HTTPStatus, sample.RequestID = responseDiagnostics(err)
			if sample.Error == "" {
				sample.Error = errorCode(err)
			}
		}
	}()
	switch phase {
	case "put":
		if allowMultipart && f.Bytes > multipartThreshold {
			sample = uploadMultipart(ctx, client, root, c, key, phase, f, failFast)
			if sample.Error != "" {
				err = errors.New(sample.Error)
			}
			return sample
		}
		sample.Operation = "PutObject"
		sample.DispatchedType = "single-put"
		var body *os.File
		body, err = root.Open(f.Path)
		if err != nil {
			return sample
		}
		defer body.Close()
		digest, _ := hex.DecodeString(f.SHA256)
		sc := types.StorageClassStandard
		if c.StorageClass != "" {
			sc = types.StorageClass(c.StorageClass)
		}
		input := &awss3.PutObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key), Body: body,
			ContentLength: aws.Int64(int64(f.Bytes)), ContentType: aws.String("application/octet-stream"),
			ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(digest)), StorageClass: sc}
		if c.KMSKeyID != "" {
			input.ServerSideEncryption, input.SSEKMSKeyId = types.ServerSideEncryptionAwsKms, aws.String(c.KMSKeyID)
		}
		_, err = client.PutObject(ctx, input)
		sample.PayloadBytes = int64(f.Bytes) // declared upload bytes, not observed wire bytes
		if failFast && err != nil {
			status, _ := responseDiagnostics(err)
			if status == 0 || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				sample.Indeterminate = true
				sample.Reconciliation = "needs_reconciliation"
			}
		}
	case "head":
		sample.Operation = "HeadObject"
		var out *awss3.HeadObjectOutput
		out, err = client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)})
		if err == nil && (out.ContentLength == nil || *out.ContentLength != int64(f.Bytes)) {
			sample.Error = "size_mismatch"
		}
	case "delete":
		sample.Operation = "DeleteObject"
		_, err = client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)})
	default:
		var out *awss3.GetObjectOutput
		out, err = client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key)})
		if err != nil {
			return sample
		}
		h := sha256.New()
		sample.PayloadBytes, err = io.Copy(h, io.LimitReader(out.Body, int64(f.Bytes)+1))
		closeErr := out.Body.Close()
		err = errors.Join(err, closeErr)
		sample.SHA256 = hex.EncodeToString(h.Sum(nil))
		if err == nil && (out.ContentLength == nil || *out.ContentLength != int64(f.Bytes) || sample.PayloadBytes != int64(f.Bytes)) {
			sample.Error = "size_mismatch"
		} else if err == nil && sample.SHA256 != f.SHA256 {
			sample.Error = "sha256_mismatch"
		}
	}
	return sample
}

func listPhase(ctx context.Context, client *awss3.Client, bucket, phase string, r *Result, files []benchmarkdata.File) error {
	if ctx.Err() != nil {
		return ErrInterrupted
	}
	start := time.Now()
	expected := map[string]int64{}
	for _, f := range files {
		expected[r.Prefix+f.Path] = int64(f.Bytes)
	}
	seenKeys, seenTokens := map[string]bool{}, map[string]bool{}
	var token *string
	var samples []Sample
	var stopTrigger string
	// No page can be empty and truncated indefinitely, nor return unlimited extras.
	for page := 0; page <= len(r.Dataset.Files); page++ {
		if ctx.Err() != nil {
			return ErrInterrupted
		}
		t := time.Now()
		callCtx, cancel := context.WithTimeout(ctx, requestTimeout)
		out, err := client.ListObjectsV2(callCtx, &awss3.ListObjectsV2Input{Bucket: aws.String(bucket), Prefix: aws.String(r.Prefix), ContinuationToken: token, MaxKeys: aws.Int32(1000)})
		cancel()
		status, requestID := responseDiagnostics(err)
		s := Sample{Phase: phase, Operation: "ListObjectsV2", LatencyNS: time.Since(t).Nanoseconds(), Error: errorCode(err), HTTPStatus: status, RequestID: requestID}
		if err == nil {
			for _, object := range out.Contents {
				key := aws.ToString(object.Key)
				size, exists := expected[key]
				if !exists || seenKeys[key] || object.Size == nil || *object.Size != size {
					s.Error = "list_mismatch"
				}
				seenKeys[key] = true
			}
			if aws.ToBool(out.IsTruncated) {
				next := aws.ToString(out.NextContinuationToken)
				if next == "" || seenTokens[next] || page == len(r.Dataset.Files) {
					s.Error = "invalid_pagination"
				}
				seenTokens[next] = true
				token = out.NextContinuationToken
			} else {
				token = nil
				if len(seenKeys) != len(expected) {
					s.Error = "list_mismatch"
				}
			}
		}
		samples = append(samples, s)
		if s.Error != "" || token == nil {
			if s.Error != "" {
				stopTrigger = sanitizeStopTrigger(s.Error)
			}
			break
		}
	}
	return finishPhaseWithStats(r, phase, start, samples, len(samples), len(samples), 0, stopTrigger)
}

func finishPhase(r *Result, name string, start time.Time, samples []Sample) error {
	return finishPhaseWithStats(r, name, start, samples, len(samples), len(samples), 0, "")
}

func finishPhaseWithStats(r *Result, name string, start time.Time, samples []Sample, planned, dispatched, skipped int, stopTrigger string) error {
	p := Phase{
		Name:        name,
		Samples:     len(samples),
		Planned:     planned,
		Dispatched:  dispatched,
		Completed:   len(samples),
		Skipped:     skipped,
		StopTrigger: stopTrigger,
		ElapsedNS:   time.Since(start).Nanoseconds(),
	}
	latencies := make([]int64, 0, len(samples))
	for _, s := range samples {
		latencies = append(latencies, s.LatencyNS)
		if s.Error != "" {
			p.Failed++
		} else {
			p.SuccessfulBytes += s.PayloadBytes
		}
	}
	slices.Sort(latencies)
	if len(latencies) != 0 {
		p.P50NS, p.P95NS, p.MaxNS = latencies[int(math.Ceil(.5*float64(len(latencies))))-1], latencies[int(math.Ceil(.95*float64(len(latencies))))-1], latencies[len(latencies)-1]
	}
	if p.ElapsedNS > 0 {
		p.BytesPerSecond = float64(p.SuccessfulBytes) * 1e9 / float64(p.ElapsedNS)
	}
	if stopTrigger != "" && r.StopTrigger == "" {
		r.StopTrigger = stopTrigger
	}
	r.Samples = append(r.Samples, samples...)
	r.Phases = append(r.Phases, p)
	if p.Failed != 0 {
		return errors.New("phase failed; see sample error codes in result.json")
	}
	return nil
}

func sanitizeStopTrigger(trigger string) string {
	if trigger == "" {
		return ""
	}
	if safeCode.MatchString(trigger) {
		return trigger
	}
	return "operation_failed"
}

func saveResult(out string, r Result) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return errors.New("cannot encode run artifact")
	}
	path := filepath.Join(out, "result.json.tmp")
	if err = os.WriteFile(path, append(b, '\n'), 0600); err == nil {
		err = os.Rename(path, filepath.Join(out, "result.json"))
	}
	if err != nil {
		return errors.New("cannot persist run artifact")
	}
	return nil
}

func clientVersions() map[string]string {
	v := map[string]string{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
		"logical_cpus": fmt.Sprint(runtime.NumCPU()), "gomaxprocs": fmt.Sprint(runtime.GOMAXPROCS(0)),
		"protocol": "S3 path-style / single PutObject / SHA256 header", "sdk_max_attempts": "1", "timeout": requestTimeout.String(),
		"http_decompression": "disabled", "vcs_revision": "unknown"}
	if path, err := os.Executable(); err == nil {
		if f, err := os.Open(path); err == nil {
			h := sha256.New()
			if _, err := io.Copy(h, f); err == nil {
				v["executable_sha256"] = hex.EncodeToString(h.Sum(nil))
			}
			f.Close()
		}
	}
	if b, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range b.Deps {
			if dep.Path == "github.com/aws/aws-sdk-go-v2" || dep.Path == "github.com/aws/aws-sdk-go-v2/service/s3" {
				v[dep.Path] = dep.Version
			}
		}
		for _, s := range b.Settings {
			if s.Key == "vcs.revision" || s.Key == "vcs.modified" {
				v[s.Key] = s.Value
				if s.Key == "vcs.revision" {
					v["vcs_revision"] = s.Value
				}
			}
		}
	}
	return v
}

func uploadMultipart(ctx context.Context, client *awss3.Client, root *os.Root, c Config, key, phase string, f benchmarkdata.File, failFast bool) (sample Sample) {
	start := time.Now()
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	sample = Sample{Phase: phase, Operation: "CompleteMultipartUpload", File: f.Path, Group: f.Group, DispatchedType: "multipart"}
	var err error
	defer func() {
		sample.LatencyNS = time.Since(start).Nanoseconds()
		if err != nil {
			sample.HTTPStatus, sample.RequestID = responseDiagnostics(err)
			if sample.Error == "" {
				sample.Error = errorCode(err)
			}
			if failFast {
				status, _ := responseDiagnostics(err)
				if status == 0 || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
					sample.Indeterminate = true
					sample.Reconciliation = "needs_reconciliation"
				}
			}
		}
	}()

	file, e := root.Open(f.Path)
	if e != nil {
		err = e
		return sample
	}
	defer file.Close()

	sc := types.StorageClassStandard
	if c.StorageClass != "" {
		sc = types.StorageClass(c.StorageClass)
	}
	createInput := &awss3.CreateMultipartUploadInput{
		Bucket:            aws.String(c.Bucket),
		Key:               aws.String(key),
		ContentType:       aws.String("application/octet-stream"),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		StorageClass:      sc,
	}
	if c.KMSKeyID != "" {
		createInput.ServerSideEncryption = types.ServerSideEncryptionAwsKms
		createInput.SSEKMSKeyId = aws.String(c.KMSKeyID)
	}

	createOut, e := client.CreateMultipartUpload(ctx, createInput)
	if e != nil {
		err = e
		return sample
	}
	uploadID := createOut.UploadId

	abort := func() {
		abortCtx, abortCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer abortCancel()
		_, _ = client.AbortMultipartUpload(abortCtx, &awss3.AbortMultipartUploadInput{
			Bucket:   aws.String(c.Bucket),
			Key:      aws.String(key),
			UploadId: uploadID,
		})
	}

	const partSize = 5 * 1024 * 1024 // 5 MiB minimum per part
	totalBytes := int64(f.Bytes)
	var completedParts []types.CompletedPart
	var partNumber int32 = 1
	offset := int64(0)

	for offset < totalBytes {
		remaining := totalBytes - offset
		curPartSize := int64(partSize)
		if curPartSize > remaining {
			curPartSize = remaining
		}

		partBuf := make([]byte, curPartSize)
		if _, e := io.ReadFull(file, partBuf); e != nil {
			abort()
			err = e
			return sample
		}

		partSum := sha256.Sum256(partBuf)
		partChecksum := base64.StdEncoding.EncodeToString(partSum[:])

		partInput := &awss3.UploadPartInput{
			Bucket:         aws.String(c.Bucket),
			Key:            aws.String(key),
			UploadId:       uploadID,
			PartNumber:     aws.Int32(partNumber),
			Body:           bytes.NewReader(partBuf),
			ContentLength:  aws.Int64(curPartSize),
			ChecksumSHA256: aws.String(partChecksum),
		}
		partOut, e := client.UploadPart(ctx, partInput)
		if e != nil {
			abort()
			err = e
			return sample
		}

		completedParts = append(completedParts, types.CompletedPart{
			PartNumber:     aws.Int32(partNumber),
			ETag:           partOut.ETag,
			ChecksumSHA256: partOut.ChecksumSHA256,
		})

		offset += curPartSize
		partNumber++
	}

	completeInput := &awss3.CompleteMultipartUploadInput{
		Bucket:   aws.String(c.Bucket),
		Key:      aws.String(key),
		UploadId: uploadID,
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	}
	_, e = client.CompleteMultipartUpload(ctx, completeInput)
	if e != nil {
		abort()
		err = e
		return sample
	}

	sample.PayloadBytes = totalBytes
	sample.PartCount = len(completedParts)
	return sample
}

func overwritePhase(ctx context.Context, client *awss3.Client, root *os.Root, c Config, o Options, r *Result) error {
	byGroup := map[string][]benchmarkdata.File{}
	for _, f := range r.Dataset.Files {
		byGroup[f.Group] = append(byGroup[f.Group], f)
	}

	groups := make([]string, 0, len(byGroup))
	for g := range byGroup {
		groups = append(groups, g)
	}
	slices.Sort(groups)

	var putSamples, headSamples, getSamples []Sample
	latestPerGroup := map[string]benchmarkdata.File{}
	start := time.Now()

	for _, group := range groups {
		files := byGroup[group]
		slices.SortFunc(files, func(a, b benchmarkdata.File) int {
			return a.Version - b.Version
		})
		ext := filepath.Ext(files[0].Path)
		key := r.Prefix + group + ext

		for _, f := range files {
			if ctx.Err() != nil {
				return ErrInterrupted
			}

			// PUT
			sPut := fileOperation(ctx, client, root, c, key, "put", f, o.FailFast, false)
			sPut.Phase = "overwrite-put"
			putSamples = append(putSamples, sPut)
			r.HistoricalBytes += int64(f.Bytes)
			if sPut.Error != "" {
				_ = finishPhase(r, "overwrite-put", start, putSamples)
				return errors.New("overwrite put failed: " + sPut.Error)
			}

			// HEAD
			sHead := fileOperation(ctx, client, root, c, key, "head", f, o.FailFast, false)
			sHead.Phase = "overwrite-head"
			headSamples = append(headSamples, sHead)
			if sHead.Error != "" {
				_ = finishPhase(r, "overwrite-head", start, headSamples)
				return errors.New("overwrite head failed: " + sHead.Error)
			}

			// GET verify
			sGet := fileOperation(ctx, client, root, c, key, "verify-get", f, o.FailFast, false)
			sGet.Phase = "overwrite-get"
			getSamples = append(getSamples, sGet)
			if sGet.Error != "" {
				_ = finishPhase(r, "overwrite-get", start, getSamples)
				return errors.New("overwrite get integrity failed: " + sGet.Error)
			}

			latestPerGroup[group] = f
		}
	}

	if err := finishPhase(r, "overwrite-put", start, putSamples); err != nil {
		return err
	}
	if err := finishPhase(r, "overwrite-head", start, headSamples); err != nil {
		return err
	}
	if err := finishPhase(r, "overwrite-get", start, getSamples); err != nil {
		return err
	}

	var liveFiles []benchmarkdata.File
	var liveBytes int64
	for _, group := range groups {
		f := latestPerGroup[group]
		ext := filepath.Ext(f.Path)
		liveFiles = append(liveFiles, benchmarkdata.File{
			Path:    group + ext,
			Bytes:   f.Bytes,
			Group:   group,
			Version: f.Version,
			SHA256:  f.SHA256,
		})
		liveBytes += int64(f.Bytes)
	}
	r.LiveLogicalBytes = liveBytes

	// Verify via list: only the live objects should be in prefix
	if err := listPhase(ctx, client, c.Bucket, "list", r, liveFiles); err != nil {
		return err
	}

	if o.RetainObjects {
		r.Cleanup = "retained_owned_prefix_for_observation"
		return nil
	}

	// Delete live files
	startDel := time.Now()
	var delSamples []Sample
	for _, group := range groups {
		f := latestPerGroup[group]
		ext := filepath.Ext(f.Path)
		key := r.Prefix + group + ext
		sDel := fileOperation(ctx, client, root, c, key, "delete", f, o.FailFast, false)
		delSamples = append(delSamples, sDel)
	}
	if err := finishPhase(r, "delete", startDel, delSamples); err != nil {
		return err
	}

	if err := listPhase(ctx, client, c.Bucket, "after-delete-list", r, nil); err != nil {
		return err
	}
	r.Cleanup = "logical_delete_verified_physical_retention_unknown"
	return nil
}

func edgeClonePhase(ctx context.Context, client1 *awss3.Client, root *os.Root, c1 Config, o Options, r *Result, traffic *bodyTraffic) error {
	if o.SecondConfig == "" {
		return errors.New("edge-clone requires --second-config for independent destination domain")
	}
	b, err := secretfile.Read(o.SecondConfig, 32<<10)
	if err != nil {
		return fmt.Errorf("read second-config failed: %w", err)
	}
	var cDest Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&cDest); err != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid second-config JSON or unknown field")
	}

	if c1.DedupDomain == "" || cDest.DedupDomain == "" {
		return errors.New("edge-clone requires dedup_domain declared in both source and destination configs")
	}
	if c1.Bucket == cDest.Bucket {
		return errors.New("edge-clone requires distinct buckets for source and destination")
	}
	if c1.CredentialsFile == cDest.CredentialsFile {
		return errors.New("edge-clone requires distinct credentials files for source and destination")
	}
	// Different file paths can still hold the same DSG access key.
	sourceSecrets, sourceErr := secretfile.Read(c1.CredentialsFile, 16<<10)
	destSecrets, destErr := secretfile.Read(cDest.CredentialsFile, 16<<10)
	var sourceIdentity, destIdentity credentials
	if sourceErr != nil || destErr != nil || decode(sourceSecrets, &sourceIdentity) != nil || decode(destSecrets, &destIdentity) != nil || sourceIdentity.AccessKeyID == "" || destIdentity.AccessKeyID == "" {
		return errors.New("edge-clone cannot verify both credential identities")
	}
	if sourceIdentity.AccessKeyID == destIdentity.AccessKeyID {
		return errors.New("edge-clone requires distinct access key identities for source and destination")
	}
	if c1.DedupDomain == cDest.DedupDomain {
		return fmt.Errorf("edge-clone requires distinct source and destination dedup domains, both are %q", c1.DedupDomain)
	}

	trafficDest := &bodyTraffic{}
	clientDest, closeDest, err := newClient(cDest, o.Concurrency, trafficDest)
	if err != nil {
		return fmt.Errorf("destination client initialization failed: %w", err)
	}
	defer closeDest()

	switch o.Variant {
	case "clone-download":
		if o.SourcePrefix == "" {
			return errors.New("clone-download requires non-empty --source-prefix")
		}
		// Preflight destination bucket connectivity and runtime domain verification
		if _, err := clientDest.ListObjectsV2(ctx, &awss3.ListObjectsV2Input{
			Bucket:  aws.String(cDest.Bucket),
			MaxKeys: aws.Int32(1),
		}); err != nil {
			return fmt.Errorf("clone-download destination preflight failed for bucket %s: %w", cDest.Bucket, err)
		}

		var totalBytes int64
		pipelineHasher := sha256.New()
		var getSamples []Sample
		startGet := time.Now()

		for _, f := range r.Dataset.Files {
			srcKey := o.SourcePrefix + f.Path
			startF := time.Now()
			getOut, err := client1.GetObject(ctx, &awss3.GetObjectInput{
				Bucket: aws.String(c1.Bucket),
				Key:    aws.String(srcKey),
			})
			status, reqID := responseDiagnostics(err)
			if err != nil {
				getSamples = append(getSamples, Sample{
					Phase: "clone-download-get", Operation: "GetObject", File: f.Path, Group: f.Group,
					HTTPStatus: status, RequestID: reqID, Error: errorCode(err),
					LatencyNS: time.Since(startF).Nanoseconds(),
				})
				_ = finishPhase(r, "clone-download-get", startGet, getSamples)
				return fmt.Errorf("clone download GET failed for %s: %w", f.Path, err)
			}
			hasher := sha256.New()
			buf := make([]byte, 64*1024)
			mw := io.MultiWriter(hasher, pipelineHasher)
			n, err := io.CopyBuffer(mw, getOut.Body, buf)
			getOut.Body.Close()
			if err != nil {
				return fmt.Errorf("read source GET stream failed for %s: %w", f.Path, err)
			}
			if n != int64(f.Bytes) {
				return fmt.Errorf("source GET size mismatch for %s: got %d, want %d", f.Path, n, f.Bytes)
			}
			hexSum := hex.EncodeToString(hasher.Sum(nil))
			if hexSum != f.SHA256 {
				return fmt.Errorf("source GET integrity mismatch for %s: got %s, want %s", f.Path, hexSum, f.SHA256)
			}
			getSamples = append(getSamples, Sample{
				Phase: "clone-download-get", Operation: "GetObject", File: f.Path, Group: f.Group,
				PayloadBytes: n, SHA256: hexSum, HTTPStatus: 200,
				LatencyNS: time.Since(startF).Nanoseconds(),
			})
			totalBytes += n
		}
		if err := finishPhase(r, "clone-download-get", startGet, getSamples); err != nil {
			return err
		}
		r.CloneReceipt = &CloneReceipt{
			SourceDomain:      c1.DedupDomain,
			DestinationDomain: cDest.DedupDomain,
			SourcePrefix:      o.SourcePrefix,
			DestinationPrefix: r.Prefix,
			FilesCloned:       len(r.Dataset.Files),
			TotalBytesCloned:  totalBytes,
			ReceiptSHA256:     hex.EncodeToString(pipelineHasher.Sum(nil)),
		}
		r.Cleanup = "edge_clone_download_verified"

	case "clone-upload":
		if o.SourcePrefix == "" {
			return errors.New("clone-upload requires non-empty --source-prefix pointing to source domain prefix")
		}
		var totalBytes int64
		pipelineHasher := sha256.New()
		var getSamples []Sample
		var putSamples []Sample
		startGet := time.Now()
		startPut := time.Now()

		for _, f := range r.Dataset.Files {
			srcKey := o.SourcePrefix + f.Path
			destKey := r.Prefix + f.Path

			// 1. GET from source domain (client1)
			startFGet := time.Now()
			getOut, err := client1.GetObject(ctx, &awss3.GetObjectInput{
				Bucket: aws.String(c1.Bucket),
				Key:    aws.String(srcKey),
			})
			status, reqID := responseDiagnostics(err)
			if err != nil {
				getSamples = append(getSamples, Sample{
					Phase: "clone-source-get", Operation: "GetObject", File: f.Path, Group: f.Group,
					HTTPStatus: status, RequestID: reqID, Error: errorCode(err),
					LatencyNS: time.Since(startFGet).Nanoseconds(),
				})
				_ = finishPhase(r, "clone-source-get", startGet, getSamples)
				return fmt.Errorf("clone source GET failed for %s: %w", f.Path, err)
			}

			// Bounded streaming: stream GET body directly into PUT body via pipe using 64 KiB buffer
			pr, pw := io.Pipe()
			copyErrCh := make(chan error, 1)

			go func(file benchmarkdata.File, srcBody io.ReadCloser) {
				defer srcBody.Close()
				hasher := sha256.New()
				mw := io.MultiWriter(pw, hasher, pipelineHasher)
				buf := make([]byte, 64*1024)
				n, copyErr := io.CopyBuffer(mw, srcBody, buf)
				if copyErr != nil {
					_ = pw.CloseWithError(fmt.Errorf("read source GET stream failed for %s: %w", file.Path, copyErr))
					copyErrCh <- copyErr
					return
				}
				if n != int64(file.Bytes) {
					errSize := fmt.Errorf("source GET size mismatch for %s: got %d, want %d", file.Path, n, file.Bytes)
					_ = pw.CloseWithError(errSize)
					copyErrCh <- errSize
					return
				}
				hexSum := hex.EncodeToString(hasher.Sum(nil))
				if hexSum != file.SHA256 {
					errSha := fmt.Errorf("source GET integrity mismatch for %s: got %s, want %s", file.Path, hexSum, file.SHA256)
					_ = pw.CloseWithError(errSha)
					copyErrCh <- errSha
					return
				}
				_ = pw.Close()
				copyErrCh <- nil
			}(f, getOut.Body)

			// 2. PUT directly into destination domain (clientDest) with bytes from source GET
			startFPut := time.Now()
			sc := types.StorageClassStandard
			if cDest.StorageClass != "" {
				sc = types.StorageClass(cDest.StorageClass)
			}
			expectedSHABytes, _ := hex.DecodeString(f.SHA256)
			putInput := &awss3.PutObjectInput{
				Bucket:            aws.String(cDest.Bucket),
				Key:               aws.String(destKey),
				Body:              pr,
				ContentLength:     aws.Int64(int64(f.Bytes)),
				StorageClass:      sc,
				ContentType:       aws.String("application/octet-stream"),
				ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
				ChecksumSHA256:    aws.String(base64.StdEncoding.EncodeToString(expectedSHABytes)),
			}
			if cDest.KMSKeyID != "" {
				putInput.ServerSideEncryption = types.ServerSideEncryptionAwsKms
				putInput.SSEKMSKeyId = aws.String(cDest.KMSKeyID)
			}
			putCtx := WithPayloadHash(ctx, f.SHA256)
			_, putErr := clientDest.PutObject(putCtx, putInput)
			if putErr != nil {
				// The remote endpoint may reject PUT before consuming the
				// request body. Closing the reader wakes a producer blocked
				// on the pipe, instead of deadlocking on copyErrCh.
				_ = pr.CloseWithError(putErr)
			}
			copyErr := <-copyErrCh
			putStatus, putReqID := responseDiagnostics(putErr)

			if putErr != nil {
				putSamples = append(putSamples, Sample{
					Phase: "clone-dest-put", Operation: "PutObject", File: f.Path, Group: f.Group,
					HTTPStatus: putStatus, RequestID: putReqID, Error: errorCode(putErr),
					LatencyNS: time.Since(startFPut).Nanoseconds(),
				})
				_ = finishPhase(r, "clone-source-get", startGet, getSamples)
				_ = finishPhase(r, "clone-dest-put", startPut, putSamples)
				return fmt.Errorf("clone destination PUT failed for %s: %w", f.Path, putErr)
			}
			if copyErr != nil {
				return copyErr
			}

			getSamples = append(getSamples, Sample{
				Phase: "clone-source-get", Operation: "GetObject", File: f.Path, Group: f.Group,
				PayloadBytes: int64(f.Bytes), SHA256: f.SHA256, HTTPStatus: status,
				LatencyNS: time.Since(startFGet).Nanoseconds(),
			})
			putSamples = append(putSamples, Sample{
				Phase: "clone-dest-put", Operation: "PutObject", File: f.Path, Group: f.Group,
				PayloadBytes: int64(f.Bytes), SHA256: f.SHA256, HTTPStatus: putStatus,
				LatencyNS: time.Since(startFPut).Nanoseconds(),
			})
			totalBytes += int64(f.Bytes)
		}
		if err := finishPhase(r, "clone-source-get", startGet, getSamples); err != nil {
			return err
		}
		if err := finishPhase(r, "clone-dest-put", startPut, putSamples); err != nil {
			return err
		}

		// 3. Verify in destination: HEAD and GET
		oDest := o
		oDest.SourcePrefix = ""
		if err := filePhase(ctx, clientDest, root, cDest, oDest, "head", r); err != nil {
			return fmt.Errorf("clone destination head failed: %w", err)
		}
		if err := filePhase(ctx, clientDest, root, cDest, oDest, "verify-get", r); err != nil {
			return fmt.Errorf("clone destination get integrity failed: %w", err)
		}

		r.CloneReceipt = &CloneReceipt{
			SourceDomain:      c1.DedupDomain,
			DestinationDomain: cDest.DedupDomain,
			SourcePrefix:      o.SourcePrefix,
			DestinationPrefix: r.Prefix,
			FilesCloned:       len(r.Dataset.Files),
			TotalBytesCloned:  totalBytes,
			ReceiptSHA256:     hex.EncodeToString(pipelineHasher.Sum(nil)),
		}
		if o.RetainObjects {
			r.Cleanup = "retained_owned_prefix_for_observation"
		} else {
			if err := filePhase(ctx, clientDest, root, cDest, oDest, "delete", r); err != nil {
				return err
			}
			r.Cleanup = "edge_clone_upload_verified"
		}
	default:
		return errors.New("unknown edge-clone variant: " + o.Variant)
	}

	// 4. Runtime domain mapping verification for both source and destination
	obsSrc := traffic.getObservedDomain()
	obsDest := trafficDest.getObservedDomain()
	if obsSrc == "" {
		return errors.New("edge-clone source runtime domain missing: backend did not return X-Dsg-Dedup-Domain")
	}
	if obsDest == "" {
		return errors.New("edge-clone destination runtime domain missing: backend did not return X-Dsg-Dedup-Domain")
	}
	if obsSrc != c1.DedupDomain {
		return fmt.Errorf("edge-clone source runtime domain binding mismatch: backend returned domain %q, expected %q", obsSrc, c1.DedupDomain)
	}
	if obsDest != cDest.DedupDomain {
		return fmt.Errorf("edge-clone destination runtime domain binding mismatch: backend returned domain %q, expected %q", obsDest, cDest.DedupDomain)
	}
	if obsSrc == obsDest {
		return fmt.Errorf("edge-clone runtime collision: both source and destination bound to identical domain %q", obsSrc)
	}
	r.Tenant1RuntimeDomain = obsSrc
	r.Tenant2RuntimeDomain = obsDest
	r.DomainContrastVerified = true
	r.DomainPhysicalIsolation = true

	return nil
}

func domainPhase(ctx context.Context, client1 *awss3.Client, root *os.Root, c1 Config, o Options, r *Result, traffic1 *bodyTraffic) error {
	b, err := secretfile.Read(o.SecondConfig, 32<<10)
	if err != nil {
		return fmt.Errorf("read second-config failed: %w", err)
	}
	var c2 Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&c2); err != nil || d.Decode(new(any)) != io.EOF {
		return errors.New("invalid second-config JSON or unknown field")
	}

	if c1.DedupDomain == "" || c2.DedupDomain == "" {
		return errors.New("domain scenario requires dedup_domain declared in both configs")
	}
	if c1.Bucket == c2.Bucket {
		return errors.New("domain scenario requires distinct buckets for tenant 1 and tenant 2")
	}
	if c1.CredentialsFile == c2.CredentialsFile {
		return errors.New("domain scenario requires distinct credentials files for tenant 1 and tenant 2")
	}

	switch o.Variant {
	case "shared-default":
		if c1.DedupDomain != c2.DedupDomain {
			return fmt.Errorf("shared-default requires identical tenant dedup domains, got %q and %q", c1.DedupDomain, c2.DedupDomain)
		}
	case "separated":
		if c1.DedupDomain == c2.DedupDomain {
			return fmt.Errorf("separated variant requires distinct tenant dedup domains, both are %q", c1.DedupDomain)
		}
	default:
		return errors.New("unknown domain variant: " + o.Variant)
	}

	r.Tenant1Domain = c1.DedupDomain
	r.Tenant2Domain = c2.DedupDomain

	traffic2 := &bodyTraffic{}
	client2, closeClient2, err := newClient(c2, o.Concurrency, traffic2)
	if err != nil {
		return fmt.Errorf("second client initialization failed: %w", err)
	}
	defer closeClient2()

	// 1. Tenant 1 PUT, HEAD and GET verify all files into Tenant 1's bucket
	if err := filePhase(ctx, client1, root, c1, o, "put", r); err != nil {
		return fmt.Errorf("tenant 1 PUT phase failed: %w", err)
	}
	if err := filePhase(ctx, client1, root, c1, o, "head", r); err != nil {
		return fmt.Errorf("tenant 1 HEAD phase failed: %w", err)
	}
	if err := filePhase(ctx, client1, root, c1, o, "verify-get", r); err != nil {
		return fmt.Errorf("tenant 1 GET integrity failed: %w", err)
	}

	// 2. Cross-Owner Auth: Tenant 2 attempts to read Tenant 1's object in Tenant 1's bucket.
	// It MUST be rejected with HTTP 403 Forbidden (AccessDenied).
	if len(r.Dataset.Files) > 0 {
		startCross := time.Now()
		testFile := r.Dataset.Files[0]
		targetKey := r.Prefix + testFile.Path
		_, err := client2.GetObject(ctx, &awss3.GetObjectInput{
			Bucket: aws.String(c1.Bucket),
			Key:    aws.String(targetKey),
		})
		status, reqID := responseDiagnostics(err)
		r.DomainCrossOwnerCheck = &Sample{
			Phase:      "cross-owner-auth-check",
			Operation:  "GetObject",
			File:       testFile.Path,
			HTTPStatus: status,
			RequestID:  reqID,
			LatencyNS:  time.Since(startCross).Nanoseconds(),
		}
		if err == nil {
			return errors.New("domain isolation failure: cross-owner GET succeeded without authorization grant")
		}
		if status != http.StatusForbidden {
			return fmt.Errorf("cross-owner GET must be rejected with HTTP 403 Forbidden, got %d (%v)", status, err)
		}
		r.DomainCrossOwnerRejected = true
	}

	// 3. Second Tenant Operations - ALWAYS preserve Tenant 2 evidence via defer
	prefix2 := "warmbench-tenant2/" + filepath.Base(r.Prefix) + "/"
	o2 := o
	o2.SourcePrefix = prefix2
	res2 := Result{Prefix: prefix2, Dataset: r.Dataset}
	defer func() {
		r.SecondTenantPhases = res2.Phases
		r.SecondTenantSamples = res2.Samples
		if res2.Cleanup != "" {
			r.SecondTenantCleanup = res2.Cleanup
		}
	}()

	if err := filePhase(ctx, client2, root, c2, o2, "put", &res2); err != nil {
		return fmt.Errorf("tenant 2 PUT phase failed: %w", err)
	}
	if err := filePhase(ctx, client2, root, c2, o2, "verify-get", &res2); err != nil {
		return fmt.Errorf("tenant 2 GET integrity failed: %w", err)
	}
	r.SecondTenantVerified = true

	// 4. Runtime domain mapping verification from observed response headers
	obs1 := traffic1.getObservedDomain()
	obs2 := traffic2.getObservedDomain()
	r.Tenant1RuntimeDomain = obs1
	r.Tenant2RuntimeDomain = obs2

	if obs1 == "" {
		return errors.New("domain scenario tenant 1 runtime domain missing: backend did not return X-Dsg-Dedup-Domain")
	}
	if obs2 == "" {
		return errors.New("domain scenario tenant 2 runtime domain missing: backend did not return X-Dsg-Dedup-Domain")
	}
	if obs1 != c1.DedupDomain {
		return fmt.Errorf("tenant 1 runtime domain binding mismatch: backend returned domain %q, expected %q", obs1, c1.DedupDomain)
	}
	if obs2 != c2.DedupDomain {
		return fmt.Errorf("tenant 2 runtime domain binding mismatch: backend returned domain %q, expected %q", obs2, c2.DedupDomain)
	}
	switch o.Variant {
	case "shared-default":
		if obs1 != obs2 {
			return fmt.Errorf("shared-default runtime domain mismatch: tenant 1 mapped to %q, tenant 2 mapped to %q", obs1, obs2)
		}
	case "separated":
		if obs1 == obs2 {
			return fmt.Errorf("separated variant runtime collision: backend mapped both tenants to identical domain %q", obs1)
		}
	}

	// Every successful PUT must supply the complete physical-counter pair;
	// a single debug-enabled response cannot certify an entire dataset.
	expectedPUTs := uint64(len(r.Dataset.Files))
	if traffic1.telemetryResponses.Load() != expectedPUTs || traffic2.telemetryResponses.Load() != expectedPUTs {
		return fmt.Errorf("domain scenario requires backend debug telemetry on every PUT (X-Dsg-Chunks-Dup and X-Dsg-Bytes-Stored): tenant 1 %d/%d, tenant 2 %d/%d; verify signed x-dsg-debug: 1 and debug permissions",
			traffic1.telemetryResponses.Load(), expectedPUTs, traffic2.telemetryResponses.Load(), expectedPUTs)
	}

	t1Dup := traffic1.chunksDup.Load()
	t2Dup := traffic2.chunksDup.Load()
	t2Stored := traffic2.bytesStored.Load()

	switch o.Variant {
	case "shared-default":
		// Inter-tenant deduplication: Tenant 2 uploads the same dataset that Tenant 1 uploaded into Tenant 1's bucket.
		// Since both tenants share the dedup domain, Tenant 2 must deduplicate against Tenant 1's already committed chunks.
		// Tenant 2 duplicate count must strictly exceed Tenant 1's intra-dataset duplicate baseline (t2Dup > t1Dup).
		if t2Dup <= t1Dup {
			return fmt.Errorf("shared-default domain isolation failure: cross-tenant deduplication was not observed on backend (tenant 1 dup=%d, tenant 2 dup=%d)", t1Dup, t2Dup)
		}
		r.DomainDedupObserved = true
	case "separated":
		// Physical isolation: Tenant 1 and Tenant 2 belong to distinct dedup domains.
		// Tenant 2 must NOT deduplicate against Tenant 1's chunks in another domain (t2Dup <= t1Dup).
		// Any excess duplicate chunks indicates cross-domain deduplication leakage.
		if t2Dup > t1Dup {
			return fmt.Errorf("separated domain isolation failure: cross-domain deduplication was observed on backend (tenant 1 dup=%d, tenant 2 dup=%d, excess=%d)", t1Dup, t2Dup, t2Dup-t1Dup)
		}
		// Tenant 2 must store physical bytes in its own separated domain
		if t2Stored == 0 {
			return errors.New("separated domain isolation failure: tenant 2 stored 0 physical bytes; cannot prove independent physical storage")
		}
		r.DomainPhysicalIsolation = true
	}
	r.DomainContrastVerified = true

	// Cleanup
	if o.RetainObjects {
		r.Cleanup = "retained_owned_prefix_for_observation"
		return nil
	}
	if err := filePhase(ctx, client1, root, c1, o, "delete", r); err != nil {
		return err
	}
	if err := filePhase(ctx, client2, root, c2, o2, "delete", &res2); err != nil {
		return err
	}
	res2.Cleanup = "logical_delete_verified"
	r.Cleanup = "domain_two_tenant_isolation_verified"
	return nil
}
