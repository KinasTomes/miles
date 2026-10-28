package readpath

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
)

// Both actors use ONE Badger, physical S3 adapter and coordination gate. Only
// the S3 SDK service is simulated; no boolean authorization or pin substitute.
func TestWarmR1ReadOverlapsManagedOverwriteAndDelete(t *testing.T) {
	for _, deleting := range []bool{false, true} {
		t.Run(map[bool]string{false: "overwrite", true: "delete"}[deleting], func(t *testing.T) {
			ctx := context.Background()
			f := newWarmFixture(t, 0)
			wantOld := f.body
			if deleting {
				f.request.Range = &storage.ByteRange{Offset: 2, Length: int64(len(f.body) - 5)}
				wantOld = f.body[2 : len(f.body)-3]
			}
			seed := contracttest.NewFixture()
			limits := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
			g, err := coordination.NewR1Gate(f.clock, seed.Lease.Fence.ServiceIncarnation, limits)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := coordination.NewWriterRegistry(g, limits, coordination.Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			_, sidecar, err := pack.Inspect(f.backend.body, f.ref)
			if err != nil {
				t.Fatal(err)
			}
			prefix := f.ref.ID[:2]
			service := &handoffS3{objects: map[string][]byte{
				"packs/" + prefix + "/" + f.ref.ID + "/" + f.ref.Generation:                bytes.Clone(f.backend.body),
				"packs-index/" + prefix + "/" + f.ref.ID + "/" + f.ref.Generation + ".idx": sidecar,
			}}
			physical, err := packs3.New(service, "shared-bucket", "")
			if err != nil {
				t.Fatal(err)
			}
			f.reader.options.Packs = physical
			f.reader.options.Gate = g
			f.reader.options.Admission = admission(t, 128<<20, 128<<20) // explicit test-only concurrent budget
			firstEmitted, proceed, readerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(proceed) }) }
			var oldResult WarmResult
			var oldBody []byte
			var oldErr error
			go func() {
				defer close(readerDone)
				blocked := false
				oldResult, oldErr = f.reader.Read(ctx, f.request, func(_ context.Context, b []byte) error {
					oldBody = append(oldBody, b...)
					if !blocked {
						blocked = true
						close(firstEmitted)
						<-proceed
					}
					return nil
				})
			}()
			defer func() { release(); <-readerDone }()
			select {
			case <-firstEmitted:
			case <-readerDone:
				t.Fatal("reader failed before race barrier", oldErr)
			}
			if !errors.Is(g.Retire(f.ref), storage.ErrConflict) {
				t.Fatal("old reader has no physical pin")
			}
			q := seed.Request("managed-concurrent-write", 1)
			oldHead, err := f.metadata.GetObject(ctx, q.Head.Object)
			if err != nil {
				t.Fatal(err)
			}
			session, err := write.BeginWriteSession(ctx, write.SessionOptions{Registry: registry, Metadata: f.metadata, Packs: physical, Fence: seed.Lease.Fence, Deadline: f.request.Deadline, Object: q.Head.Object, OpID: q.Head.OpID, CredentialID: q.CredentialID, ProtectedUntil: seed.Now.Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			var newBody []byte
			var size int64
			if deleting {
				q.Head = storage.ObjectHead{Version: 1, Object: q.Head.Object, Domain: seed.Bucket.Domain, OpID: q.Head.OpID, Deleted: true}
			} else {
				newBody = bytes.Repeat([]byte("new-managed-object\n"), 2000)
				id, err := write.NewManifestID()
				if err != nil {
					t.Fatal(err)
				}
				pipeline := write.Pipeline{Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10}, Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
				prepared, err := session.Prepare(pipeline, bytes.NewReader(newBody), id)
				if err != nil {
					t.Fatal(err)
				}
				size = prepared.Size
			}
			if err := session.Reserve(storage.Reservation{Version: 1, Revision: 1, State: storage.ReservationActive, ID: q.ReservationID, OpID: q.Head.OpID, Object: q.Head.Object, OwnerID: seed.Bucket.OwnerID, BaseRevision: oldHead.Revision, BaseSize: oldHead.Size, Bytes: max(0, size-oldHead.Size), StagedBytes: size, Deadline: seed.Now.Add(time.Hour), Writer: q.Writer}); err != nil {
				t.Fatal(err)
			}
			var published storage.CommitResult
			if deleting {
				published, err = session.Delete(q)
			} else {
				published, err = session.Commit(q)
			}
			if err != nil || published.Head.Revision != 2 {
				t.Fatal(published, err)
			}
			if err := session.Close(); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(g.Retire(f.ref), storage.ErrConflict) {
				t.Fatal("writer completion released reader's generation")
			}
			newRequest := f.request
			newRequest.Range = nil
			newResult, got, err := f.read(ctx, newRequest)
			if deleting {
				if !errors.Is(err, storage.ErrNotFound) || len(got) != 0 {
					t.Fatal("tombstone bypassed through cache", err)
				}
			} else {
				if err != nil || newResult.Head.Revision != 2 || !bytes.Equal(got, newBody) {
					t.Fatal("new admission did not see new head", newResult, err)
				}
			}
			if !errors.Is(coordination.ProtectPackStore(physical).DeletePack(ctx, f.ref), coordination.ErrMaintenanceDisabled) {
				t.Fatal("MVP enabled physical delete")
			}
			release()
			<-readerDone
			if oldErr != nil || oldResult.Head.Revision != 1 || !bytes.Equal(oldBody, wantOld) {
				t.Fatal("bound reader changed namespace midstream", oldResult, oldErr)
			}
			drained(t, f.reader.options.Admission)
			if err := g.Retire(f.ref); err != nil {
				t.Fatal("old generation did not quiesce", err)
			}
			if f.clock.Pending() != 0 {
				t.Fatal("read/write heartbeat leaked")
			}
		})
	}
}
