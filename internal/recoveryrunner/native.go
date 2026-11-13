package recoveryrunner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/KinasTomes/miles/internal/benchmarknative"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/recovery"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// NativeEvidence retains facts, not caller-supplied verdicts. Replay is into
// independent in-memory images; it never installs over the running namespace.
type NativeEvidence struct {
	RunID, Slot, Source, History         string
	Epoch                                uint64
	Probe                                s.WriterFence
	LeaseRejected, DispatchRejected      bool
	Seal, Checkpoint, ReplayedCheckpoint recovery.AttachmentReference
	RebuiltRecords                       int
}

// InjectLeaseFault closes ONLY a newly acquired synthetic control lease. No
// runtime writer is reclaimed, no clock is advanced, and no raw row is edited.
func InjectLeaseFault(ctx context.Context, store s.R1Store, request s.R1Acquire) (bool, bool, error) {
	if request.Kind != "control" || request.Fence.Epoch != 1 {
		return false, false, s.ErrInvalid
	}
	if _, err := store.AcquireR1(ctx, request); err != nil {
		return false, false, err
	}
	if err := store.CloseR1(ctx, request.Fence); err != nil {
		return false, false, err
	}
	_, check := store.CheckR1(ctx, request.Fence)
	dispatch := store.BeginR1Action(ctx, request.Fence, "acceptance-stale-dispatch")
	if !errors.Is(check, s.ErrFenced) || !errors.Is(dispatch, s.ErrFenced) {
		return false, false, errors.New("closed probe retained native dispatch authority")
	}
	if err := store.ReleaseR1(ctx, request.Fence); err != nil {
		return true, true, err
	}
	return true, true, nil
}

// configureNative supplies the concrete injector/verifier for the CLI. No
// external shell command or boolean 'reviewed' switch can certify a native run.
func configureNative(ctx context.Context, opt *Options) error {
	c, err := benchmarknative.Read(opt.NativeConfig)
	if err != nil {
		return err
	}
	if opt.FaultScope != "lease-revoke" && opt.FaultScope != "checkpoint-cut" {
		return errors.New("native injector supports only own lease-revoke or checkpoint-cut")
	}
	if c.Account != opt.ExpectedAWSAccount || opt.SystemConfigTable != c.Tables["SystemConfig"] {
		return s.ErrInvalid
	}
	cfg, err := benchmarknative.Session(ctx, c, opt.AWSCredentialsFile)
	if err != nil {
		return err
	}
	api := ddb.NewFromConfig(cfg)
	for name := range dynamodb.Tables() {
		table := c.Tables[name]
		if table == "" {
			return errors.New("missing native table binding")
		}
		arn := "arn:aws:dynamodb:" + c.Region + ":" + c.Account + ":table/" + table
		tags, err := api.ListTagsOfResource(ctx, &ddb.ListTagsOfResourceInput{ResourceArn: &arn})
		if err != nil {
			return err
		}
		actual := map[string]string{}
		for _, tag := range tags.Tags {
			actual[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
		if tags.NextToken != nil {
			return errors.New("truncated ownership tags")
		}
		for key, value := range c.Tags {
			if actual[key] != value {
				return errors.New("native table ownership mismatch")
			}
		}
	}
	physical := s3.NewFromConfig(cfg)
	if err := benchmarknative.CheckBucket(ctx, physical, c); err != nil {
		return err
	}
	clk := clock.NewReal()
	utc := coordination.Chrony{Clock: clk, Reference: "169.254.169.123", RestartID: c.RunID + "-" + c.Slot}
	if _, err := utc.Bounds(ctx); err != nil {
		return err
	}
	store, err := dynamodb.New(dynamodb.Options{API: api, Tables: c.Tables, MaxAttempts: 1,
		Authority: &dynamodb.SharedAuthority{Time: utc, Lifetime: 5 * time.Minute}, RequireProducers: true})
	if err != nil {
		return err
	}
	objects, err := recovery.NewS3Objects(physical, c.Bucket, "", c.KMSKey)
	if err != nil {
		return err
	}
	catalog, err := recovery.LoadSelectedCatalog(ctx, objects, c.History, c.Incarnation)
	if err != nil {
		return err
	}
	evidence := NativeEvidence{RunID: c.RunID, Slot: c.Slot, Source: c.Source, History: c.History}
	save := func() error {
		body, err := json.MarshalIndent(evidence, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(opt.Out, "native-proof.json"), body, 0600)
	}
	opt.CheckGate = func(ctx context.Context) (*GateState, error) {
		g, err := store.GetR1Gate(ctx)
		if err != nil {
			return nil, err
		}
		if g.HistoryID != c.History || g.Incarnation != c.Incarnation {
			return nil, s.ErrFenced
		}
		return &GateState{HistoryID: g.HistoryID, Epoch: int64(g.Epoch), State: g.State}, nil
	}
	opt.ReadCheckpoint = func(ctx context.Context, history string, epoch int64) (string, error) {
		if history != c.History || epoch != int64(evidence.Epoch+1) {
			return "", s.ErrFenced
		}
		e, err := store.GetRecoveryEpoch(ctx, history, evidence.Epoch)
		if err != nil {
			return "", err
		}
		if e.State != "Sealed" || e.SealKey != evidence.Seal.Key || e.SealSHA256 != evidence.Seal.Reference.SHA256 {
			return "", s.ErrFenced
		}
		return e.SealKey, nil
	}
	opt.SimulateFault = func(ctx context.Context, _ string) (err error) {
		defer func() { err = errors.Join(err, save()) }()
		if !time.Now().Before(c.StopAt.Add(-3 * time.Minute)) {
			return errors.New("insufficient native cut/cleanup time")
		}
		g, err := store.GetR1Gate(ctx)
		if err != nil {
			return err
		}
		if g.HistoryID != c.History || g.Incarnation != c.Incarnation || g.State != "Open" {
			return s.ErrFenced
		}
		evidence.Epoch = g.Epoch
		probeID, err := s.NewBucketID()
		if err != nil {
			return err
		}
		evidence.Probe = s.WriterFence{ServiceIncarnation: g.Incarnation, LeaseID: "benchmark-probe-" + probeID, Epoch: 1}
		// Record the intended lease before the first mutation; no blind resend.
		if err := save(); err != nil {
			return err
		}
		evidence.LeaseRejected, evidence.DispatchRejected, err = InjectLeaseFault(ctx, store, s.R1Acquire{
			Fence: evidence.Probe, Owner: c.RunID, Kind: "control", Deadline: time.Now().Add(4 * time.Minute)})
		if err != nil {
			return err
		}
		workerID, err := s.NewBucketID()
		if err != nil {
			return err
		}
		fence := s.WriterFence{ServiceIncarnation: g.Incarnation, LeaseID: "benchmark-cut-" + workerID, Epoch: 1}
		if _, err := store.AcquireR1(ctx, s.R1Acquire{Fence: fence, Owner: c.RunID, Kind: "recovery", Deadline: time.Now().Add(4 * time.Minute)}); err != nil {
			return err
		}
		// Worker closure uses ordinary R1/R2 fencing. Errors retain authority and
		// cut intent for reconciliation, never force reopen a gate.
		w := recovery.Worker{Store: store, Payloads: store, Objects: objects, Catalog: catalog, Clock: clk, Fence: fence}
		cut := s.RecoveryCutRequest{HistoryID: g.HistoryID, Epoch: g.Epoch, CloseID: workerID, Worker: fence}
		body, _ := json.Marshal(cut)
		if err := os.WriteFile(filepath.Join(opt.Out, "native-cut-intent.json"), body, 0600); err != nil {
			return err
		}
		evidence.Seal, err = w.CloseAndSeal(ctx, cut)
		if err != nil {
			return err
		}
		if err := store.CloseR1(ctx, fence); err != nil {
			return err
		}
		return store.ReleaseR1(ctx, fence)
	}
	opt.VerifyNativeProof = func(ctx context.Context, history string, before, after int64) (NativeProof, error) {
		var zero NativeProof
		if history != c.History || before != int64(evidence.Epoch) || after != before+1 {
			return zero, s.ErrFenced
		}
		evidence.Checkpoint, evidence.ReplayedCheckpoint, evidence.RebuiltRecords, err = recovery.VerifyReplay(ctx, objects, catalog, evidence.Epoch)
		if err != nil {
			return zero, err
		}
		rebuilt, err := (recovery.Checkpointer{Objects: objects, Catalog: catalog}).Rebuild(ctx, evidence.Checkpoint)
		if err != nil {
			return zero, err
		}
		manifest, err := loadManifest(opt.Manifest)
		if err != nil {
			return zero, err
		}
		for _, file := range manifest.Files {
			found := false
			for _, head := range rebuilt.Heads {
				if head.Object.Bucket == c.LogicalBucket && head.Object.Key == file.Path && !head.Deleted && head.Size == int64(file.Bytes) {
					found = true
					break
				}
			}
			if !found {
				return zero, errors.New("sealed replay omitted seeded trial object")
			}
		}
		if err := save(); err != nil {
			return zero, err
		}
		return NativeProof{LeaseFenceVerified: evidence.LeaseRejected && evidence.DispatchRejected,
			SealedCutVerified: evidence.Seal.Key != "", ReplayIdempotencyVerified: evidence.RebuiltRecords > 0}, nil
	}
	return nil
}
