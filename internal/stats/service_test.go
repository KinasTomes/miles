package stats

import (
	"context"
	"errors"
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
	"time"
)

type boundedSource struct{ started chan struct{} }

func (f boundedSource) ObservePage(ctx context.Context, _ s.ObservationRequest) (s.ObservationPage, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return s.ObservationPage{}, ctx.Err()
}
func (boundedSource) GetManifestPage(context.Context, string, uint32) (s.ManifestPage, error) {
	panic("unexpected")
}
func (boundedSource) GetChunks(context.Context, []s.ChunkKey, bool) (map[s.ChunkKey]s.Chunk, error) {
	panic("unexpected")
}
func (boundedSource) GetObject(context.Context, s.ObjectKey) (s.ObjectHead, error) {
	panic("unexpected")
}
func (boundedSource) GetBucket(context.Context, string) (s.Bucket, error) { panic("unexpected") }

type emptyInventory struct{}

func (emptyInventory) CollectInventory(context.Context, int) (s.PhysicalInventory, error) {
	return s.PhysicalInventory{Complete: true, VersionScope: "all-retained"}, nil
}
func TestCollectorCancellationJoinsWork(t *testing.T) {
	source := boundedSource{started: make(chan struct{}, 1)}
	svc, err := New(Options{Store: source, Inventory: emptyInventory{}, Now: time.Now, MaxRecords: 10, MaxReferences: 10, MaxPhysicalObjects: 10, Timeout: time.Minute, Interval: time.Hour, MaxAge: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); svc.Run(ctx) }()
	<-source.started
	cancel()
	<-done
	if _, err = svc.Global(context.Background()); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("cancelled collection published", err)
	}
}
func TestRejectInvalidCollectionBudgets(t *testing.T) {
	if _, err := New(Options{Store: boundedSource{}, Inventory: emptyInventory{}, Now: time.Now, MaxRecords: 1 << 30, MaxReferences: 10, MaxPhysicalObjects: 10, Timeout: time.Minute, Interval: time.Minute, MaxAge: time.Minute}); !errors.Is(err, s.ErrInvalid) {
		t.Fatal(err)
	}
}
