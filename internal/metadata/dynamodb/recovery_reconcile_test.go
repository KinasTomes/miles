package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func recoveryCommit(t *testing.T, f *recoveryFixture, label string) s.CommitRequest {
	t.Helper()
	ctx := context.Background()
	seed := contracttest.NewFixture()
	r := seed.Request(label, 0)
	r.Writer = f.acquire(t, "producer-"+label, "writer", r.Head.OpID, 1)
	seed.Lease.Fence = r.Writer
	r.Producer = &s.MutationContext{HistoryID: f.catalog.History, Epoch: 0, LogicalOpID: r.Head.OpID, MutationID: r.Head.OpID, Object: r.Head.Object, Writer: r.Writer}
	ctx = s.WithMutation(ctx, *r.Producer)
	if err := f.m.BindR1(ctx, r.Writer, r.Head); err != nil {
		t.Fatal(err)
	}
	if err := f.m.update(ctx, func(tx *transaction) error {
		if err := put(tx, key("bucket-namespace", seed.Bucket.Name), s.BucketNamespace{Version: 1}); err != nil {
			return err
		}
		return put(tx, chunkKey(seed.Chunk(seed.Bucket.Domain).Key), seed.Chunk(seed.Bucket.Domain))
	}); err != nil {
		t.Fatal(err)
	}
	page := s.ManifestPage{Version: 1, ManifestID: r.Head.ManifestID, Domain: r.Head.Domain, Chunks: []s.ChunkRef{{Key: seed.Chunk(seed.Bucket.Domain).Key, RawBytes: 4}}}
	if err := f.m.WriteManifestPages(ctx, []s.ManifestPage{page}, r.Writer); err != nil {
		t.Fatal("prepare manifest", err)
	}
	if err := f.m.ReserveLogicalQuota(ctx, s.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: "owner", Object: r.Head.Object, Bytes: 4, StagedBytes: 4, Revision: 1, State: s.ReservationActive, Deadline: seed.Now.Add(30 * time.Minute), Writer: r.Writer}); err != nil {
		t.Fatal("reserve", err)
	}
	if err := f.m.BeginR1Action(ctx, r.Writer, "commit/"+r.Head.OpID); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLocalRecoveryCutReconcilesClosedCommitAfterRestart(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "dispatch-not-committed"
		if committed {
			name = "committed-response-lost"
		}
		t.Run(name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			ctx := context.Background()
			r := recoveryCommit(t, f, name)
			original := f.m.api
			f.m.api = &dispatchFault{API: original, objectTable: f.m.tables["Objects"], before: !committed, after: committed}
			if _, err := f.m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
				t.Fatal("fault did not leave an unknown commit", err)
			}
			f.m.api = original
			var delayed *ddb.TransactWriteItemsInput
			if !committed {
				delayed = savedAttempt(t, f.m, r.Head.OpID)
			}
			if err := f.m.CloseR1(ctx, r.Writer); err != nil {
				t.Fatal(err)
			}
			// A different Store has no request/session state from the original node.
			cut := f.request(0)
			closeRecovery(t, f.peer, cut)
			lease, err := f.peer.GetR1Lease(ctx, r.Writer.LeaseID)
			if err != nil || lease.State != "Closed" || len(lease.Pending) != 0 || len(lease.Roots) != 1 {
				t.Fatal("closed producer was not reconciled", lease, err)
			}
			q, err := f.peer.GetQuota(ctx, "owner")
			used := int64(0)
			if committed {
				used = 4
			}
			if err != nil || q.Reserved != 0 || q.Staged != 0 || q.Used != used {
				t.Fatal("quota differs from the original commit outcome", q, err)
			}
			if delayed != nil {
				if _, err := original.TransactWriteItems(ctx, delayed); !isKnownCancellation(err) {
					t.Fatal("late original dispatch escaped fencing", err)
				}
			}
			if _, err := f.service(f.peer, f.worker).CloseAndSeal(ctx, cut); err != nil {
				t.Fatal("reconciled epoch did not seal", err)
			}
		})
	}
}

func TestLocalR1ReconciliationClosureACKAndUnknownIO(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	r := recoveryCommit(t, f, "reconcile-closure-ack")
	original := f.m.api
	f.m.api = &dispatchFault{API: original, objectTable: f.m.tables["Objects"], before: true}
	if _, err := f.m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	f.m.api = original
	if err := f.m.ReconcileR1(ctx, r.Writer); !errors.Is(err, s.ErrFenced) {
		t.Fatal("active writer was reconciled", err)
	}
	if err := f.m.CloseR1(ctx, r.Writer); err != nil {
		t.Fatal(err)
	}
	fault := &closeFault{API: original, before: true}
	f.m.api = fault
	if err := f.m.ReconcileR1(ctx, r.Writer); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	q, _ := f.peer.GetQuota(ctx, "owner")
	lease, _ := f.peer.GetR1Lease(ctx, r.Writer.LeaseID)
	if q.Reserved != 4 || q.Staged != 4 || len(lease.Pending) != 1 {
		t.Fatal("unknown closure released protection", q, lease)
	}
	fault.before, fault.after = false, true
	if err := f.m.ReconcileR1(ctx, r.Writer); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("lost closure ACK not injected", err)
	}
	if err := f.peer.ReconcileR1(ctx, r.Writer); err != nil {
		t.Fatal("retry of committed closure", err)
	}
	q, _ = f.peer.GetQuota(ctx, "owner")
	if q.Reserved != 0 || q.Staged != 0 || q.Used != 0 {
		t.Fatal("closure retry changed quota twice", q)
	}

	w := f.acquire(t, "unknown-physical", "writer", "unresolved-pack", 1)
	if err := f.peer.BeginR1Action(ctx, w, "data/pack"); err != nil {
		t.Fatal(err)
	}
	if err := f.peer.CloseR1(ctx, w); err != nil {
		t.Fatal(err)
	}
	if err := f.peer.ReconcileR1(ctx, w); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("unknown physical I/O was erased", err)
	}
	lease, _ = f.peer.GetR1Lease(ctx, w.LeaseID)
	if len(lease.Pending) != 1 {
		t.Fatal("physical action lost its protection", lease)
	}
}

func TestLocalRecoveryHourlyCutReconcilesCrashedCommit(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	r := recoveryCommit(t, f, "crashed-hourly-commit")
	original := f.m.api
	f.m.api = &dispatchFault{API: original, objectTable: f.m.tables["Objects"], before: true}
	if _, err := f.m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	f.m.api = original
	// The original process never closes its lease. A fresh worker reaches the
	// persisted hourly deadline and must reconcile it without releasing I/O pins.
	f.utc.Advance(time.Hour + time.Second)
	f.worker = f.acquire(t, "hourly-successor", "recovery", "", 1)
	epoch, due, err := f.peer.RecoveryCutDue(ctx, f.catalog.History, f.worker)
	if err != nil || epoch != 0 || !due {
		t.Fatal("hourly cut not due", epoch, due, err)
	}
	if _, err := f.service(f.peer, f.worker).CloseAndSeal(ctx, f.request(0)); err != nil {
		t.Fatal("hourly cut did not reconcile and seal", err)
	}
	lease, _ := f.peer.GetR1Lease(ctx, r.Writer.LeaseID)
	if lease.State != "Closed" || len(lease.Pending) != 0 || len(lease.Roots) != 1 {
		t.Fatal("crash recovery lost retained roots", lease)
	}
	e, err := f.peer.GetRecoveryEpoch(ctx, f.catalog.History, 0)
	if err != nil || e.State != "Sealed" {
		t.Fatal(e, err)
	}
}
