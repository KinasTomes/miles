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
	DomainCrossOwnerRejected bool                   `json:"domain_cross_owner_rejected,omitempty"`
	SecondTenantVerified     bool                   `json:"second_tenant_verified,omitempty"`
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
	root, manifest, raw, err := loadDataset(ctx, o.Manifest)
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
		if o.Variant == "clone-download" {
			if o.SourcePrefix == "" {
				return result, errors.New("clone-download requires non-empty --source-prefix")
			}
			if !strings.HasSuffix(o.SourcePrefix, "/") {
				o.SourcePrefix += "/"
			}
		}
		if err = edgeClonePhase(ctx, client, root, c, o, &result); err != nil {
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

func edgeClonePhase(ctx context.Context, client *awss3.Client, root *os.Root, c Config, o Options, r *Result) error {
	switch o.Variant {
	case "clone-download":
		if err := filePhase(ctx, client, root, c, o, "verify-get", r); err != nil {
			return err
		}
		r.Cleanup = "edge_clone_download_verified"
		return nil
	case "clone-upload":
		if err := filePhase(ctx, client, root, c, o, "put", r); err != nil {
			return err
		}
		if err := filePhase(ctx, client, root, c, o, "head", r); err != nil {
			return err
		}
		if err := filePhase(ctx, client, root, c, o, "verify-get", r); err != nil {
			return err
		}
		if o.RetainObjects {
			r.Cleanup = "retained_owned_prefix_for_observation"
			return nil
		}
		if err := filePhase(ctx, client, root, c, o, "delete", r); err != nil {
			return err
		}
		r.Cleanup = "edge_clone_upload_verified"
		return nil
	default:
		return errors.New("unknown edge-clone variant: " + o.Variant)
	}
}

func domainPhase(ctx context.Context, client1 *awss3.Client, root *os.Root, c1 Config, o Options, r *Result, traffic *bodyTraffic) error {
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

	client2, closeClient2, err := newClient(c2, o.Concurrency, traffic)
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
	// It MUST be rejected (HTTP 403 Forbidden / AccessDenied or 404).
	if len(r.Dataset.Files) > 0 {
		testFile := r.Dataset.Files[0]
		targetKey := r.Prefix + testFile.Path
		_, err := client2.GetObject(ctx, &awss3.GetObjectInput{
			Bucket: aws.String(c1.Bucket),
			Key:    aws.String(targetKey),
		})
		if err == nil {
			return errors.New("domain isolation failure: cross-owner GET succeeded without authorization grant")
		}
		status, _ := responseDiagnostics(err)
		if status != 403 && status != 404 {
			return fmt.Errorf("cross-owner GET returned unexpected error status %d: %w", status, err)
		}
		r.DomainCrossOwnerRejected = true
	}

	// 3. Tenant 2 PUT and GET verify all files into Tenant 2's bucket
	prefix2 := "warmbench-tenant2/" + filepath.Base(r.Prefix) + "/"
	o2 := o
	o2.SourcePrefix = prefix2
	res2 := *r
	res2.Prefix = prefix2
	if err := filePhase(ctx, client2, root, c2, o2, "put", &res2); err != nil {
		return fmt.Errorf("tenant 2 PUT phase failed: %w", err)
	}
	if err := filePhase(ctx, client2, root, c2, o2, "verify-get", &res2); err != nil {
		return fmt.Errorf("tenant 2 GET integrity failed: %w", err)
	}
	r.SecondTenantVerified = true

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
	r.Cleanup = "domain_two_tenant_isolation_verified"
	return nil
}
