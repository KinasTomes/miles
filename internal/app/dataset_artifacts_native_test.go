package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/stats"
	"github.com/KinasTomes/miles/internal/storage"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

// Opt in to exporting small, measured native artifacts. Normal CI still builds
// this collector; no dataset or evidence is written without an explicit output.
func TestNativeDatasetArtifacts(t *testing.T) {
	out := os.Getenv("MILES_DATASET_ARTIFACTS_OUT")
	if out == "" {
		t.Skip("artifact export not requested")
	}
	if !filepath.IsAbs(out) {
		t.Fatal("artifact output must be absolute")
	}
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal("output must be new", err)
	}
	runNativeScenario(t, true, "", func(ctx context.Context, nodes []Dependencies, m *dynamodb.Store, _ *s3.Client, _ string, bucket storage.Bucket, token string) {
		captureDatasetArtifacts(t, ctx, out, nodes, m, bucket, token)
	})
}

func captureDatasetArtifacts(t *testing.T, ctx context.Context, out string, nodes []Dependencies, m *dynamodb.Store, first storage.Bucket, token string) {
	started := time.Now().UTC()
	write := func(name string, body []byte) {
		t.Helper()
		path := filepath.Join(out, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	encode := func(name string, value any) {
		t.Helper()
		body, err := json.MarshalIndent(value, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		write(name, append(body, '\n'))
	}
	h, err := admin.NewServer(*nodes[0].Admin)
	if err != nil {
		t.Fatal(err)
	}
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, r)
		if response.Code != want {
			t.Fatalf("%s %s: status=%d (response withheld)", method, path, response.Code)
		}
		return bytes.Clone(response.Body.Bytes())
	}
	type cohort struct {
		Alias      string `json:"alias"`
		Owner      string `json:"owner"`
		Bucket     string `json:"bucket"`
		BucketID   string `json:"bucket_id"`
		Domain     string `json:"domain"`
		credential string
		stored     storage.Bucket
	}
	cohorts := []cohort{{Alias: "owner-a", Owner: "owner", Bucket: first.Name, BucketID: first.ID, Domain: first.Domain, credential: "RUNTIMEKEY00000000001", stored: first}}
	for _, spec := range []struct{ alias, owner, domain string }{{"owner-b", "artifact-shared", first.Domain}, {"owner-c", "artifact-isolated", "artifact-isolated"}} {
		body, _ := json.Marshal(map[string]any{"user_id": spec.owner, "default_domain": spec.domain, "logical_limit": 1 << 30})
		call("POST", "/admin/v1/users", string(body), 201)
		body, _ = json.Marshal(map[string]string{"user_id": spec.owner})
		var c struct {
			ID string `json:"access_key_id"`
		}
		// Never export the credential response (it contains a newly issued secret).
		if err := json.Unmarshal(call("POST", "/admin/v1/credentials", string(body), 201), &c); err != nil || c.ID == "" {
			t.Fatal("credential setup failed")
		}
		b, _, err := nodes[0].Metadata.CreateBucket(ctx, storage.BucketCreate{CredentialID: c.ID, UserID: spec.owner, Name: "artifact-" + spec.alias})
		if err != nil {
			t.Fatal(err)
		}
		call("GET", "/admin/v1/buckets/"+url.PathEscape(b.Name)+"/policy", "", 200)
		cohorts = append(cohorts, cohort{Alias: spec.alias, Owner: spec.owner, Bucket: b.Name, BucketID: b.ID, Domain: b.Domain, credential: c.ID, stored: b})
	}
	encode("workload.json", map[string]any{"mode": "controlled-incremental-domain-demonstration", "cohorts": cohorts, "key_prefix": "dataset/", "order": "manifest-order-per-owner", "live_keys": "all versions use distinct keys", "layout_selection": "version-zero from each entropy family, final phase only"})
	write("policy.json", call("GET", "/admin/v1/policy", "", 200))
	for _, c := range cohorts {
		write("policies/"+c.Alias+".json", call("GET", "/admin/v1/buckets/"+url.PathEscape(c.Bucket)+"/policy", "", 200))
	}
	input := filepath.Join(t.TempDir(), "dataset")
	manifest, err := benchmarkdata.Generate(input, "smoke", "miles-w02-v1")
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := os.ReadFile(filepath.Join(input, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	write("dataset-manifest.json", manifestBytes)
	type phase struct {
		Name               string    `json:"name"`
		Owners             int       `json:"owners"`
		WorkloadFinishedAt time.Time `json:"workload_finished_at"`
		SnapshotID         string    `json:"snapshot_id"`
	}
	var phases []phase
	var gets []map[string]any
	capture := func(name string, owners int, finished time.Time) {
		t.Helper()
		if err := nodes[0].Statistics.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		prefix := "phases/" + name + "/"
		body := call("GET", "/admin/v1/stats", "", 200)
		write(prefix+"global.json", body)
		var global stats.Summary
		if err := json.Unmarshal(body, &global); err != nil || !global.PhysicalComplete || global.BytesStored == nil || global.CollectionStartedAt.Before(finished) {
			t.Fatal("incomplete/stale capture", err)
		}
		if global.BytesLogical != int64(owners)*manifest.TotalBytes {
			t.Fatal("live logical bytes mismatch")
		}
		phases = append(phases, phase{name, owners, finished, global.SnapshotID})
		for i, c := range cohorts {
			body := call("GET", "/admin/v1/buckets/"+url.PathEscape(c.Bucket)+"/stats", "", 200)
			write(prefix+"bucket-"+c.Alias+".json", body)
			var b stats.BucketSummary
			if err := json.Unmarshal(body, &b); err != nil || b.SnapshotID != global.SnapshotID || b.BucketID != c.BucketID {
				t.Fatal("snapshot/incarnation mismatch", err)
			}
			write(prefix+"usage-"+c.Alias+".json", call("GET", "/admin/v1/users/"+c.Owner+"/usage?detail=true", "", 200))
			q, err := m.GetQuota(ctx, c.Owner)
			want := int64(0)
			if i < owners {
				want = manifest.TotalBytes
			}
			if err != nil || q.Used != want || q.Reserved != 0 || q.Staged != 0 {
				t.Fatal("logical quota mismatch", err)
			}
			if owners == len(cohorts) {
				for _, f := range manifest.Files {
					if f.Version != 0 {
						continue
					}
					path := "/admin/v1/objects/layout?" + url.Values{"bucket": {c.Bucket}, "key": {"dataset/" + f.Path}}.Encode()
					body := call("GET", path, "", 200)
					write(prefix+"layout-"+c.Alias+"-"+f.Group+".json", body)
					var layout stats.Layout
					if err := json.Unmarshal(body, &layout); err != nil || layout.Domain != c.Domain || layout.Size != int64(f.Bytes) || layout.Occurrences != 1 || !layout.AsOf.Equal(global.AsOf) {
						t.Fatal("layout binding mismatch", err)
					}
				}
			}
		}
	}
	capture("empty", 0, time.Now().UTC())
	for i, c := range cohorts {
		for _, f := range manifest.Files {
			data, err := os.ReadFile(filepath.Join(input, f.Path))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != f.SHA256 {
				t.Fatal("input hash mismatch")
			}
			svc, err := nodes[0].ObjectsForRequest(ctx, c.Bucket)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.PutObject(ctx, object.PutRequest{Bucket: c.stored, Key: "dataset/" + f.Path, UserID: c.Owner, CredentialID: c.credential, ContentLength: int64(len(data)), Body: bodyForNative(data)}); err != nil {
				t.Fatal(err)
			}
			edge, err := nodes[2].ObjectsForRequest(ctx, c.Bucket)
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			if _, err = edge.GetObject(ctx, object.GetRequest{Bucket: c.Bucket, BucketID: c.BucketID, Key: "dataset/" + f.Path, CredentialID: c.credential, Emit: func(_ context.Context, b []byte) error { _, err := got.Write(b); return err }}); err != nil {
				t.Fatal(err)
			}
			actual := sha256.Sum256(got.Bytes())
			if got.Len() != f.Bytes || hex.EncodeToString(actual[:]) != f.SHA256 {
				t.Fatal("GET verification failed")
			}
			gets = append(gets, map[string]any{"cohort": c.Alias, "file": f.Path, "bytes": got.Len(), "sha256": hex.EncodeToString(actual[:])})
		}
		capture(c.Alias, i+1, time.Now().UTC())
	}
	encode("verified-gets.json", gets)
	keys := map[storage.ChunkKey]bool{}
	for _, c := range cohorts {
		for _, f := range manifest.Files {
			keys[storage.ChunkKey{Domain: c.Domain, Hash: f.SHA256}] = true
		}
	}
	var requested []storage.ChunkKey
	for key := range keys {
		requested = append(requested, key)
	}
	rows, err := m.GetChunks(ctx, requested, true)
	if err != nil || len(rows) != len(requested) {
		t.Fatal("canonical identities missing", err)
	}
	var identities []map[string]any
	for key, row := range rows {
		identities = append(identities, map[string]any{"domain": key.Domain, "hash": key.Hash, "raw_bytes": row.RawBytes, "stored_bytes": row.Location.Range.Length, "offset": row.Location.Range.Offset, "pack_id": row.Location.Pack.ID, "generation": row.Location.Pack.Generation, "location_revision": row.LocationRevision})
	}
	sort.Slice(identities, func(i, j int) bool {
		return fmt.Sprint(identities[i]["domain"], identities[i]["hash"]) < fmt.Sprint(identities[j]["domain"], identities[j]["hash"])
	})
	encode("identities.json", identities)
	repo, _ := filepath.Abs("../..")
	git := func(args ...string) ([]byte, error) {
		c := exec.Command("git", append([]string{"-c", "safe.directory=" + repo}, args...)...)
		c.Dir = repo
		return c.Output()
	}
	commit, err := git("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	_, diffErr := git("diff", "--quiet", "HEAD")
	untracked, err := git("ls-files", "--others", "--exclude-standard", "--", "*.go", "go.mod", "go.sum")
	if err != nil {
		t.Fatal(err)
	}
	binary, _ := os.Executable()
	binaryBytes, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	binarySum := sha256.Sum256(binaryBytes)
	encode("run.json", map[string]any{"schema_version": 1, "status": "passed", "kind": "local-native-engine-metadata-smoke", "source_commit": strings.TrimSpace(string(commit)), "source_dirty": diffErr != nil || len(untracked) > 0, "go_version": runtime.Version(), "test_binary_sha256": hex.EncodeToString(binarySum[:]), "started_at": started, "finished_at": time.Now().UTC(), "backend": "dynamodb-local+s3-moto", "transport": "object-service PUT/GET and authenticated Admin handlers; not a SigV4 front-door benchmark", "scheduler": "no periodic job runner; explicit bounded stats refresh", "dataset_manifest_sha256": fmt.Sprintf("%x", sha256.Sum256(manifestBytes)), "phases": phases, "limitations": []string{"smoke only; no throughput/cost claim", "snapshots approximate, not atomic", "layout metadata-only; full GET hashes recorded separately", "three sequential cohorts, not independent performance trials", "all retained listed bytes include setup/control/history", "temporary restore billing and infrastructure are not measured"}})
	t.Logf("exported %d verified GETs and %d identities to explicit output", len(gets), len(rows))
}
