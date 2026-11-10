package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/lifecycle"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/multipart/core"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/storage"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

type coldFlowScenario struct {
	Class         storage.StorageClass
	Part, Restore bool
}

func TestNativeColdCloudEdgeFlows(t *testing.T) {
	for _, class := range []storage.StorageClass{storage.GlacierIR, storage.Glacier} {
		for _, part := range []bool{false, true} {
			stages := []bool{false}
			if class == storage.Glacier {
				stages = append(stages, true)
			}
			for _, restore := range stages {
				t.Run(string(class)+map[bool]string{false: "/PUT", true: "/UploadPart"}[part]+map[bool]string{false: "/cold", true: "/restored"}[restore], func(t *testing.T) { runNativeSharedRuntime(t, false, coldFlowScenario{class, part, restore}) })
			}
		}
	}
}

// Native DDB + production SDK/Moto validates composition, not AWS restore time.
// Async acceptance/expiry are separately controlled in native lifecycle tests.
func nativeColdFlows(t *testing.T, ctx context.Context, nodes []Dependencies, m *dynamodb.Store, _ *s3.Client, _ string, bucket storage.Bucket, clk *gcFlowClock, scenario coldFlowScenario, token string) {
	t.Helper()
	const cred = "RUNTIMEKEY00000000001"
	class := scenario.Class
	for _, node := range nodes {
		// Exercise exact jobs here. Periodic discovery has its own native test.
		for kind, executor := range node.Operations.Cluster.Runner.Executors {
			executor.Periodic = 0
			node.Operations.Cluster.Runner.Executors[kind] = executor
		}
		t.Cleanup(func() {
			if e := node.Operations.Cluster.Runner.Close(context.Background()); e != nil {
				t.Error(e)
			}
		})
	}
	handler, e := admin.NewServer(*nodes[0].Admin)
	if e != nil {
		t.Fatal(e)
	}
	call := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	response := call("PUT", "/admin/v1/policy", `{"expected_revision":1,"tiering":{"cold_after_days":0,"min_pack_live_ratio":0,"cold_storage_class":"`+string(class)+`"}}`)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body)
	}
	data := bytes.Repeat([]byte("cold-runtime-body"), 600)
	put := func(key string) {
		svc, e := nodes[0].ObjectsForRequest(ctx, bucket.Name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = svc.PutObject(ctx, object.PutRequest{Bucket: bucket, Key: key, UserID: "owner", CredentialID: cred, ContentLength: int64(len(data)), Body: bodyForNative(data)}); e != nil {
			t.Fatal(e)
		}
	}
	read := func(key string) ([]byte, error) {
		svc, e := nodes[2].ObjectsForRequest(ctx, bucket.Name)
		if e != nil {
			return nil, e
		}
		var got bytes.Buffer
		_, e = svc.GetObject(ctx, object.GetRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: key, CredentialID: cred, Emit: func(_ context.Context, b []byte) error { _, e := got.Write(b); return e }})
		return got.Bytes(), e
	}
	head := func(key string) string {
		t.Helper()
		svc, e := nodes[2].ObjectsForRequest(ctx, bucket.Name)
		if e != nil {
			t.Fatal(e)
		}
		stat, e := svc.Stat(ctx, object.GetRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: key, CredentialID: cred})
		if e != nil {
			t.Fatal(e)
		}
		return stat.Info.Restore
	}
	restore := func(key string, days int) (object.RestoreResult, error) {
		svc, e := nodes[1].ObjectsForRequest(ctx, bucket.Name)
		if e != nil {
			return object.RestoreResult{}, e
		}
		return svc.RestoreObject(ctx, object.RestoreRequest{Bucket: bucket.Name, BucketID: bucket.ID, Key: key, CredentialID: cred, Days: days, Tier: "Expedited"})
	}
	put("cold-original")
	duplicate := func(key string) {
		if !scenario.Part {
			put(key)
			return
		}
		svc, e := nodes[0].ObjectsForRequest(ctx, bucket.Name)
		if e != nil {
			t.Fatal(e)
		}
		u, e := svc.CreateUpload(ctx, object.CreateUploadRequest{Bucket: bucket, Key: key, CredentialID: cred, ChecksumAlgorithm: "SHA256", ChecksumType: "COMPOSITE"})
		if e != nil {
			t.Fatal(e)
		}
		part, e := svc.UploadPart(ctx, object.PartRequest{Upload: u, Number: 1, Body: bodyForNative(data), ContentLength: int64(len(data)), CredentialID: cred})
		if e != nil {
			t.Fatal(e)
		}
		u, e = svc.GetUpload(ctx, u.ID)
		if e != nil {
			t.Fatal(e)
		}
		input := core.Input{V: 1, BucketID: bucket.ID, Key: key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType, Parts: []core.Part{{PartNumber: 1, ETag: part.ETag, Checksum: &part.ChecksumValue}}}
		if _, e = svc.CompleteUpload(ctx, object.CompleteRequest{Upload: u, Input: input, UserID: "owner", CredentialID: cred}); e != nil {
			t.Fatal(e)
		}
	}
	key, _ := engine.HashChunk(bucket.Domain, data)
	rows, e := m.GetChunks(ctx, []storage.ChunkKey{key}, true)
	if e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	old := rows[key]
	// Pins and durable PUT protection outrank cold_after_days=0. No sleeps or
	// unsafe record edits: move the modeled UTC only after the request quiesces.
	tick := func() {
		for _, node := range nodes {
			if e := node.Operations.Registry.Tick(ctx, new(Admission), jobs.NodeStatus{Ready: true, AppliedPolicyRevision: 2}); e != nil {
				t.Fatal(e)
			}
			if e := node.Operations.Cluster.Runner.Tick(ctx); e != nil {
				t.Fatal(e)
			}
		}
	}
	wait := func(id string) jobs.Job {
		for range 12 {
			tick()
			job, e := m.GetJob(ctx, id)
			if e != nil {
				t.Fatal(e)
			}
			if job.State.Terminal() {
				tick()
				if job.State != jobs.Succeeded {
					t.Fatal(job)
				}
				return job
			}
		}
		job, _ := m.GetJob(ctx, id)
		t.Fatalf("job did not settle: %+v", job)
		return job
	}
	params, _ := json.Marshal(lifecycle.Params{PackID: old.Location.Pack.ID, Generation: old.Location.Pack.Generation})
	job, e := nodes[0].Operations.Cluster.Create(ctx, jobs.Create{Type: "tiering", Params: params})
	if e != nil {
		t.Fatal(e)
	}
	tick()
	before, e := m.GetPacks(ctx, []storage.PackRef{old.Location.Pack})
	if e != nil || before[old.Location.Pack].State != "Warm" || before[old.Location.Pack].Class != storage.Standard {
		t.Fatal("tiering ignored PUT protection", before, e)
	}
	clk.offset.Add(int64(46 * time.Minute))
	wait(job.ID)
	packs, e := m.GetPacks(ctx, []storage.PackRef{old.Location.Pack})
	if e != nil || packs[old.Location.Pack].Class != class || packs[old.Location.Pack].State != "Cold" || packs[old.Location.Pack].MinRetentionUntil.Before(clk.Now().WallUTC.Add(89*24*time.Hour)) {
		t.Fatal(packs, e)
	}
	copyService, e := nodes[1].ObjectsForRequest(ctx, bucket.Name)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = copyService.CopyObject(ctx, object.CopyRequest{Source: bucket, SourceKey: "cold-original", Bucket: bucket, Key: "metadata-copy", UserID: "owner", CredentialID: cred}); e != nil {
		t.Fatal("same-domain Cold metadata copy", e)
	}
	if class == storage.GlacierIR {
		if out, e := restore("cold-original", 1); !errors.Is(e, object.ErrNotArchived) || head("cold-original") != "" {
			t.Fatal("IR restore", out, e)
		}
		for range 2 {
			got, e := read("cold-original")
			if e != nil || !bytes.Equal(got, data) {
				t.Fatal("IR read", e)
			}
		}
		duplicate("reuse-ir")
		rows, e = m.GetChunks(ctx, []storage.ChunkKey{key}, true)
		if e != nil || rows[key] != old {
			t.Fatal("IR PUT changed identity/location", rows, e)
		}
		if got, e := read("reuse-ir"); e != nil || !bytes.Equal(got, data) {
			t.Fatal("successful IR PUT unreadable", e)
		}
	} else {
		if scenario.Restore {
			restoreJob := func() string {
				t.Helper()
				page, e := m.ListJobs(ctx, jobs.PageRequest{Limit: 100, ActiveOnly: true})
				if e != nil {
					t.Fatal(e)
				}
				id := ""
				for _, job := range page.Jobs {
					if job.Type == "pack_restore" {
						if id != "" {
							t.Fatal("restore of one generation queued twice", page.Jobs)
						}
						id = job.ID
					}
				}
				if id == "" {
					t.Fatal("no restore job queued")
				}
				return id
			}
			if h := head("cold-original"); h != "" {
				t.Fatal("x-amz-restore before any restore", h)
			}
			if out, e := restore("cold-original", 2); e != nil || out != (object.RestoreResult{Initiated: 1}) {
				t.Fatal("RestoreObject did not start", out, e)
			}
			if out, e := restore("metadata-copy", 2); e != nil || out != (object.RestoreResult{InProgress: 1}) {
				t.Fatal("second RestoreObject of the shared pack", out, e)
			}
			// GET is refused without waiting; its auto-restore shares the job.
			got, e := read("cold-original")
			if !errors.Is(e, storage.ErrCold) || len(got) != 0 {
				t.Fatal("unrestored archive read", e)
			}
			wait(restoreJob()) // Moto completion is emulator evidence, not a live AWS demo.
			restored := head("cold-original")
			if !strings.HasPrefix(restored, `ongoing-request="false", expiry-date="`) || head("metadata-copy") != restored {
				t.Fatal("x-amz-restore after completion", restored)
			}
			for _, key := range []string{"cold-original", "metadata-copy"} {
				if got, e = read(key); e != nil || !bytes.Equal(got, data) {
					t.Fatal("completed restore not readable", key, e)
				}
			}
			if out, e := restore("cold-original", 1); e != nil || out != (object.RestoreResult{}) {
				t.Fatal("copy already lasting Days was restored again", out, e)
			}
			if out, e := restore("cold-original", 5); e != nil || out != (object.RestoreResult{Initiated: 1}) {
				t.Fatal("renewal did not start", out, e)
			}
			wait(restoreJob())
			if renewed := head("cold-original"); renewed == restored || !strings.HasPrefix(renewed, `ongoing-request="false", expiry-date="`) {
				t.Fatal("renewal kept expiry", restored, renewed)
			}
			if got, e = read("cold-original"); e != nil || !bytes.Equal(got, data) {
				t.Fatal("renewed restore not readable", e)
			}
		}
		duplicate("revived")
		rows, e = m.GetChunks(ctx, []storage.ChunkKey{key}, true)
		if e != nil || rows[key].Key != old.Key || rows[key].Location.Pack == old.Location.Pack {
			t.Fatal("Restored PUT did not revive from body", rows, e)
		}
		if got, e := read("revived"); e != nil || !bytes.Equal(got, data) {
			t.Fatal("successful revived PUT unreadable", e)
		}
	}
	// IR has live canonical chunks; GLACIER was superseded by body revival, so
	// promotion has no live plan and is intentionally not dispatched in that case.
	if class == storage.GlacierIR {
		response = call(http.MethodPost, "/admin/v1/packs/"+old.Location.Pack.ID+"/"+old.Location.Pack.Generation+"/promote", `{}`)
		var detail jobs.Detail
		if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &detail) != nil {
			t.Fatal(response.Code, response.Body)
		}
		wait(detail.ID)
		rows, e = m.GetChunks(ctx, []storage.ChunkKey{key}, true)
		if e != nil || rows[key].Location.Pack == old.Location.Pack {
			t.Fatal(rows, e)
		}
		if got, e := read("cold-original"); e != nil || !bytes.Equal(got, data) {
			t.Fatal("promotion changed body", e)
		}
	}
}
