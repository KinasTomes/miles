package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func nativeUploadJobs(t *testing.T, ctx context.Context, nodes []Dependencies, clients runtimeClients, prefix string, bucket storage.Bucket) {
	t.Helper()
	svc, e := nodes[0].ObjectsForRequest(ctx, bucket.Name)
	if e != nil {
		t.Fatalf("upload job fixture at stage %d: %v", 0, e)
	}
	u, e := svc.CreateUpload(ctx, object.CreateUploadRequest{Bucket: bucket, Key: "job-expired", CredentialID: "RUNTIMEKEY00000000001", ChecksumAlgorithm: "SHA256", ChecksumType: "COMPOSITE"})
	if e != nil {
		t.Fatal(e)
	}
	payload := bytes.Repeat([]byte("staged data"), 1024)
	if _, e = svc.UploadPart(ctx, object.PartRequest{Upload: u, Number: 1, CredentialID: "RUNTIMEKEY00000000001", ContentLength: int64(len(payload)), Body: bodyForNative(payload)}); e != nil {
		t.Fatal(e)
	}
	// Fault fixture changes only this synthetic upload's expiry. The production
	// worker still observes the base table and invokes the real fenced cleanup.
	_, e = clients.metadata.UpdateItem(ctx, &ddb.UpdateItemInput{TableName: aws.String(prefix + "Uploads"), Key: map[string]types.AttributeValue{"upload_id": &types.AttributeValueMemberS{Value: u.ID}}, UpdateExpression: aws.String("SET #upload.#expiry=:expired, #revision=:token"), ExpressionAttributeNames: map[string]string{"#upload": "upload-record", "#expiry": "expires_at", "#revision": "record_revision"}, ExpressionAttributeValues: map[string]types.AttributeValue{":expired": &types.AttributeValueMemberS{Value: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}, ":token": &types.AttributeValueMemberS{Value: strings.Repeat("f", 64)}}})
	if e != nil {
		t.Fatal(e)
	}
	store := nodes[0].Operations.Registry.Store
	for _, d := range nodes {
		gate := new(Admission)
		d.Operations.Cluster.Gate = gate
		if e = d.Operations.Registry.Tick(ctx, gate, jobs.NodeStatus{Ready: true, AppliedPolicyRevision: 1}); e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() {
			if e := d.Operations.Cluster.Runner.Close(context.Background()); e != nil {
				t.Error(e)
			}
		})
	}
	wait := func(id string) jobs.Job {
		deadline := time.Now().Add(25 * time.Second)
		for time.Now().Before(deadline) {
			for _, d := range nodes {
				if e = d.Operations.Cluster.Runner.Tick(ctx); e != nil {
					t.Fatal(e)
				}
			}
			j, e := store.GetJob(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			if j.State.Terminal() {
				return j
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("upload job did not settle")
		return jobs.Job{}
	}
	j, e := nodes[1].Operations.Cluster.Create(ctx, jobs.Create{Type: "purge_uploads", DryRun: true})
	if e != nil {
		t.Fatal(e)
	}
	// Suppress automatic scheduling during a dry-run proof so a distinct real
	// periodic purge cannot mutate the fixture in parallel.
	for _, d := range nodes {
		for _, kind := range []jobs.Type{"purge_uploads", "recover_uploads"} {
			x := d.Operations.Cluster.Runner.Executors[kind]
			x.Periodic = 0
			d.Operations.Cluster.Runner.Executors[kind] = x
		}
	}
	if out := wait(j.ID); out.State != jobs.Succeeded {
		t.Fatal(out)
	}
	raw, e := nodes[0].Metadata.(*sharedMetadata).GetUpload(ctx, u.ID)
	if e != nil || raw.State != "Active" {
		t.Fatal("dry run changed upload", raw, e)
	}
	j, e = nodes[2].Operations.Cluster.Create(ctx, jobs.Create{Type: "purge_uploads"})
	if e != nil {
		t.Fatal(e)
	}
	out := wait(j.ID)
	if out.State != jobs.Succeeded {
		t.Fatal(out)
	}
	var counts uploadCheckpoint
	if json.Unmarshal(out.Result, &counts) != nil || counts.Aborted != 1 {
		t.Fatal(string(out.Result))
	}
	quota, e := nodes[0].Metadata.(*sharedMetadata).GetQuota(ctx, bucket.OwnerID)
	if e != nil || quota.ActiveUploads != 0 || quota.Staged != 0 {
		t.Fatal(quota, e)
	}
	raw, e = nodes[0].Metadata.(*sharedMetadata).GetUpload(ctx, u.ID)
	if e != nil || raw.State != "Aborted" {
		t.Fatal(raw, e)
	}
	j, e = nodes[2].Operations.Cluster.Create(ctx, jobs.Create{Type: "recover_uploads"})
	if e != nil {
		t.Fatal(e)
	}
	if out := wait(j.ID); out.State != jobs.Succeeded {
		t.Fatal(out)
	}
}
