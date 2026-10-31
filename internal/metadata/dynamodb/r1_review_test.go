package dynamodb

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/coordination"
	s "github.com/KinasTomes/miles/internal/storage"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type r1SlowReadAPI struct {
	API
	lease        Key
	reads, sends int
	afterRead    func()
}

func (a *r1SlowReadAPI) GetItem(ctx context.Context, in *ddb.GetItemInput, opts ...func(*ddb.Options)) (*ddb.GetItemOutput, error) {
	out, err := a.API.GetItem(ctx, in, opts...)
	if reflect.DeepEqual(in.Key, a.lease.Attributes) {
		a.reads++
		if a.reads == 2 {
			a.afterRead()
		}
	}
	return out, err
}
func (a *r1SlowReadAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	a.sends++
	return a.API.TransactWriteItems(ctx, in, opts...)
}

func TestLocalR1FinalDispatchRejectsExpiryDuringAuthorityRead(t *testing.T) {
	ctx := context.Background()
	m, _, f, utc := sharedFixture(t)
	r := f.Request("slow-final-read", 0)
	admit := r1Request(f, "slow-final-read", 1)
	admit.Object = r.Head.Object
	r.Writer = admit.Fence
	r.Head.Deleted = true
	if _, err := m.AcquireR1(ctx, admit); err != nil {
		t.Fatal(err)
	}
	if err := m.update(ctx, func(tx *transaction) error {
		return put(tx, key("reservations", r.ReservationID), s.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, Object: r.Head.Object, OwnerID: f.Bucket.OwnerID, Revision: 1, State: s.ReservationActive, Writer: r.Writer, Deadline: f.Now.Add(time.Minute)})
	}); err != nil {
		t.Fatal(err)
	}
	api := &r1SlowReadAPI{API: m.api, lease: r1LeaseKey(r.Writer.LeaseID), afterRead: func() { utc.Advance(31 * time.Second) }}
	m.api = api
	// The second native lease read belongs to the final send guard, after
	// dispatchDeadline. No transaction has been dispatched at the delay point.
	table := m.tables["SystemConfig"]
	input := &ddb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{{Put: &types.Put{TableName: &table, Item: Item{"config_id": text("r1-late-dispatch-probe")}}}}}
	err := m.publishAttempt(ctx, r, input, Key{}, nil)
	if !errors.Is(err, s.ErrFenced) || api.reads != 2 || api.sends != 0 {
		t.Fatal("expired final dispatch", err, api.reads, api.sends)
	}
}

type r1ActionSeam struct {
	s.R1Store
	afterBegin func()
	settleErr  error
}

func (a *r1ActionSeam) BeginR1Action(ctx context.Context, f s.WriterFence, id string) error {
	if err := a.R1Store.BeginR1Action(ctx, f, id); err != nil {
		return err
	}
	if a.afterBegin != nil {
		a.afterBegin()
	}
	return nil
}
func (a *r1ActionSeam) SettleR1Action(ctx context.Context, f s.WriterFence, id string) error {
	if a.settleErr != nil {
		return a.settleErr
	}
	return a.R1Store.SettleR1Action(ctx, f, id)
}

type r1ClosedProof struct{ out s.MultipartRecoveryResult }

func (p r1ClosedProof) ResolveCompletion(context.Context, s.MultipartRequest) (s.MultipartRecoveryResult, error) {
	return p.out, nil
}
func (p r1ClosedProof) ResolvePartPublisher(context.Context, s.MultipartRequest, string) (s.MultipartRecoveryResult, error) {
	return p.out, nil
}

func TestLocalR1ActionACKAndMultipartReconciliation(t *testing.T) {
	for _, mode := range []string{"expired-ack", "cancelled-ack", "expired-ack-unknown-settlement", "multipart-proof"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m, peer, f, utc := sharedFixture(t)
			local, _ := clock.NewVirtual(f.Now, 100)
			seam := &r1ActionSeam{R1Store: m}
			limits := coordination.LeaseLimits{Lifetime: 30 * time.Second, Heartbeat: 5 * time.Second}
			g, err := coordination.NewSharedR1Gate(local, "shared-service", "review-worker", limits, seam, utc)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := coordination.NewWriterRegistry(g, limits, coordination.Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			r := r1Request(f, "review-worker", 1)
			op := f.Request("review-worker", 0).Head.OpID
			deadline, _ := local.Now().Instant.Add(time.Minute)
			w, err := registry.Register(ctx, r.Fence, deadline)
			if err != nil {
				t.Fatal(err)
			}
			work, err := w.BeginOperation(ctx, r.Object, op)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "multipart-proof" {
				seam.afterBegin = func() {
					if mode == "cancelled-ack" {
						cancel()
						return
					}
					utc.Advance(31 * time.Second)
					if err := local.Advance(31 * time.Second); err != nil {
						t.Fatal(err)
					}
				}
			}
			if mode == "expired-ack-unknown-settlement" {
				seam.settleErr = s.ErrUnavailable
			}
			called := false
			err = w.Do(ctx, "commit/"+op, func(s.WriterFence) error { called = true; return s.ErrUnavailable })
			if mode != "multipart-proof" && (called || err == nil) {
				t.Fatal("new physical dispatch after permission lost", called, err)
			}
			if mode == "multipart-proof" && !errors.Is(err, s.ErrUnavailable) {
				t.Fatal(err)
			}
			w.CloseFence()
			work.Finish()
			if mode == "multipart-proof" {
				proof := r1ClosedProof{out: s.MultipartRecoveryResult{Disposition: s.RecoveryClosed, Upload: s.MultipartUpload{ID: "review-upload", Object: r.Object, CompletionOp: op, ClosedFence: 3, ClosedWriter: r.Fence, State: "Aborting"}}}
				request := s.MultipartRequest{UploadID: proof.out.Upload.ID, OpID: op}
				seam.settleErr = s.ErrUnavailable
				if _, err = w.ReconcileMultipart(ctx, proof, request, 3); !errors.Is(err, s.ErrUnavailable) {
					t.Fatal("settlement error hidden", err)
				}
				if err = w.Release(); !errors.Is(err, coordination.ErrReconcilePending) {
					t.Fatal("failed settlement released local protection", err)
				}
				seam.settleErr = nil
				if _, err = w.ReconcileMultipart(ctx, proof, request, 3); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "expired-ack-unknown-settlement" {
				if err = w.Release(); !errors.Is(err, coordination.ErrReconcilePending) {
					t.Fatal("unknown settlement released", err)
				}
				lease, e := peer.GetR1Lease(context.Background(), r.Fence.LeaseID)
				if e != nil || len(lease.Pending) != 1 {
					t.Fatal("lost durable protection", lease, e)
				}
				return
			}
			if err = w.Release(); err != nil {
				t.Fatal(err)
			}
			lease, err := peer.GetR1Lease(context.Background(), r.Fence.LeaseID)
			gate, ge := peer.GetR1Gate(context.Background())
			if err != nil || ge != nil || lease.State != "Released" || len(lease.Pending) != 0 || gate.Active != 0 {
				t.Fatal("release did not drain shared state", lease, gate, err, ge)
			}
		})
	}
}
