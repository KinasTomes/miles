package dynamodb

import (
	"context"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

// RuntimeObservation is a bounded live projection, not authority or a snapshot.
type RuntimeObservation struct {
	CutAt, GateStartedAt, OldestPendingExport time.Time
	GateClosed                                bool
	StuckCompleting                           int
}

func (m *Store) ObserveRuntime(ctx context.Context, history string, now time.Time) (out RuntimeObservation, err error) {
	gate, err := m.GetR1Gate(ctx)
	if err != nil {
		return out, err
	}
	if gate.HistoryID != history {
		return out, storage.ErrFenced
	}
	state, err := m.GetRecoveryState(ctx, history)
	if err != nil {
		return out, err
	}
	if state.HistoryID != history || state.Incarnation != gate.Incarnation {
		return out, storage.ErrFenced
	}
	// A closed but unsealed first epoch advances the schedule, not recovery
	// progress. Withhold telemetry rather than mistake that reset for a seal.
	if state.NextToSeal == 0 && state.NextCutEpoch != 0 {
		return out, storage.ErrUnavailable
	}
	// Before the first close, the durable initial schedule supplies history age.
	cutAt := state.NextCutAt.Add(-storage.RecoveryCutInterval)
	if state.NextToSeal > 0 {
		epoch, e := m.GetRecoveryEpoch(ctx, history, state.NextToSeal-1)
		if e != nil {
			return out, e
		}
		if epoch.State != "Sealed" || epoch.CutAt.IsZero() {
			return out, storage.ErrUnavailable
		}
		cutAt = epoch.CutAt
	}
	out.GateClosed = gate.State != "Open"
	if gate.State == "RecoveryCut" {
		epoch, e := m.GetRecoveryEpoch(ctx, history, gate.Epoch)
		if e != nil {
			return out, e
		}
		if epoch.CloseID != gate.RecoveryCutID || epoch.StartedAt.IsZero() {
			return out, storage.ErrUnavailable
		}
		out.GateStartedAt = epoch.StartedAt
	}
	stuck := 0
	q := storage.ObservationRequest{Kind: "uploads", Limit: 128}
	for page := 0; ; page++ {
		if page == 32 {
			return out, storage.ErrUnavailable
		}
		result, e := m.ObservePage(ctx, q)
		if e != nil {
			return out, e
		}
		for _, upload := range result.Uploads {
			if upload.State == "Completing" && !upload.LeaseUntil.After(now) {
				stuck++
			}
		}
		if result.Next == "" {
			break
		}
		if result.Next == q.Cursor {
			return out, storage.ErrUnavailable
		}
		q.Cursor = result.Next
	}
	oldest, e := m.ObserveExportLag(ctx, history)
	if e != nil {
		return out, e
	}
	out.CutAt = cutAt
	out.OldestPendingExport = oldest
	out.StuckCompleting = stuck
	return out, nil
}
