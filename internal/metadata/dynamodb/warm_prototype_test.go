package dynamodb

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage/contracttest"
)

// This is an adapter timing probe, not HTTP PUT throughput. Existing fixture
// authority is deliberately confined to _test; production startup is unchanged.
func TestLocalWarmMetadataPrototype(t *testing.T) {
	f := contracttest.NewFixture()
	m, client := fixtureStore(t, f)
	peer, err := New(Options{API: client, Tables: m.tables, Now: m.now, Authority: m.authority})
	if err != nil {
		t.Fatal(err)
	}
	type sample struct {
		Iteration int    `json:"iteration"`
		Operation string `json:"operation"`
		LatencyNS int64  `json:"latency_ns"`
	}
	rows := []sample{}
	ctx := context.Background()
	for i := range 10 {
		r := f.Request(fmt.Sprintf("warm-probe-%d", i), uint64(i))
		start := time.Now()
		reserve := int64(0)
		if i == 0 {
			reserve = r.Head.Size
		}
		contracttest.Prepare(t, m, f, r, reserve)
		rows = append(rows, sample{i, "prepare_manifest_and_reservation", time.Since(start).Nanoseconds()})
		start = time.Now()
		committed, e := m.CommitObject(ctx, r)
		rows = append(rows, sample{i, "commit_object", time.Since(start).Nanoseconds()})
		if e != nil || committed.Head.Revision != uint64(i+1) {
			t.Fatal(committed, e)
		}
		start = time.Now()
		head, e := peer.GetObject(ctx, r.Head.Object)
		if e != nil {
			t.Fatal(e)
		}
		page, e := peer.GetManifestPage(ctx, head.ManifestID, 0)
		rows = append(rows, sample{i, "peer_head_and_manifest_read", time.Since(start).Nanoseconds()})
		if e != nil || head.Size != 4 || head.Revision != uint64(i+1) || len(page.Chunks) != 1 || page.Chunks[0].Key != f.Chunk("default").Key {
			t.Fatal("peer did not observe committed manifest", e)
		}
		if event, e := peer.GetOutbox(ctx, r.Event.ID); e != nil || event.Revision != head.Revision {
			t.Fatal("missing commit event", event, e)
		}
		if replay, e := m.CommitObject(ctx, r); e != nil || !replay.AlreadyCommitted {
			t.Fatal("commit replay changed result", replay, e)
		}
	}
	quota, err := peer.GetQuota(ctx, f.Quota.OwnerID)
	if err != nil || quota.Used != 4 || quota.Reserved != 0 {
		t.Fatal("overwrite or replay double-counted quota", quota, err)
	}
	path := os.Getenv("MILES_WARM_METADATA_ARTIFACT")
	if path == "" {
		return
	}
	artifact := map[string]any{"schema_version": 1, "kind": "native-adapter-timing-probe", "metadata_backend": "dynamodb-local", "status": "passed", "go_version": runtime.Version(), "samples": rows, "verified_head_revisions": 10, "mvp_threshold_applied": false, "limitations": []string{"4-byte manifest fixture; not the Warm dataset or HTTP PUT/GET throughput", "Test-only lease authority; no production R1/R2 runtime, physical S3 I/O, AWS latency or billed units measured", "Table creation, cleanup, outbox verification and replay checks excluded from samples"}}
	b, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(append(b, '\n'))
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
}
