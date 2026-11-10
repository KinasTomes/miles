// Package benchmark runs the same bounded, single-PUT S3 workload against both
// targets. It never provisions infrastructure or substitutes a fake Miles server.
package benchmark

import (
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
	"sync"
	"time"

	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Options struct {
	Manifest      string
	Out           string
	Concurrency   int
	RetainObjects bool          // Retain only this run prefix for external statistics; caller owns cleanup.
	ReadRepeats   int           // additional full GET passes, separate from initial verification
	FailFast      bool          // Halt dispatching new requests immediately on first error; in-flight requests drain.
	DrainTimeout  time.Duration // Emergency deadline for in-flight requests to drain after interruption (0 uses requestTimeout)
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
	SchemaVersion       int                    `json:"schema_version"`
	MVPThresholdApplied bool                   `json:"mvp_threshold_applied"`
	Status              string                 `json:"status"`
	Error               string                 `json:"error,omitempty"`
	Target              string                 `json:"target"`
	Region              string                 `json:"region"`
	StartedAt           time.Time              `json:"started_at"`
	FinishedAt          time.Time              `json:"finished_at"`
	Prefix              string                 `json:"prefix"`
	ManifestSHA256      string                 `json:"manifest_sha256"`
	Dataset             benchmarkdata.Manifest `json:"dataset"`
	Concurrency         int                    `json:"concurrency"`
	ReadRepeats         int                    `json:"read_repeats"`
	FailFast            bool                   `json:"fail_fast"`
	StopTrigger         string                 `json:"stop_trigger,omitempty"`
	NeedsReconciliation bool                   `json:"needs_reconciliation,omitempty"`
	DeclaredEnvironment Environment            `json:"declared_environment"`
	Client              map[string]string      `json:"client"`
	Phases              []Phase                `json:"phases"`
	Samples             []Sample               `json:"samples"`
	Cleanup             string                 `json:"cleanup"`
	HTTPBodyBytes       map[string]uint64      `json:"http_body_bytes"`
	Limitations         []string               `json:"limitations"`
}

// Run writes a private artifact even on request failure. A fresh random prefix
// owns only this run's keys; failures retain them for diagnosis, never bulk delete.
func Run(ctx context.Context, c Config, o Options) (result Result, err error) {
	if ctx == nil || o.Concurrency < 1 || o.Concurrency > 64 || o.ReadRepeats < 0 || o.ReadRepeats > 10 || o.Manifest == "" || o.Out == "" {
		return result, errors.New("require manifest, new out directory, concurrency 1..64 and read-repeats 0..10")
	}
	root, manifest, raw, err := loadDataset(ctx, o.Manifest)
	if err != nil {
		return result, err
	}
	defer root.Close()
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
		FailFast: o.FailFast,
		Cleanup:  "not_started", Phases: []Phase{}, Samples: []Sample{},
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

	if !o.FailFast {
		jobs := make(chan int)
		for i := 0; i < min(o.Concurrency, total); i++ {
			workers.Go(func() {
				for index := range jobs {
					entry := r.Dataset.Files[index]
					samples[index] = fileOperation(ctx, client, root, c, r.Prefix+entry.Path, phase, entry, false)
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
				s := fileOperation(drainCtx, client, root, c, r.Prefix+entry.Path, phase, entry, true)
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

func fileOperation(ctx context.Context, client *awss3.Client, root *os.Root, c Config, key, phase string, f benchmarkdata.File, failFast bool) (sample Sample) {
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
		sample.Operation = "PutObject"
		var body *os.File
		body, err = root.Open(f.Path)
		if err != nil {
			return sample
		}
		defer body.Close()
		digest, _ := hex.DecodeString(f.SHA256)
		input := &awss3.PutObjectInput{Bucket: aws.String(c.Bucket), Key: aws.String(key), Body: body,
			ContentLength: aws.Int64(int64(f.Bytes)), ContentType: aws.String("application/octet-stream"),
			ChecksumSHA256: aws.String(base64.StdEncoding.EncodeToString(digest)), StorageClass: types.StorageClassStandard}
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
