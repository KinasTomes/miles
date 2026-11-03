package dynamodb

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/multipart/core"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestLocalMultipartTakeoverSettlesOldRecoveryProducer(t *testing.T) {
	for _, scenario := range []struct {
		name                            string
		closed, before, after, physical bool
	}{
		{name: "closed-owner", closed: true}, {name: "live-owner"},
		{name: "before-send", before: true}, {name: "lost-ack", after: true},
		{name: "unknown-physical-io", physical: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			multipartTakeoverProducer(t, scenario.closed, scenario.before, scenario.after, scenario.physical)
		})
	}
}

func multipartTakeoverProducer(t *testing.T, closed, before, after, physical bool) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	object := contracttest.NewFixture().Request("multipart-takeover", 0).Head.Object
	uploadID, createOp, completeOp := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
	admit := func(id, op string) (context.Context, s.WriterFence) {
		w := f.acquire(t, id, "writer", op, 1)
		c := s.WithMutation(ctx, s.MutationContext{HistoryID: f.catalog.History, Epoch: 0, LogicalOpID: op, MutationID: op, Object: object, Writer: w})
		if err := f.m.BindR1(c, w, s.ObjectHead{Object: object, ManifestID: uploadID}); err != nil {
			t.Fatal(err)
		}
		return c, w
	}
	createCtx, creator := admit("multipart-create", createOp)
	u, err := f.m.CreateUpload(createCtx, s.CreateUploadRequest{Request: s.MultipartRequest{UploadID: uploadID, Credential: "writer-key", OpID: createOp, Writer: creator}, Upload: s.MultipartUpload{ID: uploadID, Object: object, ReservationID: "takeover-reservation", ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT"}})
	if err != nil {
		t.Fatal(err)
	}
	f.release(t, creator)
	oldCtx, old := admit("multipart-old", completeOp)
	in := core.Input{V: 1, BucketID: object.BucketID, Key: object.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType, Parts: []core.Part{{PartNumber: 1, ETag: strings.Repeat("a", 32)}}}
	owner := strings.Repeat("d", 32)
	if err := f.m.BindR1(oldCtx, old, s.ObjectHead{Object: object, ManifestID: owner}); err != nil {
		t.Fatal(err)
	}
	d, err := manifest.WriteCompletionStream(manifest.CompletionInput, owner, bytes.NewReader(in.Canonical()), func(n uint32, b []byte) error {
		return f.m.WriteCompletionPage(oldCtx, manifest.CompletionInput, owner, n, b, old)
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err = f.m.FreezeCompletion(oldCtx, s.FreezeRequest{Request: s.MultipartRequest{UploadID: u.ID, Credential: "writer-key", OpID: completeOp, Writer: old, ExpectedRevision: u.Revision}, Input: d})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.m.BeginR1Action(oldCtx, old, "commit/"+completeOp); err != nil {
		t.Fatal(err)
	}
	if physical {
		if err = f.m.BeginR1Action(oldCtx, old, "data/pack"); err != nil {
			t.Fatal(err)
		}
	}
	if closed {
		if err = f.m.CloseR1(ctx, old); err != nil {
			t.Fatal(err)
		}
	}
	quota, err := f.m.GetQuota(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	f.utc.Advance(66 * time.Second)
	nextCtx, next := admit("multipart-successor", completeOp)
	request := s.MultipartRequest{UploadID: u.ID, Credential: "writer-key", OpID: completeOp, Writer: next, ExpectedRevision: u.Revision, Fence: u.Fence}
	original := f.peer.api
	f.peer.api = &takeoverResponseFault{API: original, before: before, after: after}
	u, err = f.peer.ClaimCompletion(nextCtx, request)
	f.peer.api = original
	if before || after {
		if !errors.Is(err, s.ErrUnavailable) {
			t.Fatal("fault not injected", err)
		}
		u, err = f.peer.GetUpload(ctx, request.UploadID)
		if err != nil {
			t.Fatal(err)
		}
		lease, e := f.peer.GetR1Lease(ctx, old.LeaseID)
		if e != nil {
			t.Fatal(e)
		}
		if before {
			if u.Writer != old || len(lease.Pending) != 1 || lease.State != "Active" {
				t.Fatal("handoff partially committed", u, lease)
			}
			u, err = f.peer.ClaimCompletion(nextCtx, request)
		} else if u.Writer != next || lease.State != "Closed" || len(lease.Pending) != 0 {
			t.Fatal("lost ACK hid non-atomic closure", u, lease)
		}
	}
	if err != nil {
		t.Fatal("takeover", err)
	}
	if afterQuota, e := f.peer.GetQuota(ctx, "owner"); e != nil || afterQuota != quota {
		t.Fatal("handoff changed staging accounting", afterQuota, quota, e)
	}
	if e := f.peer.BeginR1Action(ctx, old, "commit/"+completeOp); !errors.Is(e, s.ErrFenced) {
		t.Fatal("old owner can register another dispatch", e)
	}
	if physical {
		lease, e := f.peer.GetR1Lease(ctx, old.LeaseID)
		if e != nil || lease.State != "Closed" || len(lease.Pending) != 1 || lease.Pending[0] != "data/pack" || len(lease.Roots) != 2 {
			t.Fatal("unknown I/O lost protection", lease, e)
		}
		if e = f.peer.ReconcileR1(ctx, old); !errors.Is(e, s.ErrR1Busy) {
			t.Fatal("physical unknown incorrectly settled", e)
		}
		return
	}
	ledger, err := f.peer.GetReservation(ctx, u.ReservationID)
	if err != nil || ledger.Writer != next {
		t.Fatal("reservation not transferred", ledger, err)
	}
	if err = f.peer.ReconcileR1(ctx, old); err != nil {
		t.Fatal("takeover stranded old recovery producer", err)
	}
	oldLease, err := f.peer.GetR1Lease(ctx, old.LeaseID)
	if err != nil || len(oldLease.Pending) != 0 || len(oldLease.Roots) != 2 || oldLease.State != "Closed" {
		t.Fatal("takeover discarded roots or retained commit", oldLease, err)
	}
	f.release(t, next)
	if _, err = f.service(f.peer, f.worker).CloseAndSeal(ctx, f.request(0)); err != nil {
		t.Fatal("takeover prevented epoch sealing", err)
	}
}

func recoveryCommit(t *testing.T, f *recoveryFixture, label string) s.CommitRequest {
	t.Helper()
	ctx := context.Background()
	seed := contracttest.NewFixture()
	r := seed.Request(label, 0)
	r.Writer = f.acquire(t, "producer-"+label, "writer", r.Head.OpID, 1)
	r = prepareRecoveryCommit(t, f, r)
	if err := f.m.BeginR1Action(ctx, r.Writer, "commit/"+r.Head.OpID); err != nil {
		t.Fatal(err)
	}
	return r
}

func prepareRecoveryCommit(t *testing.T, f *recoveryFixture, r s.CommitRequest) s.CommitRequest {
	t.Helper()
	ctx := context.Background()
	seed := contracttest.NewFixture()
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

func TestLocalR1CommitClosurePreservesMultipartReservation(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	r := contracttest.NewFixture().Request("multipart-owned-reservation", 0)
	r.Writer = f.acquire(t, "multipart-owner", "writer", r.Head.OpID, 1)
	v := s.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: "owner", Object: r.Head.Object, Revision: 1, State: s.ReservationActive, Bytes: 4, StagedBytes: 4, Deadline: f.utc.Now().Add(time.Minute), Writer: r.Writer, UploadID: "multipart-owned-upload"}
	if err := f.m.ReserveLogicalQuota(ctx, v); err != nil {
		t.Fatal(err)
	}
	if err := f.m.BeginR1Action(ctx, r.Writer, "commit/"+r.Head.OpID); err != nil {
		t.Fatal(err)
	}
	if err := f.m.CloseR1(ctx, r.Writer); err != nil {
		t.Fatal(err)
	}
	if err := f.peer.ReconcileR1(ctx, r.Writer); err != nil {
		t.Fatal(err)
	}
	got, err := f.peer.GetReservation(ctx, v.ID)
	q, quotaErr := f.peer.GetQuota(ctx, "owner")
	if err != nil || quotaErr != nil || got != v || q.Reserved != 4 || q.Staged != 4 || q.Used != 0 {
		t.Fatal("R1 reconciliation consumed multipart-owned staging", got, q, err, quotaErr)
	}
}

type r1ReconcileFault struct {
	s.R1Store
	fail bool
}

func (a *r1ReconcileFault) ReconcileR1(ctx context.Context, f s.WriterFence) error {
	if a.fail {
		return s.ErrUnavailable
	}
	return a.R1Store.ReconcileR1(ctx, f)
}

func TestLocalRegistryQuiescenceReconcilesSharedCommit(t *testing.T) {
	for _, committed := range []bool{false, true} {
		name := "not-committed"
		if committed {
			name = "committed-ack-lost"
		}
		t.Run(name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			ctx := context.Background()
			seam := &r1ReconcileFault{R1Store: f.m}
			limits := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
			gate, err := coordination.NewSharedR1Gate(f.local, f.catalog.Incarnation, "node", limits, seam, f.utc)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := coordination.NewWriterRegistry(gate, limits, coordination.Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			r := contracttest.NewFixture().Request(name, 0)
			deadline, _ := f.local.Now().Instant.Add(time.Minute)
			_, writer, err := registry.AdmitProducer(ctx, r.Head.Object, r.Head.OpID, deadline)
			if err != nil {
				t.Fatal(err)
			}
			work, err := writer.BeginOperation(ctx, r.Head.Object, r.Head.OpID)
			if err != nil {
				t.Fatal(err)
			}
			r.Writer = writer.Fence()
			r = prepareRecoveryCommit(t, f, r)
			original := f.m.api
			f.m.api = &dispatchFault{API: original, objectTable: f.m.tables["Objects"], before: !committed, after: committed}
			if err := writer.Do(ctx, "commit/"+r.Head.OpID, func(s.WriterFence) error {
				_, e := f.m.CommitObject(ctx, r)
				return e
			}); !errors.Is(err, s.ErrUnavailable) {
				t.Fatal(err)
			}
			f.m.api = original
			if err := registry.Quiescent(); !errors.Is(err, coordination.ErrWorkPending) {
				t.Fatal("executing work was released", err)
			}
			lease, _ := f.peer.GetR1Lease(ctx, r.Writer.LeaseID)
			if lease.State != "Active" {
				t.Fatal("quiescence probe closed active work", lease)
			}
			work.Finish()
			writer.CloseFence()
			seam.fail = true
			if err := registry.Quiescent(); !errors.Is(err, s.ErrUnavailable) {
				t.Fatal("unconfirmed reconciliation reported clean", err)
			}
			lease, _ = f.peer.GetR1Lease(ctx, r.Writer.LeaseID)
			if lease.State != "Closed" || len(lease.Pending) != 1 || len(lease.Roots) != 1 {
				t.Fatal("unknown outcome lost protection", lease)
			}
			seam.fail = false
			if err := registry.Quiescent(); err != nil {
				t.Fatal("reconciled registry cannot shut down", err)
			}
			lease, _ = f.peer.GetR1Lease(ctx, r.Writer.LeaseID)
			if lease.State != "Released" || len(lease.Pending) != 0 {
				t.Fatal("joined writer leaked shared protection", lease)
			}
			if committed {
				if out, err := writer.ReconcileCommit(ctx, f.m, r.Head.OpID); err != nil || out.Head.Revision != 1 {
					t.Fatal("original outcome lost after safe release", out, err)
				}
			}
		})
	}
}

// The closure and transfer share one native transaction, including a lost ACK.
type takeoverResponseFault struct {
	API
	before, after bool
}

func (a *takeoverResponseFault) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	if a.before {
		a.before = false
		return nil, errors.New("takeover send failed")
	}
	out, err := a.API.TransactWriteItems(ctx, in, opts...)
	if err == nil && a.after {
		a.after = false
		return nil, errors.New("takeover ACK lost")
	}
	return out, err
}
