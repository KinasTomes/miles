package dynamodb

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var _ s.RecoveryStore = (*Store)(nil)

func recoveryEpochKey(h string, e uint64) Key {
	return stringKey("RecoveryEpochs", "history_epoch_key", fmt.Sprintf("%s/%d", h, e))
}
func recoveryStateKey(h string) Key {
	return stringKey("SystemConfig", "config_id", "recovery/state/"+h)
}
func (m *Store) GetRecoveryState(ctx context.Context, h string) (s.RecoveryState, error) {
	if !mutationID.MatchString(h) {
		return s.RecoveryState{}, s.ErrInvalid
	}
	v, err := r1Read[s.RecoveryState](m.transaction(ctx), recoveryStateKey(h))
	if err == nil && v.HistoryID != h {
		err = s.ErrUnavailable
	}
	return v, err
}

// Due is a scheduling observation, bound to its epoch. It does not grant
// publication authority; BeginRecoveryCut still checks the current gate/fence.
func (m *Store) RecoveryCutDue(ctx context.Context, history string, worker s.WriterFence) (uint64, bool, error) {
	if err := m.CheckRecoveryWorker(ctx, worker); err != nil {
		return 0, false, err
	}
	state, err := m.GetRecoveryState(ctx, history)
	if err != nil {
		return 0, false, err
	}
	if state.Incarnation != worker.ServiceIncarnation {
		return 0, false, s.ErrFenced
	}
	bounds, err := m.r1Bounds(ctx)
	if err != nil {
		return 0, false, err
	}
	return state.NextCutEpoch, !bounds.Lower.Before(state.NextCutAt), nil
}
func (m *Store) GetRecoveryEpoch(ctx context.Context, h string, e uint64) (s.RecoveryEpoch, error) {
	if !mutationID.MatchString(h) || e >= s.MaxPolicyRevision {
		return s.RecoveryEpoch{}, s.ErrInvalid
	}
	v, err := r1Read[s.RecoveryEpoch](m.transaction(ctx), recoveryEpochKey(h, e))
	if err == nil && (v.HistoryID != h || v.Epoch != e) {
		err = s.ErrUnavailable
	}
	return v, err
}

// Provisioning only, after the caller has verified the immutable genesis and
// catalog. Refuse legacy/partially enabled producers; never synthesize a base.
func (m *Store) InitializeRecovery(ctx context.Context, state s.RecoveryState) error {
	if state.NextCutEpoch != 0 || !state.NextCutAt.IsZero() {
		return s.ErrInvalid
	}
	bounds, err := m.r1Bounds(ctx)
	if err != nil {
		return err
	}
	state.NextCutAt = bounds.Upper.UTC().Add(s.RecoveryCutInterval)
	if state.Validate() != nil || state.NextToSeal != 0 || state.Revision != 1 || !m.requireProducers {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err != nil {
			return err
		}
		if g.HistoryID != state.HistoryID || g.Incarnation != state.Incarnation || g.RecoveryVersion != 1 || g.Epoch != 0 || g.State != "Open" {
			return s.ErrFenced
		}
		old, err := r1Read[s.RecoveryState](t, recoveryStateKey(state.HistoryID))
		if err == nil {
			if old.GenesisSHA256 == state.GenesisSHA256 && old.GenesisLength == state.GenesisLength {
				return nil
			}
			return s.ErrConflict
		}
		if !errors.Is(err, s.ErrNotFound) {
			return err
		}
		g.RecoveryEnabled = true
		g.Revision++
		if err := r1Put(t, r1GateKey(), g); err != nil {
			return err
		}
		return r1Put(t, recoveryStateKey(state.HistoryID), state)
	})
}
func (m *Store) recoveryOwned(t *transaction, r s.RecoveryCutRequest) (s.R1Gate, s.RecoveryEpoch, error) {
	var zero s.RecoveryEpoch
	if r.Validate() != nil {
		return s.R1Gate{}, zero, s.ErrInvalid
	}
	if err := m.recoveryFence(t, r.Worker, true); err != nil {
		return s.R1Gate{}, zero, err
	}
	g, err := r1Read[s.R1Gate](t, r1GateKey())
	if err != nil {
		return g, zero, err
	}
	e, err := r1Read[s.RecoveryEpoch](t, recoveryEpochKey(r.HistoryID, r.Epoch))
	if err != nil {
		return g, e, err
	}
	if g.HistoryID != r.HistoryID || g.Incarnation != r.Worker.ServiceIncarnation || e.HistoryID != r.HistoryID || e.Epoch != r.Epoch || e.Incarnation != g.Incarnation || e.CloseID != r.CloseID || e.Worker != r.Worker {
		return g, e, s.ErrFenced
	}
	return g, e, nil
}
func (m *Store) BeginRecoveryCut(ctx context.Context, r s.RecoveryCutRequest) (s.RecoveryEpoch, error) {
	var out s.RecoveryEpoch
	if r.Validate() != nil || !m.requireProducers {
		return out, s.ErrInvalid
	}
	if err := m.reclaimRecoveryWorker(ctx, r); err != nil {
		return out, err
	}
	err := m.update(ctx, func(t *transaction) error {
		if err := m.recoveryFence(t, r.Worker, true); err != nil {
			return err
		}
		g, err := r1Read[s.R1Gate](t, r1GateKey())
		if err != nil {
			return err
		}
		if g.HistoryID != r.HistoryID || g.Incarnation != r.Worker.ServiceIncarnation || g.RecoveryVersion != 1 {
			return s.ErrFenced
		}
		if _, err = r1Read[s.RecoveryState](t, recoveryStateKey(r.HistoryID)); err != nil {
			return err
		}
		e, err := r1Read[s.RecoveryEpoch](t, recoveryEpochKey(r.HistoryID, r.Epoch))
		if err == nil && (e.HistoryID != r.HistoryID || e.Epoch != r.Epoch) {
			return s.ErrUnavailable
		}
		if err == nil && e.State != "Open" {
			if e.CloseID != r.CloseID || e.Incarnation != g.Incarnation {
				return s.ErrConflict
			}
			if e.Worker != r.Worker {
				old, err := r1Read[s.R1Lease](t, r1LeaseKey(e.Worker.LeaseID))
				if err != nil {
					return err
				}
				if old.Fence == e.Worker && (old.State != "Released" && old.State != "Closed" || len(old.Pending) != 0) {
					return s.ErrR1Busy
				}
				// A reused slot can only be acquired after old Released, under R1 CAS.
				if old.Fence != e.Worker && (old.Fence.LeaseID != e.Worker.LeaseID || old.Fence.Epoch <= e.Worker.Epoch) {
					return s.ErrFenced
				}
				e.Worker = r.Worker
				e.Revision++
				if err = r1Put(t, recoveryEpochKey(r.HistoryID, r.Epoch), e); err != nil {
					return err
				}
			}
			out = e
			return nil
		}
		if err != nil && !errors.Is(err, s.ErrNotFound) {
			return err
		}
		if g.Epoch != r.Epoch || g.State != "Open" {
			return s.ErrR1Busy
		}
		b, err := m.r1Bounds(ctx)
		if err != nil {
			return err
		}
		g.State = "RecoveryCut"
		g.RecoveryCutID = r.CloseID
		g.Revision++
		e = s.RecoveryEpoch{Version: 1, HistoryID: r.HistoryID, Incarnation: g.Incarnation, Epoch: r.Epoch, Revision: e.Revision + 1, State: "Closing", CloseID: r.CloseID, Worker: r.Worker, StartedAt: b.Upper, Phase: "producers"}
		if err = r1Put(t, r1GateKey(), g); err != nil {
			return err
		}
		out = e
		return r1Put(t, recoveryEpochKey(r.HistoryID, r.Epoch), e)
	})
	return out, err
}

// A page cursor is recorded in the same transaction as all terminal receipt
// checks. Frozen partition membership, not Active==0, is the closure proof.
func (m *Store) StepRecoveryCut(ctx context.Context, r s.RecoveryCutRequest) (s.RecoveryEpoch, error) {
	var out s.RecoveryEpoch
	if r.Validate() != nil {
		return out, s.ErrInvalid
	}
	before, err := m.GetRecoveryEpoch(ctx, r.HistoryID, r.Epoch)
	if err != nil {
		return out, err
	}
	if before.State != "Closing" {
		err = m.view(ctx, func(t *transaction) error { _, e, err := m.recoveryOwned(t, r); out = e; return err })
		return out, err
	}
	if before.Phase != "producers" && before.Phase != "mutations" {
		return out, s.ErrUnavailable
	}
	resource := recoveryPartition(before.Phase, r.HistoryID, r.Epoch)
	in := &ddb.QueryInput{TableName: aws.String(m.tables["Leases"]), ConsistentRead: aws.Bool(true), Limit: aws.Int32(24), KeyConditionExpression: aws.String("#pk = :pk"), ExpressionAttributeNames: map[string]string{"#pk": "resource_id"}, ExpressionAttributeValues: Item{":pk": text(resource)}}
	if before.Cursor != "" {
		in.ExclusiveStartKey = Item{"resource_id": text(resource), "lease_id": text(before.Cursor)}
	}
	page, err := m.api.Query(ctx, in)
	if err != nil {
		return out, backend(err)
	}
	if page == nil {
		return out, s.ErrUnavailable
	}
	next := ""
	if len(page.LastEvaluatedKey) > 0 {
		v, ok := page.LastEvaluatedKey["lease_id"].(*types.AttributeValueMemberS)
		if !ok || v.Value <= before.Cursor {
			return out, s.ErrUnavailable
		}
		next = v.Value
	}
	err = m.update(ctx, func(t *transaction) error {
		g, e, err := m.recoveryOwned(t, r)
		if err != nil {
			return err
		}
		if e.State != "Closing" || e.Revision != before.Revision || g.State != "RecoveryCut" || g.RecoveryCutID != r.CloseID || g.Epoch != r.Epoch {
			return s.ErrConflict
		}
		previous := e.Cursor
		bounds, err := m.r1Bounds(ctx)
		if err != nil {
			return err
		}
		if !bounds.Upper.Before(e.StartedAt.Add(s.RecoveryCloseBudget)) {
			return s.ErrRecoveryCloseTimeout
		}
		for _, item := range page.Items {
			pk, ok := item["resource_id"].(*types.AttributeValueMemberS)
			if !ok || pk.Value != resource {
				return s.ErrUnavailable
			}
			sk, ok := item["lease_id"].(*types.AttributeValueMemberS)
			if !ok || sk.Value <= previous {
				return s.ErrUnavailable
			}
			previous = sk.Value
			k := recoveryMemberKey(e.Phase, r.HistoryID, r.Epoch, sk.Value)
			if e.Phase == "producers" {
				p, err := r1Read[recoveryProducer](t, k)
				if err != nil {
					return err
				}
				if !p.Terminal || p.ClosureRevision == 0 {
					return s.ErrR1Busy
				}
				if p.HistoryID != r.HistoryID || p.Epoch != r.Epoch || p.Version != 1 || (p.Kind != "writer" && p.Kind != "control") {
					return s.ErrUnavailable
				}
				if p.Fence.ServiceIncarnation != g.Incarnation || !reflect.DeepEqual(recoveryProducerKey(s.R1Lease{HistoryID: p.HistoryID, RecoveryEpoch: p.Epoch, Fence: p.Fence}), k) {
					return s.ErrUnavailable
				}
				e.ProofSHA256, err = s.RecoveryDigest(e.ProofSHA256, p)
				if err != nil {
					return err
				}
				e.ProducerCount++
			} else {
				event, err := r1Read[s.OutboxEvent](t, k)
				if err != nil {
					return err
				}
				if event.Validate() != nil || event.HistoryID != r.HistoryID || event.Epoch != r.Epoch || event.ID != sk.Value {
					return s.ErrUnavailable
				}
				actual, err := get[s.OutboxEvent](t, key("events", event.ID))
				if err != nil {
					return err
				}
				if !reflect.DeepEqual(actual, event) {
					return s.ErrConflict
				}
				e.EventsSHA256, err = s.RecoveryDigest(e.EventsSHA256, event)
				if err != nil {
					return err
				}
				e.EventCount++
			}
		}
		if next != "" && (len(page.Items) == 0 || next != previous) {
			return s.ErrUnavailable
		}
		e.Cursor = next
		e.Revision++
		if next == "" {
			if e.Phase == "producers" {
				e.Phase = "mutations"
			} else {
				e.Phase = "complete"
				e.State = "Closed"
				b, err := m.r1Bounds(ctx)
				if err != nil {
					return err
				}
				e.CutAt = b.Upper
				state, err := r1Read[s.RecoveryState](t, recoveryStateKey(r.HistoryID))
				if err != nil {
					return err
				}
				if state.HistoryID != r.HistoryID || state.Incarnation != g.Incarnation || state.NextCutEpoch != r.Epoch {
					return s.ErrConflict
				}
				state.NextCutEpoch = r.Epoch + 1
				state.NextCutAt = e.CutAt.UTC().Add(s.RecoveryCutInterval)
				state.Revision++
				if err = r1Put(t, recoveryStateKey(r.HistoryID), state); err != nil {
					return err
				}
				g.Epoch++
				g.State = "Open"
				g.RecoveryCutID = ""
				g.Revision++
				if err = r1Put(t, r1GateKey(), g); err != nil {
					return err
				}
			}
		}
		out = e
		return r1Put(t, recoveryEpochKey(r.HistoryID, r.Epoch), e)
	})
	return out, err
}
func (m *Store) AbortRecoveryCut(ctx context.Context, r s.RecoveryCutRequest) error {
	return m.update(ctx, func(t *transaction) error {
		g, e, err := m.recoveryOwned(t, r)
		if err != nil {
			return err
		}
		if e.State == "Open" && g.State == "Open" && g.Epoch == r.Epoch {
			return nil
		}
		if e.State != "Closing" || g.State != "RecoveryCut" || g.Epoch != r.Epoch || g.RecoveryCutID != r.CloseID {
			return s.ErrConflict
		}
		e.State = "Open"
		e.Revision++
		e.Phase = ""
		e.Cursor = ""
		g.State = "Open"
		g.RecoveryCutID = ""
		g.Revision++
		if err = r1Put(t, recoveryEpochKey(r.HistoryID, r.Epoch), e); err != nil {
			return err
		}
		return r1Put(t, r1GateKey(), g)
	})
}
func (m *Store) PublishRecoverySeal(ctx context.Context, r s.RecoveryCutRequest, revision uint64, key, sum string) error {
	raw, decodeErr := hex.DecodeString(sum)
	if r.Validate() != nil || decodeErr != nil || len(raw) != 32 || hex.EncodeToString(raw) != sum || key != fmt.Sprintf("journal/epochs/%s/%d/seal-%s.json", r.HistoryID, r.Epoch, sum) {
		return s.ErrInvalid
	}
	return m.update(ctx, func(t *transaction) error {
		_, e, err := m.recoveryOwned(t, r)
		if err != nil {
			return err
		}
		if e.State == "Sealed" {
			if e.SealKey == key && e.SealSHA256 == sum {
				return nil
			}
			return s.ErrConflict
		}
		if e.State != "Closed" || e.Phase != "complete" || e.Revision != revision {
			return s.ErrConflict
		}
		state, err := r1Read[s.RecoveryState](t, recoveryStateKey(r.HistoryID))
		if err != nil {
			return err
		}
		if state.HistoryID != r.HistoryID || state.Incarnation != e.Incarnation {
			return s.ErrFenced
		}
		if state.NextToSeal != r.Epoch {
			return s.ErrConflict
		}
		state.NextToSeal++
		state.Revision++
		state.SealKey = key
		state.SealSHA256 = sum
		e.State = "Sealed"
		e.Revision++
		e.SealKey = key
		e.SealSHA256 = sum
		if err = r1Put(t, recoveryStateKey(r.HistoryID), state); err != nil {
			return err
		}
		return r1Put(t, recoveryEpochKey(r.HistoryID, r.Epoch), e)
	})
}
