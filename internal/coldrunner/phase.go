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
	"sync/atomic"
	"time"

	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/benchmarknative"
	"github.com/KinasTomes/miles/internal/secretfile"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var Phases = map[string]bool{"inspect": true, "read": true, "restore-request": true, "restore-status": true,
	"put-cold": true, "part-cold": true, "put-restored": true, "part-restored": true}

type PhaseOptions struct {
	Phase, Manifest, Out, NativeConfig, AWSCredentialsFile, SourceSnapshot, File, Variant string
	// Local fixtures inject native observations; the production CLI always uses
	// the installation-bound AWS provider and trusted Chrony.
	Capture func(context.Context) (Snapshot, error)
}
type RequestReceipt struct {
	Operation, Key string
	Status         int
	UploadID, ETag string `json:",omitempty"`
}
type PhaseResult struct {
	SchemaVersion   int              `json:"schema_version"`
	Scope           string           `json:"scope"`
	Phase           string           `json:"phase"`
	Status          string           `json:"status"`
	StartedAt       string           `json:"started_at"`
	FinishedAt      string           `json:"finished_at"`
	ManifestSHA256  string           `json:"manifest_sha256"`
	Probes          []string         `json:"probes"`
	Snapshot        *Snapshot        `json:"snapshot,omitempty"`
	Requests        []RequestReceipt `json:"requests"`
	ReadVerified    bool             `json:"read_verified"`
	RevivalVerified bool             `json:"revival_verified"`
	IRReuseVerified bool             `json:"ir_reuse_verified"`
	Error           string           `json:"error,omitempty"`
}

func statusRecorder(value *atomic.Int32) func(*awss3.Options) {
	return func(o *awss3.Options) {
		o.APIOptions = append(o.APIOptions, func(stack *middleware.Stack) error {
			return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc("cold-native-response", func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (middleware.DeserializeOutput, middleware.Metadata, error) {
				out, meta, err := next.HandleDeserialize(ctx, in)
				if response, ok := out.RawResponse.(*smithyhttp.Response); ok {
					value.Store(int32(response.StatusCode))
				}
				return out, meta, err
			}), middleware.After)
		})
	}
}

func saveJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0600)
}

// RunPhase is one bounded operation. Repeated status calls are read-only;
// mutations never replay an existing output directory or an uncertain receipt.
func RunPhase(ctx context.Context, cfg benchmark.Config, opt PhaseOptions) (res *PhaseResult, err error) {
	res = &PhaseResult{SchemaVersion: 1, Scope: "cold-lifecycle-phase", Phase: opt.Phase, Status: "failed", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if !Phases[opt.Phase] || opt.Variant != "ir" && opt.Variant != "flexible" {
		return res, s.ErrInvalid
	}
	if err = os.Mkdir(opt.Out, 0700); err != nil {
		return res, err
	}
	defer func() {
		res.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err != nil {
			res.Error = err.Error()
		}
		err = errors.Join(err, saveJSON(filepath.Join(opt.Out, "result.json"), res))
	}()
	root, manifest, raw, err := benchmark.LoadDataset(ctx, opt.Manifest)
	if err != nil {
		return res, err
	}
	defer root.Close()
	sum := sha256.Sum256(raw)
	res.ManifestSHA256 = hex.EncodeToString(sum[:])
	var baseline Snapshot
	if opt.SourceSnapshot != "" {
		data, e := secretfile.Read(opt.SourceSnapshot, 32<<20)
		if e != nil {
			return res, e
		}
		if json.Unmarshal(data, &baseline) != nil {
			return res, errors.New("invalid source snapshot")
		}
	}
	capture := opt.Capture
	if opt.NativeConfig != "" {
		if capture != nil {
			return res, errors.New("cannot mix native config and injected observations")
		}
		binding, e := benchmarknative.Read(opt.NativeConfig)
		if e != nil {
			return res, e
		}
		if binding.LogicalBucket != cfg.Bucket {
			return res, errors.New("logical bucket differs")
		}
		if opt.SourceSnapshot != "" && !SameBinding(binding, baseline.Binding) {
			return res, errors.New("Cold resume binding differs")
		}
		retained := []s.PackRef{}
		for _, p := range baseline.Packs {
			retained = append(retained, p.Ref)
		}
		capture, e = nativeSnapshot(ctx, binding, opt.AWSCredentialsFile, &manifest, retained)
		if e != nil {
			return res, e
		}
	}
	if capture == nil {
		return res, errors.New("native Cold observer required")
	}
	before, err := capture(ctx)
	if err != nil {
		return res, err
	}
	res.Snapshot = &before
	if opt.SourceSnapshot != "" && !SameBinding(before.Binding, baseline.Binding) {
		return res, errors.New("Cold snapshot belongs to another trial")
	}
	if err = saveJSON(filepath.Join(opt.Out, "snapshot.json"), before); err != nil {
		return res, err
	}
	current, err := currentPhysical(before)
	if err != nil {
		return res, err
	}
	res.Probes, err = SelectProbes(&manifest, before)
	if err != nil {
		return res, err
	}
	if opt.Phase == "inspect" {
		res.Status = "observed"
		return res, nil
	}
	if opt.Phase == "restore-status" {
		if opt.Variant != "flexible" {
			return res, errors.New("IR must not request or await restore")
		}
		res.Status = "ready"
		for _, p := range current {
			metadata, ok := before.Pack(p.Ref, false)
			if !ok {
				return res, s.ErrNotFound
			}
			if p.Class == s.Glacier && (p.State != "Restored" || metadata.State != "Restored" ||
				p.RestoredUntil.Before(before.Bounds.Upper.Add(4*time.Minute)) || metadata.RestoredUntil.Before(before.Bounds.Upper.Add(4*time.Minute))) {
				res.Status = "waiting"
			}
		}
		return res, nil
	}
	client, cleanup, err := benchmark.NewClient(cfg, 1)
	if err != nil {
		return res, err
	}
	defer cleanup()
	var status atomic.Int32
	record := statusRecorder(&status)
	read := func(key, path string) error {
		var fileSHA string
		var size int
		for _, f := range manifest.Files {
			if f.Path == path {
				fileSHA = f.SHA256
				size = f.Bytes
			}
		}
		request, cancel := context.WithTimeout(ctx, 120*time.Second)
		defer cancel()
		out, e := client.GetObject(request, &awss3.GetObjectInput{Bucket: &cfg.Bucket, Key: &key}, record)
		res.Requests = append(res.Requests, RequestReceipt{Operation: "GetObject", Key: key, Status: int(status.Load())})
		if e != nil {
			return e
		}
		defer out.Body.Close()
		if status.Load() != 200 {
			return errors.New("full GET did not return HTTP 200")
		}
		hash := sha256.New()
		n, e := io.Copy(hash, out.Body)
		if e != nil || n != int64(size) || hex.EncodeToString(hash.Sum(nil)) != fileSHA {
			return errors.New("Cold full GET size/SHA256 mismatch")
		}
		return nil
	}
	if opt.Phase == "read" {
		for _, p := range current {
			if p.Class == s.Glacier && (p.State != "Restored" || p.RestoredUntil.Before(before.Bounds.Upper.Add(4*time.Minute))) {
				return res, errors.New("restore incomplete/expiry too close; status may be resumed later")
			}
		}
		for _, file := range manifest.Files {
			if err = read(file.Path, file.Path); err != nil {
				return res, err
			}
		}
		res.ReadVerified = true
		res.Status = "passed"
		return res, nil
	}
	if opt.Phase == "restore-request" {
		if opt.Variant != "flexible" {
			return res, errors.New("IR restore is forbidden")
		}
		requested := map[s.PackRef]bool{}
		for i, file := range manifest.Files {
			archive := false
			for _, key := range before.Files[file.Path] {
				chunk, _ := before.Chunk(key)
				p, _ := before.Pack(chunk.Location.Pack, true)
				if p.Class == s.Glacier && !requested[p.Ref] {
					archive = true
				}
			}
			if !archive {
				continue
			}
			for _, k := range before.Files[file.Path] {
				c, _ := before.Chunk(k)
				requested[c.Location.Pack] = true
			}
			// Strict pre-restore negative case; authorization/network errors never count.
			out, e := client.GetObject(ctx, &awss3.GetObjectInput{Bucket: &cfg.Bucket, Key: &file.Path}, record)
			if e == nil {
				out.Body.Close()
				return res, errors.New("archive unexpectedly readable before restore")
			}
			var api smithy.APIError
			if !errors.As(e, &api) || api.ErrorCode() != "InvalidObjectState" || status.Load() != 403 {
				return res, e
			}
			res.Requests = append(res.Requests, RequestReceipt{Operation: "GetObjectArchived", Key: file.Path, Status: int(status.Load())})
			if err = saveJSON(filepath.Join(opt.Out, fmt.Sprintf("restore-intent-%03d.json", i)), map[string]string{"key": file.Path, "state": "dispatched-unknown"}); err != nil {
				return res, err
			}
			_, e = client.RestoreObject(ctx, &awss3.RestoreObjectInput{Bucket: &cfg.Bucket, Key: &file.Path, RestoreRequest: &types.RestoreRequest{Days: aws.Int32(1), GlacierJobParameters: &types.GlacierJobParameters{Tier: types.TierStandard}}}, record)
			res.Requests = append(res.Requests, RequestReceipt{Operation: "RestoreObject", Key: file.Path, Status: int(status.Load())})
			if e != nil && (!errors.As(e, &api) || api.ErrorCode() != "RestoreAlreadyInProgress") {
				return res, e
			}
			if e == nil && status.Load() != 202 && status.Load() != 200 || e != nil && status.Load() != 409 {
				return res, errors.New("unexpected RestoreObject HTTP status")
			}
			if err = saveJSON(filepath.Join(opt.Out, fmt.Sprintf("restore-ack-%03d.json", i)), res.Requests[len(res.Requests)-1]); err != nil {
				return res, err
			}
		}
		if len(requested) == 0 {
			return res, errors.New("no Cold source left to exercise restore")
		}
		res.Status = "waiting"
		return res, nil
	}
	index := map[string]int{"put-cold": 0, "part-cold": 1, "put-restored": 2, "part-restored": 3}[opt.Phase]
	if opt.File != "" && opt.File != res.Probes[index] {
		return res, errors.New("probe file not bound to this phase")
	}
	file := res.Probes[index]
	isPart := opt.Phase == "part-cold" || opt.Phase == "part-restored"
	if opt.Variant == "ir" && (opt.Phase == "put-restored" || opt.Phase == "part-restored") {
		return res, errors.New("IR has no Restored phase")
	}
	wanted := "Cold"
	if opt.Phase == "put-restored" || opt.Phase == "part-restored" {
		wanted = "Restored"
	}
	for _, key := range before.Files[file] {
		chunk, _ := before.Chunk(key)
		p, ok := before.Pack(chunk.Location.Pack, false)
		native, _ := before.Pack(chunk.Location.Pack, true)
		class := s.Glacier
		if opt.Variant == "ir" {
			class = s.GlacierIR
		}
		if !ok || p.State != wanted || native.State != wanted || p.Class != class || native.Class != class {
			return res, errors.New("revival source not in requested native class/state")
		}
	}
	// Source was independently observed before mutation; the new object must not
	// get its bytes from a download or a temporary restore copy.
	if err = saveJSON(filepath.Join(opt.Out, "write-intent.json"), map[string]string{"phase": opt.Phase, "source": file, "state": "dispatched-unknown"}); err != nil {
		return res, err
	}
	body, err := root.Open(file)
	if err != nil {
		return res, err
	}
	defer body.Close()
	info, err := body.Stat()
	if err != nil {
		return res, err
	}
	key := "revival/" + opt.Phase + "/" + file
	request, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	if !isPart {
		out, e := client.PutObject(request, &awss3.PutObjectInput{Bucket: &cfg.Bucket, Key: &key, Body: body, ContentLength: aws.Int64(info.Size())}, record)
		if e != nil {
			return res, e
		}
		res.Requests = append(res.Requests, RequestReceipt{Operation: "PutObject", Key: key, Status: int(status.Load()), ETag: aws.ToString(out.ETag)})
	} else {
		created, e := client.CreateMultipartUpload(request, &awss3.CreateMultipartUploadInput{Bucket: &cfg.Bucket, Key: &key}, record)
		if e != nil {
			return res, e
		}
		if aws.ToString(created.UploadId) == "" {
			return res, errors.New("multipart upload ID missing")
		}
		if err = saveJSON(filepath.Join(opt.Out, "multipart-upload.json"), map[string]string{"key": key, "upload_id": *created.UploadId}); err != nil {
			return res, err
		}
		part, e := client.UploadPart(request, &awss3.UploadPartInput{Bucket: &cfg.Bucket, Key: &key, UploadId: created.UploadId, PartNumber: aws.Int32(1), Body: body, ContentLength: aws.Int64(info.Size())}, record)
		if e != nil {
			return res, e
		}
		res.Requests = append(res.Requests, RequestReceipt{Operation: "UploadPart", Key: key, Status: int(status.Load()), UploadID: *created.UploadId, ETag: aws.ToString(part.ETag)})
		if err = saveJSON(filepath.Join(opt.Out, "part-ack.json"), res.Requests[len(res.Requests)-1]); err != nil {
			return res, err
		}
		partView, e := capture(ctx)
		if e != nil {
			return res, e
		}
		if e = VerifyReplacement(before, partView, file, partView.Bounds.Lower, opt.Variant == "ir"); e != nil {
			return res, e
		}
		if e = saveJSON(filepath.Join(opt.Out, "part-snapshot.json"), partView); e != nil {
			return res, e
		}

		if aws.ToString(part.ETag) == "" {
			return res, errors.New("part ETag missing; retain upload for reconciliation")
		}
		_, e = client.CompleteMultipartUpload(request, &awss3.CompleteMultipartUploadInput{Bucket: &cfg.Bucket, Key: &key, UploadId: created.UploadId, MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}}}, record)
		if e != nil {
			return res, e
		}
		res.Requests = append(res.Requests, RequestReceipt{Operation: "CompleteMultipartUpload", Key: key, Status: int(status.Load()), UploadID: *created.UploadId})
	}
	if err = saveJSON(filepath.Join(opt.Out, "write-ack.json"), res.Requests); err != nil {
		return res, err
	}
	after, err := capture(ctx)
	if err != nil {
		return res, err
	}
	res.Snapshot = &after
	if err = saveJSON(filepath.Join(opt.Out, "snapshot-after.json"), after); err != nil {
		return res, err
	}
	// The trusted lower bound was sampled after the response, so native
	// protection must still cover at least 15 minutes from that observation.
	if err = VerifyReplacement(before, after, file, after.Bounds.Lower, opt.Variant == "ir"); err != nil {
		return res, err
	}
	if err = read(key, file); err != nil {
		return res, err
	}
	res.ReadVerified = true
	res.RevivalVerified = opt.Variant == "flexible"
	res.IRReuseVerified = opt.Variant == "ir"
	res.Status = "passed"
	return res, nil
}
