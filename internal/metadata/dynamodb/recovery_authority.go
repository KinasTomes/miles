package dynamodb

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

type recoveryProducer struct {
	Version         uint32
	HistoryID       string
	Epoch           uint64
	Fence           s.WriterFence
	Kind, OpID      string
	Terminal        bool
	ClosureRevision uint64
}

func (p recoveryProducer) Validate() error {
	if p.Version != 1 || !mutationID.MatchString(p.HistoryID) || p.Epoch > s.MaxPolicyRevision || p.Fence.LeaseID == "" || p.Fence.Epoch == 0 || p.Fence.Epoch > s.MaxPolicyRevision || p.Fence.ServiceIncarnation == "" || (p.Kind != "writer" && p.Kind != "control") || p.Terminal != (p.ClosureRevision > 0) || p.ClosureRevision > s.MaxPolicyRevision {
		return s.ErrInvalid
	}
	return nil
}

func recoveryPartition(kind, history string, epoch uint64) string {
	return fmt.Sprintf("recovery/%s/%s/%d", kind, history, epoch)
}
func recoveryMemberKey(kind, history string, epoch uint64, id string) Key {
	k := stringKey("Leases", "resource_id", recoveryPartition(kind, history, epoch))
	k.Attributes["lease_id"] = text(id)
	return k
}
func recoveryProducerKey(l s.R1Lease) Key {
	return recoveryMemberKey("producers", l.HistoryID, l.RecoveryEpoch, hex.EncodeToString([]byte(l.Fence.LeaseID))+fmt.Sprintf("/%016x", l.Fence.Epoch))
}
func (m *Store) recordRecoveryAdmission(t *transaction, g s.R1Gate, l s.R1Lease) error {
	if g.RecoveryVersion != 1 || l.Kind == "reader" || l.Kind == "recovery" {
		return nil
	}
	// One retained entry per admitted generation; slot reuse cannot erase it.
	p := recoveryProducer{Version: 1, HistoryID: g.HistoryID, Epoch: g.Epoch, Fence: l.Fence, Kind: l.Kind, OpID: l.OpID}
	return r1Put(t, recoveryProducerKey(l), p)
}
func (m *Store) recordRecoveryClosure(t *transaction, l s.R1Lease) error {
	if l.HistoryID == "" || l.Kind == "reader" || l.Kind == "recovery" || l.State == "Active" || len(l.Pending) != 0 {
		return nil
	}
	k := recoveryProducerKey(l)
	p, err := r1Read[recoveryProducer](t, k)
	if err != nil {
		return err
	}
	if p.Fence != l.Fence || p.HistoryID != l.HistoryID || p.Epoch != l.RecoveryEpoch {
		return s.ErrFenced
	}
	if p.Terminal {
		return nil
	}
	p.Terminal = true
	p.ClosureRevision = l.Revision
	return r1Put(t, k, p)
}

func (m *Store) recoveryFence(t *transaction, f s.WriterFence, register bool) error {
	a, err := m.r1Authority()
	if err != nil {
		return err
	}
	if err = a.checkLease(t.ctx, f, r1Observe(t)); err != nil {
		return err
	}
	l, err := r1Read[s.R1Lease](t, r1LeaseKey(f.LeaseID))
	if err != nil {
		return err
	}
	if l.Kind != "recovery" {
		return s.ErrFenced
	}
	if register {
		if t.recoveryFences == nil {
			t.recoveryFences = map[s.WriterFence]bool{}
		}
		t.recoveryFences[f] = true
	}
	return nil
}
func (m *Store) CheckRecoveryWorker(ctx context.Context, f s.WriterFence) error {
	if _, shared := m.authority.(*SharedAuthority); !shared && f == (s.WriterFence{}) {
		return nil
	}
	return m.view(ctx, func(t *transaction) error { return m.recoveryFence(t, f, false) })
}

// Recovery leases can publish only immutable journal objects and fenced
// progress. After expiry+grace, closure reconciles all metadata publication;
// late immutable writes are verified by the successor, never overwritten.
func (m *Store) reclaimRecoveryWorker(ctx context.Context, r s.RecoveryCutRequest) error {
	if err := m.CheckRecoveryWorker(ctx, r.Worker); err != nil {
		return err
	}
	e, err := m.GetRecoveryEpoch(ctx, r.HistoryID, r.Epoch)
	if errors.Is(err, s.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if e.CloseID != r.CloseID || e.Worker == r.Worker || e.State == "Open" {
		return nil
	}
	old, err := m.GetR1Lease(ctx, e.Worker.LeaseID)
	if err != nil {
		return err
	}
	if old.Fence != e.Worker {
		return nil
	} // slot reuse already consumed Released
	if old.Kind != "recovery" {
		return s.ErrFenced
	}
	if old.State == "Active" {
		if err = m.ReclaimR1(ctx, old.Fence, 5*time.Second); err != nil {
			return err
		}
	}
	if len(old.Pending) != 0 || old.PinCount != 0 || len(old.Roots) != 0 {
		return s.ErrR1Busy
	}
	return m.ReleaseR1(ctx, old.Fence)
}

// This is the producer's mutation-membership attachment, not an asynchronous
// index. It commits with its outcome/outbox and is retained through sealing.
func (t multipartTx) RecordRecoveryEvent(c s.MutationContext, e s.OutboxEvent) error {
	if _, ok := t.store.authority.(*SharedAuthority); !ok {
		return nil
	}
	l, err := r1Read[s.R1Lease](t.transaction, r1LeaseKey(c.Writer.LeaseID))
	if err != nil {
		return err
	}
	g, err := r1Read[s.R1Gate](t.transaction, r1GateKey())
	if err != nil {
		return err
	}
	if g.RecoveryVersion != 1 {
		return s.ErrFenced
	}
	if l.Fence != c.Writer || l.HistoryID != e.HistoryID || l.RecoveryEpoch != e.Epoch {
		return s.ErrFenced
	}
	p, err := r1Read[recoveryProducer](t.transaction, recoveryProducerKey(l))
	if err != nil {
		return err
	}
	if p.Terminal || p.Fence != c.Writer {
		return s.ErrFenced
	}
	k := recoveryMemberKey("mutations", e.HistoryID, e.Epoch, e.ID)
	old, err := r1Read[s.OutboxEvent](t.transaction, k)
	if err == nil {
		if reflect.DeepEqual(old, e) {
			return nil
		}
		return s.ErrConflict
	}
	if !errors.Is(err, s.ErrNotFound) {
		return err
	}
	return r1Put(t.transaction, k, e)
}
