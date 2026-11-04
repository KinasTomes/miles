package recovery

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/metadata/operations"
)

type measuredObjects struct {
	ImmutableStore
	reads, bytes int
	delay        time.Duration
}

func (m *measuredObjects) Read(ctx context.Context, key string) ([]byte, error) {
	m.reads++
	timer := time.NewTimer(m.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
	}
	b, e := m.ImmutableStore.Read(ctx, key)
	m.bytes += len(b)
	return b, e
}

// Explicit opt-in keeps the latency experiment out of ordinary unit tests.
// This measures artifact I/O, not AWS latency or throughput guarantees.
func TestCheckpointLongHistoryReadBudget(t *testing.T) {
	if os.Getenv("MILES_CHECKPOINT_SCALE") != "1" {
		t.Skip("opt-in long-history I/O experiment")
	}
	w, first, packs := liveCheckpointFixture(t)
	image, e := w.Load(t.Context(), first)
	if e != nil {
		t.Fatal(e)
	}
	previous := image.Snapshot.Seal.Reference.SHA256
	for epoch := uint64(1); epoch < 720; epoch++ {
		row, e := operations.Projection("UsersQuota", "owner", epoch+1, struct {
			Owner string
			Limit int64
		}{"owner", 100}, false)
		if e != nil {
			t.Fatal(e)
		}
		seal := fixtureSeal(t, w, epoch, previous, []Effect{row})
		previous = seal.Reference.SHA256
	}
	base, e := w.Build(t.Context(), first, 718)
	if e != nil {
		t.Fatal(e)
	}
	measured := &measuredObjects{ImmutableStore: w.Objects, delay: time.Millisecond}
	w.Objects = measured
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Minute)
	defer cancel()
	start := time.Now()
	root, e := w.Build(ctx, base, 719)
	if e != nil {
		t.Fatal(e)
	}
	buildReads := measured.reads
	buildTime := time.Since(start)
	if _, e = w.Load(ctx, root); e != nil {
		t.Fatal(e)
	}
	loadReads := measured.reads - buildReads
	retention := Retention{Checkpointer: w, Store: &memoryCatalog{}, Packs: packs}
	// Publish from genesis when no prior support exists; use its canonical base.
	root, e = w.Build(ctx, AttachmentReference{Key: genesisKey(w.Catalog), Reference: w.Catalog.Genesis}, 719)
	if e != nil {
		t.Fatal(e)
	}
	before := measured.reads
	if e = retention.Publish(ctx, root); e != nil {
		t.Fatal(e)
	}
	elapsed := time.Since(start)
	t.Logf("epochs=720 events>=720 latency/read=%s incremental_build_reads=%d build_time=%s load_reads=%d publish_reads=%d total_reads=%d bytes=%d elapsed=%s", measured.delay, buildReads, buildTime, loadReads, measured.reads-before, measured.reads, measured.bytes, elapsed)
	if elapsed >= 8*time.Minute {
		t.Fatal("job step budget exceeded")
	}
}
