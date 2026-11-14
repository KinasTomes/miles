package coldrunner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/benchmark"
	"github.com/KinasTomes/miles/internal/benchmarkdata"
	"github.com/KinasTomes/miles/internal/benchmarknative"
	s "github.com/KinasTomes/miles/internal/storage"
)

func phaseFixture(t *testing.T) (benchmarkdata.Manifest, string, Snapshot, map[string][]byte) {
	t.Helper()
	dir := t.TempDir()
	data := map[string][]byte{}
	m := benchmarkdata.Manifest{SchemaVersion: 1, Generator: "fixture", GoVersion: "fixture", Profile: "backup30", Seed: "fixture", Versions: 4, RawBytesPerVersion: 32}
	now := time.Now().UTC()
	view := Snapshot{Binding: benchmarknative.Config{RunID: "fixture", Slot: "cold-rep1", Source: strings.Repeat("a", 40), LogicalBucket: "cold-bucket"}, Bounds: s.R1Bounds{Lower: now.Add(-time.Second), Upper: now.Add(time.Second)}, Files: map[string][]s.ChunkKey{}}
	view.Inventory.Native.Complete = true
	view.Inventory.Native.VersionScope = "all-retained"
	for i := 0; i < 4; i++ {
		name := fmt.Sprintf("random-%d.bin", i)
		body := []byte(strings.Repeat(fmt.Sprint(i), 32))
		hash := sha256.Sum256(body)
		f := benchmarkdata.File{Path: name, Group: "random-distinct", Version: i, Bytes: len(body), SHA256: hex.EncodeToString(hash[:])}
		m.Files = append(m.Files, f)
		m.TotalBytes += int64(len(body))
		data[name] = body
		if e := os.WriteFile(filepath.Join(dir, name), body, 0600); e != nil {
			t.Fatal(e)
		}
		key := s.ChunkKey{Domain: "fixture", Hash: f.SHA256}
		ref := s.PackRef{ID: fmt.Sprintf("pack-%d", i), Generation: "old"}
		view.Files[name] = []s.ChunkKey{key}
		view.Chunks = append(view.Chunks, s.Chunk{Version: 1, Key: key, Location: s.ChunkLocation{Pack: ref}, LocationRevision: 1})
		p := s.PackInfo{Version: 1, Ref: ref, Class: s.Glacier, State: "Cold", MinRetentionUntil: now.Add(90 * 24 * time.Hour), Size: 100}
		view.Packs = append(view.Packs, p)
		view.Physical = append(view.Physical, p)
		view.Inventory.Native.Objects = append(view.Inventory.Native.Objects, s.PhysicalObject{Key: "packs/" + ref.ID, VersionID: "v1", Pack: ref, Category: "pack", Class: s.Glacier, Size: 100}, s.PhysicalObject{Key: "index/" + ref.ID, VersionID: "i1", Pack: ref, Category: "sidecar", Class: s.Standard, Size: 10})
	}
	raw, _ := json.Marshal(m)
	path := filepath.Join(dir, "manifest.json")
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	return m, path, view, data
}
func cloneSnapshot(v Snapshot) Snapshot {
	raw, _ := json.Marshal(v)
	var out Snapshot
	_ = json.Unmarshal(raw, &out)
	return out
}
func warmChunk(v *Snapshot, index int) {
	c := &v.Chunks[index]
	c.Location.Pack.Generation = "new"
	c.LocationRevision++
	p := s.PackInfo{Version: 1, Ref: c.Location.Pack, Class: s.Standard, State: "Warm", ProtectedUntil: time.Now().Add(20 * time.Minute), Size: 50}
	v.Packs = append(v.Packs, p)
	v.Physical = append(v.Physical, p)
}
func TestNativeReplacementProofRejectsTemporaryOrLostColdData(t *testing.T) {
	_, _, before, _ := phaseFixture(t)
	after := cloneSnapshot(before)
	warmChunk(&after, 0)
	if err := VerifyReplacement(before, after, "random-0.bin", after.Bounds.Lower, false); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Snapshot){
		func(v *Snapshot) { v.Chunks[0].LocationRevision = 1 },
		func(v *Snapshot) { v.Packs[len(v.Packs)-1].ProtectedUntil = time.Now() },
		func(v *Snapshot) { v.Physical[len(v.Physical)-1].Class = s.Glacier },
		func(v *Snapshot) { v.Inventory.Native.Objects = v.Inventory.Native.Objects[1:] },
		func(v *Snapshot) { v.Binding.Slot = "foreign-rep1" },
	} {
		v := cloneSnapshot(after)
		mutate(&v)
		if VerifyReplacement(before, v, "random-0.bin", v.Bounds.Lower, false) == nil {
			t.Fatal("invalid native revival proof accepted")
		}
	}
}
func TestProbeSelectionNeedsIndependentChunks(t *testing.T) {
	m, _, v, _ := phaseFixture(t)
	if _, e := SelectProbes(&m, v); e != nil {
		t.Fatal(e)
	}
	v.Files[m.Files[1].Path] = v.Files[m.Files[0].Path]
	if _, e := SelectProbes(&m, v); e == nil {
		t.Fatal("overlapping probes accepted")
	}
}

func TestColdPhasesPUTAndMultipartReviveBeforeCompletion(t *testing.T) {
	for _, phase := range []string{"put-cold", "part-cold", "put-restored", "part-restored"} {
		t.Run(phase, func(t *testing.T) {
			_, manifest, view, data := phaseFixture(t)
			index := map[string]int{"put-cold": 0, "part-cold": 1, "put-restored": 2, "part-restored": 3}[phase]
			if strings.Contains(phase, "restored") {
				for i := range view.Packs {
					view.Packs[i].State = "Restored"
					view.Physical[i].State = "Restored"
					view.Packs[i].RestoredUntil = time.Now().Add(20 * time.Second)
					view.Physical[i].RestoredUntil = view.Packs[i].RestoredUntil
				}
			}
			var mu sync.Mutex
			dest := map[string][]byte{}
			var uploaded []byte
			completed := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				key := strings.TrimPrefix(r.URL.Path, "/cold-bucket/")
				switch {
				case r.Method == "POST" && r.URL.Query().Has("uploads"):
					w.Header().Set("Content-Type", "application/xml")
					fmt.Fprint(w, `<InitiateMultipartUploadResult><UploadId>upload-one</UploadId></InitiateMultipartUploadResult>`)
				case r.Method == "PUT" && r.URL.Query().Get("uploadId") != "":
					uploaded, _ = io.ReadAll(r.Body)
					warmChunk(&view, index)
					w.Header().Set("ETag", `"part"`)
				case r.Method == "POST" && r.URL.Query().Get("uploadId") != "":
					completed = true
					dest[key] = uploaded
					w.Header().Set("Content-Type", "application/xml")
					fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>"complete"</ETag></CompleteMultipartUploadResult>`)
				case r.Method == "PUT":
					dest[key], _ = io.ReadAll(r.Body)
					warmChunk(&view, index)
					w.Header().Set("ETag", `"put"`)
				case r.Method == "GET":
					w.Write(dest[key])
				default:
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			cred := filepath.Join(t.TempDir(), "credentials.json")
			os.WriteFile(cred, []byte(`{"access_key_id":"fixture","secret_access_key":"private"}`), 0600)
			cfg := benchmark.Config{Target: "local-s3-smoke", Endpoint: server.URL, Region: "ap-southeast-1", Bucket: "cold-bucket", CredentialsFile: cred, Environment: benchmark.Environment{Topology: "fixture", Host: "fixture", CPU: "fixture", Disk: "fixture", Network: "loopback", Metadata: "fixture", Encryption: "fixture", CacheState: "fixture", Isolation: "fixture", AsyncWork: "fixture"}}
			snapshots := 0
			capture := func(_ context.Context) (Snapshot, error) {
				mu.Lock()
				defer mu.Unlock()
				snapshots++
				if strings.HasPrefix(phase, "part") && snapshots == 2 && completed {
					t.Fatal("part native proof deferred until Complete")
				}
				return cloneSnapshot(view), nil
			}
			res, err := RunPhase(t.Context(), cfg, PhaseOptions{Phase: phase, Variant: "flexible", Manifest: manifest, Out: filepath.Join(t.TempDir(), "result"), Capture: capture})
			if err != nil || !res.RevivalVerified || !res.ReadVerified || res.Status != "passed" {
				t.Fatal(res, err)
			}
			if got := dest["revival/"+phase+fmt.Sprintf("/random-%d.bin", index)]; string(got) != string(data[fmt.Sprintf("random-%d.bin", index)]) {
				t.Fatal("mutation did not send original client bytes")
			}
		})
	}
}

func TestRestoreStatusIsRepeatableReadOnlyAndIgnoresRetainedCold(t *testing.T) {
	_, manifest, view, _ := phaseFixture(t)
	for run := 0; run < 2; run++ {
		res, err := RunPhase(t.Context(), benchmark.Config{}, PhaseOptions{Phase: "restore-status", Variant: "flexible", Manifest: manifest,
			Out: filepath.Join(t.TempDir(), "status"), Capture: func(context.Context) (Snapshot, error) { return view, nil }})
		if err != nil || res.Status != "waiting" || len(res.Requests) != 0 {
			t.Fatal(res, err)
		}
	}
	for i := range view.Chunks {
		warmChunk(&view, i)
	}
	// Old source generations are deliberately still Cold in the same snapshot.
	res, err := RunPhase(t.Context(), benchmark.Config{}, PhaseOptions{Phase: "restore-status", Variant: "flexible", Manifest: manifest,
		Out: filepath.Join(t.TempDir(), "status"), Capture: func(context.Context) (Snapshot, error) { return view, nil }})
	if err != nil || res.Status != "ready" || len(res.Requests) != 0 {
		t.Fatal(res, err)
	}
}

func TestRestoreStatusNeedsMetadataAndPhysicalExpiryThroughRead(t *testing.T) {
	_, manifest, view, _ := phaseFixture(t)
	for i := range view.Physical {
		view.Physical[i].State = "Restored"
		view.Physical[i].RestoredUntil = view.Bounds.Upper.Add(time.Hour)
	}
	check := func(want string) {
		t.Helper()
		res, err := RunPhase(t.Context(), benchmark.Config{}, PhaseOptions{Phase: "restore-status", Variant: "flexible", Manifest: manifest,
			Out: filepath.Join(t.TempDir(), "status"), Capture: func(context.Context) (Snapshot, error) { return view, nil }})
		if err != nil || res.Status != want || len(res.Requests) != 0 {
			t.Fatal(res, err)
		}
	}
	check("waiting") // S3 is ready while the durable restore job still lags.
	for i := range view.Packs {
		view.Packs[i].State = "Restored"
		view.Packs[i].RestoredUntil = view.Bounds.Upper.Add(20 * time.Second)
	}
	check("waiting") // A temporary copy expiring within the read is insufficient.
	for i := range view.Packs {
		view.Packs[i].RestoredUntil = view.Bounds.Upper.Add(time.Hour)
	}
	check("ready")
}

func TestColdPhaseRejectsTamperedDatasetBeforeObserverOrHTTP(t *testing.T) {
	_, manifest, _, _ := phaseFixture(t)
	os.WriteFile(filepath.Join(filepath.Dir(manifest), "random-0.bin"), []byte("tampered"), 0600)
	called := false
	_, err := RunPhase(t.Context(), benchmark.Config{}, PhaseOptions{Phase: "put-cold", Variant: "flexible", Manifest: manifest,
		Out: filepath.Join(t.TempDir(), "result"), Capture: func(context.Context) (Snapshot, error) { called = true; return Snapshot{}, nil }})
	if err == nil || called {
		t.Fatal("unverified local bytes reached native observer", err)
	}
}

func TestIRReuseNeedsSameLocationAndProtection(t *testing.T) {
	_, _, before, _ := phaseFixture(t)
	before.Packs[0].Class, before.Physical[0].Class = s.GlacierIR, s.GlacierIR
	after := cloneSnapshot(before)
	after.Packs[0].ProtectedUntil = after.Bounds.Lower.Add(16 * time.Minute)
	if err := VerifyReplacement(before, after, "random-0.bin", after.Bounds.Lower, true); err != nil {
		t.Fatal(err)
	}
	warmChunk(&after, 0)
	if VerifyReplacement(before, after, "random-0.bin", after.Bounds.Lower, true) == nil {
		t.Fatal("IR reupload counted as dedup reuse")
	}
}

func TestRestoreRequestReturnsWaitingWithoutPollingAndRetainsUnknownIntent(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint("lost-", lost), func(t *testing.T) {
			_, manifest, view, _ := phaseFixture(t)
			restores := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/xml")
				if r.Method == "GET" {
					w.WriteHeader(403)
					fmt.Fprint(w, `<Error><Code>InvalidObjectState</Code></Error>`)
				} else if r.Method == "POST" && r.URL.Query().Has("restore") {
					restores++
					if lost {
						w.WriteHeader(503)
						fmt.Fprint(w, `<Error><Code>ServiceUnavailable</Code></Error>`)
					} else {
						w.WriteHeader(202)
					}
				} else {
					t.Errorf("unexpected poll or request: %s", r.Method)
					w.WriteHeader(500)
				}
			}))
			defer server.Close()
			cred := filepath.Join(t.TempDir(), "credentials.json")
			os.WriteFile(cred, []byte(`{"access_key_id":"fixture","secret_access_key":"private"}`), 0600)
			cfg := benchmark.Config{Target: "local-s3-smoke", Endpoint: server.URL, Region: "ap-southeast-1", Bucket: "cold-bucket", CredentialsFile: cred, Environment: benchmark.Environment{Topology: "fixture", Host: "fixture", CPU: "fixture", Disk: "fixture", Network: "loopback", Metadata: "fixture", Encryption: "fixture", CacheState: "fixture", Isolation: "fixture", AsyncWork: "fixture"}}
			out := filepath.Join(t.TempDir(), "result")
			res, err := RunPhase(t.Context(), cfg, PhaseOptions{Phase: "restore-request", Variant: "flexible", Manifest: manifest, Out: out,
				Capture: func(context.Context) (Snapshot, error) { return view, nil }})
			if lost {
				if err == nil || restores != 1 || res.Status != "failed" {
					t.Fatal(res, restores, err)
				}
				if _, e := os.Stat(filepath.Join(out, "restore-intent-000.json")); e != nil {
					t.Fatal(e)
				}
			} else if err != nil || restores != 4 || res.Status != "waiting" {
				t.Fatal(res, restores, err)
			}
		})
	}
}
