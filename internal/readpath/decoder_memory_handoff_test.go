package readpath

import (
	"context"
	"errors"
	"testing"

	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestDecoderMemoryHandoffRejectsBeforeFetch(t *testing.T) {
	for _, scope := range []string{"node", "bucket", "request"} {
		t.Run(scope, func(t *testing.T) {
			f := newVerifiedSchedule(t, 0)
			// Schedule zstd first; deny even one decoder's workspace.
			f.p.Chunks = f.p.Chunks[1:2]
			f.e.entries = f.e.entries[1:2]
			f.p.Chunks[0].Slice = storage.ByteRange{Length: f.p.Chunks[0].Ref.RawBytes}
			f.p.Range = storage.ByteRange{Offset: f.p.Chunks[0].Ref.LogicalOffset, Length: f.p.Chunks[0].Ref.RawBytes}
			m, err := pack.EstimateDecodeMemory(f.e.entries[0])
			if err != nil {
				t.Fatal(err)
			}
			rb := budget(16 << 20)
			switch scope {
			case "node":
				f.a.limits.Node.Budget.Capacity.WorkspaceBytes = m.WorkspaceBytes - 1
			case "bucket":
				f.a.limits.Bucket.Budget.Capacity.WorkspaceBytes = m.WorkspaceBytes - 1
			case "request":
				rb.Capacity.WorkspaceBytes = m.WorkspaceBytes - 1
			}
			// Prefetch cap must also remain below the reduced capacity.
			f.a.limits.Node.Budget.Prefetch.WorkspaceBytes = 0
			f.a.limits.Bucket.Budget.Prefetch.WorkspaceBytes = 0
			rb.Prefetch.WorkspaceBytes = 0
			err = RunReadAhead(context.Background(), f.a, f.b, rb, f.p, 1, f.e,
				func(context.Context, int, []byte) error { t.Error("emitted without admission"); return nil })
			if !errors.Is(err, ErrExhausted) {
				t.Fatal(err)
			}
			if len(f.s.calls()) != 0 || f.e.verified.Load() != 0 {
				t.Fatal("side effects before admission")
			}
			drained(t, f.a)
		})
	}
}

func TestDecoderMemoryHandoffCancellation(t *testing.T) {
	for _, stage := range []string{"before decode", "after decode", "emitter"} {
		t.Run(stage, func(t *testing.T) {
			f := newVerifiedSchedule(t, 0)
			entered, release := make(chan struct{}), make(chan struct{})
			gate := func() { close(entered); <-release }
			f.e.hook = func(_ context.Context, i int, s string) error {
				if i == 1 && s == stage {
					gate()
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- RunReadAhead(ctx, f.a, f.b, budget(16<<20), f.p, 1, f.e,
					func(ctx context.Context, i int, _ []byte) error {
						if i == 1 && stage == "emitter" {
							gate()
						}
						return ctx.Err()
					})
			}()
			<-entered
			cancel()
			m, err := pack.EstimateDecodeMemory(f.e.entries[1])
			if err != nil {
				t.Fatal(err)
			}
			f.a.mu.Lock()
			slots, workspace := f.a.used.total[0], f.a.used.total[5]
			f.a.mu.Unlock()
			if slots != 1 || workspace != m.WorkspaceBytes {
				t.Error("released decoder reservation before worker/emitter quiescence")
			}
			select {
			case err := <-done:
				t.Error("returned before quiescence", err)
			default:
			}
			close(release)
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			drained(t, f.a)
		})
	}
}
