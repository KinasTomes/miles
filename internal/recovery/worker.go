package recovery

import (
	"context"
	"errors"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	s "github.com/KinasTomes/miles/internal/storage"
)

const ExportInterval = 60 * time.Second
const CutInterval = time.Hour
const workerHeartbeat = 15 * time.Second

type Worker struct {
	Store    s.RecoveryStore
	Payloads PayloadSource
	Objects  ImmutableStore
	Catalog  Catalog
	Clock    clock.Clock
	Fence    s.WriterFence
	// Observe receives only a bounded operation/result code, never entity IDs.
	Observe func(string)
}

func (w Worker) sealer() Sealer {
	return Sealer{Exporter: Exporter{Metadata: w.Store, Payloads: w.Payloads, Objects: w.Objects, Worker: w.Fence}, Store: w.Store, Catalog: w.Catalog}
}
func (w Worker) validate() error {
	if w.Store == nil || w.Payloads == nil || w.Objects == nil || w.Clock == nil || w.Catalog.Validate() != nil || w.Fence.ServiceIncarnation != w.Catalog.Incarnation || w.Fence.LeaseID == "" || w.Fence.Epoch == 0 {
		return s.ErrInvalid
	}
	return nil
}
func (w Worker) observe(code string) {
	if w.Observe != nil {
		w.Observe(code)
	}
}

// Flush revisits discovery from the beginning, including all empty base pages.
// Closed epochs publish in order; an earlier incomplete epoch blocks the chain.
func (w Worker) Flush(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	g, err := w.Store.GetR1Gate(ctx)
	if err != nil {
		return err
	}
	state, err := w.Store.GetRecoveryState(ctx, w.Catalog.History)
	if err != nil {
		return err
	}
	if g.HistoryID != w.Catalog.History || g.Incarnation != w.Catalog.Incarnation {
		return s.ErrFenced
	}
	for epoch := state.NextToSeal; epoch <= g.Epoch; epoch++ {
		q := s.OutboxQuery{HistoryID: g.HistoryID, Epoch: epoch, Limit: 128}
		for {
			next, err := w.sealer().Exporter.RunPage(ctx, q)
			if err != nil {
				return err
			}
			if next == "" {
				break
			}
			if next <= q.After || !isHex(next, 64) {
				return s.ErrInvalid
			}
			q.After = next
		}
		if epoch < g.Epoch {
			e, err := w.Store.GetRecoveryEpoch(ctx, g.HistoryID, epoch)
			if err != nil {
				return err
			}
			r := s.RecoveryCutRequest{HistoryID: g.HistoryID, Epoch: epoch, CloseID: e.CloseID, Worker: w.Fence}
			if _, err = w.Store.BeginRecoveryCut(ctx, r); err != nil {
				return err
			}
			if _, err = w.sealer().Seal(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// CloseAndSeal does not kill/reclaim healthy producers. The shared adapter
// persists the frozen generation and derives closure proof one page at a time.
func (w Worker) CloseAndSeal(ctx context.Context, r s.RecoveryCutRequest) (AttachmentReference, error) {
	var zero AttachmentReference
	if err := w.validate(); err != nil {
		return zero, err
	}
	if r.Worker != w.Fence || r.HistoryID != w.Catalog.History {
		return zero, s.ErrInvalid
	}
	end, err := w.Clock.Now().Instant.Add(s.RecoveryCloseBudget)
	if err != nil {
		return zero, err
	}
	e, err := w.Store.BeginRecoveryCut(ctx, r)
	if err != nil {
		return zero, err
	}
	abort := func(cause error) (AttachmentReference, error) {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		// CAS may find Closed after a lost ACK; it can never reopen that epoch.
		a := w.Store.AbortRecoveryCut(cleanup, r)
		if errors.Is(a, s.ErrConflict) {
			a = nil
		}
		if errors.Is(cause, s.ErrRecoveryCloseTimeout) {
			w.observe("close_timeout")
		}
		return zero, errors.Join(cause, a)
	}
	for e.State == "Closing" {
		if err = ctx.Err(); err != nil {
			return abort(err)
		}
		remaining, err := end.Sub(w.Clock.Now().Instant)
		if err != nil {
			return abort(err)
		}
		if remaining <= 0 {
			return abort(s.ErrRecoveryCloseTimeout)
		}
		next, stepErr := w.Store.StepRecoveryCut(ctx, r)
		err = stepErr
		if errors.Is(err, s.ErrRecoveryCloseTimeout) {
			return abort(err)
		}
		if errors.Is(err, s.ErrR1Busy) {
			if err = w.Clock.Sleep(ctx, min(time.Second, remaining)); err != nil {
				return abort(err)
			}
			continue
		}
		if err != nil {
			return zero, err
		} // unknown stays gated until authoritative resolution
		e = next
	}
	result, err := w.sealer().Seal(ctx, r)
	if err == nil {
		w.observe("sealed")
	}
	return result, err
}

// Run is a supervised worker for an already acquired recovery-kind R1 lease.
// The runtime/job owner supplies that lease and joins Run before releasing it.
// Dispatch never derives authority from this cadence or from job timestamps.
func (w Worker) Run(ctx context.Context) error {
	if err := w.validate(); err != nil {
		return err
	}
	renewer, ok := w.Store.(interface {
		RenewR1(context.Context, s.WriterFence) (s.R1Lease, error)
	})
	if !ok {
		return s.ErrInvalid
	}
	if err := w.Store.CheckRecoveryWorker(ctx, w.Fence); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	heartbeats := make(chan error, 1)
	go func() {
		for {
			if err := w.Clock.Sleep(ctx, workerHeartbeat); err != nil {
				cancel(err)
				heartbeats <- err
				return
			}
			if _, err := renewer.RenewR1(ctx, w.Fence); err != nil {
				cancel(err)
				heartbeats <- err
				return
			}
		}
	}()
	defer func() { cancel(nil); <-heartbeats }()
	nextCut, err := w.Clock.Now().Instant.Add(CutInterval)
	if err != nil {
		return err
	}
	for {
		if err = w.Flush(ctx); err != nil {
			if ctx.Err() != nil {
				return context.Cause(ctx)
			}
			w.observe("export_error")
		} else {
			w.observe("export_ok")
		}
		remaining, err := nextCut.Sub(w.Clock.Now().Instant)
		if err != nil {
			return err
		}
		gate, err := w.Store.GetR1Gate(ctx)
		if err != nil {
			return err
		}
		if remaining <= 0 || gate.State == "RecoveryCut" {
			id := gate.RecoveryCutID
			if id == "" {
				id, err = s.NewBucketID()
				if err != nil {
					return err
				}
			}
			_, err = w.CloseAndSeal(ctx, s.RecoveryCutRequest{HistoryID: w.Catalog.History, Epoch: gate.Epoch, CloseID: id, Worker: w.Fence})
			if err != nil {
				if ctx.Err() != nil {
					return context.Cause(ctx)
				}
				w.observe("close_error")
			}
			nextCut, err = w.Clock.Now().Instant.Add(CutInterval)
			if err != nil {
				return err
			}
		}
		if err = w.Clock.Sleep(ctx, ExportInterval); err != nil {
			return err
		}
	}
}
