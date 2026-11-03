package app

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/multipart/core"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/recovery"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func nativeSweepDuringCut(t *testing.T, ctx context.Context, nodes []Dependencies, clients runtimeClients, prefix string, bucket storage.Bucket) {
	t.Helper()
	for _, kind := range []jobs.Type{"purge_uploads", "recover_uploads"} {
		t.Run(string(kind)+"_during_cut", func(t *testing.T) {
			for _, node := range nodes {
				if err := node.Operations.Cluster.Runner.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			metadata := nodes[0].Metadata.(*sharedMetadata)
			store := metadata.Store
			svc, err := nodes[0].ObjectsForRequest(ctx, bucket.Name)
			if err != nil {
				t.Fatal(err)
			}
			u, err := svc.CreateUpload(ctx, object.CreateUploadRequest{Bucket: bucket, Key: string(kind) + "-cut", CredentialID: "RUNTIMEKEY00000000001", ChecksumAlgorithm: "SHA256", ChecksumType: "COMPOSITE"})
			if err != nil {
				t.Fatal(err)
			}
			payload := []byte("frozen sweep/cut regression")
			part, err := svc.UploadPart(ctx, object.PartRequest{Upload: u, Number: 1, CredentialID: u.Credential, ContentLength: int64(len(payload)), Body: bodyForNative(payload)})
			if err != nil {
				t.Fatal(err)
			}
			field := "expires_at"
			if kind == "recover_uploads" {
				nativeFreezeForSweep(t, ctx, metadata, u, part)
				field = "lease_until"
			}
			// Change only this synthetic fixture's time and CAS token.
			_, err = clients.metadata.UpdateItem(ctx, &ddb.UpdateItemInput{
				TableName: aws.String(prefix + "Uploads"),
				Key: map[string]types.AttributeValue{
					"upload_id": &types.AttributeValueMemberS{Value: u.ID},
				},
				UpdateExpression: aws.String("SET #upload.#time=:past, #revision=:token"),
				ExpressionAttributeNames: map[string]string{
					"#upload": "upload-record", "#time": field, "#revision": "record_revision",
				},
				ExpressionAttributeValues: map[string]types.AttributeValue{
					":past":  &types.AttributeValueMemberS{Value: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)},
					":token": &types.AttributeValueMemberS{Value: strings.Repeat("e", 64)},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			runner := nodes[1].Operations.Cluster.Runner
			executors := runner.Executors
			defer func() { runner.Executors = executors }()
			entered, resume := make(chan struct{}), make(chan struct{})
			sweep := uploadJobs{store: store, utc: metadata.utc, objects: func(call context.Context, bucket string) (*object.Service, error) {
				close(entered)
				select {
				case <-resume:
					return nodes[1].ObjectsForRequest(call, bucket)
				case <-call.Done():
					return nil, call.Err()
				}
			}}
			executor := sweep.executor(kind)
			executor.Periodic = 0
			runner.Executors = map[jobs.Type]jobs.Executor{kind: executor}
			job, err := runner.Create(ctx, jobs.Create{Type: kind})
			if err != nil {
				t.Fatal(err)
			}
			call, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			var tickErr error
			go func() { tickErr = runner.Tick(call); close(done) }()
			defer func() { cancel(); <-done }()
			select {
			case <-entered:
			case <-done:
				t.Fatal("sweep did not reach writer admission", tickErr)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			g, err := store.GetR1Gate(ctx)
			if err != nil {
				t.Fatal(err)
			}
			authority := sharedJobAuthority{metadata}
			worker, err := authority.Acquire(ctx, "cut-"+string(kind), "recovery")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := authority.Close(context.WithoutCancel(ctx), worker); err != nil {
					t.Error(err)
				}
			}()
			closeID, err := storage.NewBucketID()
			if err != nil {
				t.Fatal(err)
			}
			cut := storage.RecoveryCutRequest{HistoryID: g.HistoryID, Epoch: g.Epoch, CloseID: closeID, Worker: worker}
			epoch, err := store.BeginRecoveryCut(ctx, cut)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				err := store.AbortRecoveryCut(context.WithoutCancel(ctx), cut)
				if err != nil && !errors.Is(err, storage.ErrConflict) {
					t.Error(err)
				}
			}()
			close(resume)
			<-done
			if tickErr != nil {
				t.Fatal(tickErr)
			}
			pending, err := store.GetJob(ctx, job.ID)
			if err != nil || pending.State != jobs.Running || pending.PendingStep == "" {
				t.Fatal("cut-blocked sweep did not retain its step", pending, err)
			}
			if err := runner.Authority.Renew(ctx, pending.Worker); err != nil {
				t.Fatal(err)
			}
			for page := 0; epoch.State == "Closing" && page < 64; page++ {
				epoch, err = store.StepRecoveryCut(ctx, cut)
				if err != nil {
					t.Fatal("cut waits for the sweep that needs its admission", err)
				}
			}
			if epoch.State != "Closed" {
				t.Fatal("cut did not close", epoch.State)
			}
			lease, err := store.GetR1Lease(ctx, pending.Worker.LeaseID)
			if err != nil || lease.Kind != "recovery" {
				t.Fatal("sweep progress retains a producer lease", lease.Kind, err)
			}
			if err := store.BeginR1Action(ctx, pending.Worker, "data/forbidden"); !errors.Is(err, storage.ErrFenced) {
				t.Fatal("job progress authority allowed physical dispatch", err)
			}
			journal := nodes[0].Journal
			sealer := recovery.Worker{Store: store, Payloads: store, Objects: journal.objects, Catalog: journal.catalog, Clock: journal.clock, Fence: worker}
			if _, err := sealer.CloseAndSeal(ctx, cut); err != nil {
				t.Fatal("cut did not seal", err)
			}
			runner.Executors[kind] = executors[kind]
			if err := runner.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			result, err := store.GetJob(ctx, job.ID)
			if err != nil || result.State != jobs.Succeeded {
				t.Fatal("sweep did not resume after sealed cut", result, err)
			}
			upload, err := store.GetUpload(ctx, u.ID)
			want := "Aborted"
			if kind == "recover_uploads" {
				want = "Completed"
			}
			if err != nil || upload.State != want {
				t.Fatal("upload outcome", upload.State, err)
			}
		})
	}
}

func nativeFreezeForSweep(t *testing.T, ctx context.Context, metadata *sharedMetadata, u storage.MultipartUpload, part object.PartResult) {
	t.Helper()
	store := metadata.Store
	g, err := store.GetR1Gate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	op, err := storage.NewBucketID()
	if err != nil {
		t.Fatal(err)
	}
	writer := storage.WriterFence{LeaseID: "freeze-" + u.ID, ServiceIncarnation: g.Incarnation, Epoch: 1}
	bounds, err := metadata.utc.Bounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.AcquireR1(ctx, storage.R1Acquire{Fence: writer, Owner: "fixture", Kind: "writer", OpID: op, Object: u.Object, Deadline: bounds.Upper.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	mutation := storage.WithMutation(ctx, storage.MutationContext{Writer: writer, HistoryID: g.HistoryID, Epoch: g.Epoch, LogicalOpID: op, MutationID: op, Object: u.Object})
	owner, err := storage.NewBucketID()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{u.ID, owner} {
		if err := store.BindR1(mutation, writer, storage.ObjectHead{Object: u.Object, ManifestID: id}); err != nil {
			t.Fatal(err)
		}
	}
	input := core.Input{V: 1, BucketID: u.Object.BucketID, Key: u.Object.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType, Parts: []core.Part{{PartNumber: 1, ETag: part.ETag, Checksum: &part.ChecksumValue}}}
	descriptor, err := manifest.WriteCompletionStream(manifest.CompletionInput, owner, bytes.NewReader(input.Canonical()), func(n uint32, data []byte) error {
		return store.WriteCompletionPage(mutation, manifest.CompletionInput, owner, n, data, writer)
	})
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.GetUpload(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.FreezeCompletion(mutation, storage.FreezeRequest{Request: storage.MultipartRequest{UploadID: u.ID, Credential: u.Credential, OpID: op, Writer: writer, ExpectedRevision: current.Revision}, Input: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CloseR1(ctx, writer); err != nil {
		t.Fatal(err)
	}
	if err := store.ReleaseR1(ctx, writer); err != nil {
		t.Fatal(err)
	}
}
