package readpath

import (
	"context"
	"errors"
	"fmt"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/storage"
)

var ErrReadAheadInvalid = errors.New("invalid read-ahead input or executor result")

// ChunkExecutor is a PROPOSAL integration seam, not a verifier or authority.
// Estimate is synchronous, bounded, metadata-only and side-effect-free. It must
// cover peak simultaneously owned resources, including cache-hit copies and
// decoder workspace. No allocation of payloads or physical I/O before admission.
// Execute may run concurrently. Success transfers exclusive ownership of a full
// verified raw chunk; failure transfers no usable bytes. Its returned buffer's
// backing allocation must fit RawBytes in the cost. It must stop/join its own
// work before returning, honor cancellation, and retain no returned-buffer alias.
// Production integration must supply canonical identity, exact-generation pins,
// archive preflight and real verification. This interface proves none of those.
type ChunkExecutor interface {
	Estimate(index int, chunk engine.PlannedChunk) (Cost, error)
	Execute(ctx context.Context, index int, chunk engine.PlannedChunk) ([]byte, error)
}

// ChunkEmitter borrows a verified logical slice only until return; it must not
// retain it or spawn work using it. It must honor cancellation and, in future
// protected-reader integration, revalidate authority/deadline before emission.
// Bytes emitted earlier cannot be retracted; this is not atomic HTTP delivery.
type ChunkEmitter func(context.Context, int, []byte) error

type readJob struct {
	done        chan struct{}
	raw         []byte // read only after done closes
	reservation *Reservation
}

// RunReadAhead owns its admission ticket and joins all workers before returning.
// Plan and executor metadata must remain immutable for this call. W comes from
// the pinned bucket snapshot; maxOutstanding is explicit K (no default).
// Only contiguous occurrences are launched: a denied prefetch stops expansion,
// so later speculation never occupies this request's budget ahead of an
// unreserved demand. Other requests can exhaust quota; demand then returns the
// admission error without polling or a hidden retry/fairness guarantee.
// Any observed executor failure, including speculative corruption, cancels the
// request. Concurrent failures use the first context cause, not logical order.
// Callbacks must cooperate with cancellation; there is no safe forced goroutine
// termination. No reservation is released merely because cancellation occurred.
func RunReadAhead(ctx context.Context, admission *Admission, binding BucketBinding, requestBudget Budget, plan engine.ReadPlan, maxOutstanding int, executor ChunkExecutor, emit ChunkEmitter) error {
	if ctx == nil || admission == nil || executor == nil || emit == nil || maxOutstanding <= 0 {
		return ErrReadAheadInvalid
	}
	w := binding.Policy.Cache().ReadaheadChunks
	if w < 0 || w > 64 || plan.Object.Bucket != binding.Name || plan.Object.BucketID != binding.Policy.BucketID() {
		return ErrReadAheadInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateScheduledPlan(plan); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	ticket, err := admission.AdmitRequest(runCtx, binding, requestBudget)
	if err != nil {
		return err
	}
	// At most 65 job records/channels regardless of K or manifest length.
	limit := min(maxOutstanding, int(w)+1, len(plan.Chunks))
	jobs := make([]*readJob, 0, limit)
	defer func() {
		cancel(nil)
		for _, job := range jobs {
			<-job.done // includes result publication and executor quiescence
			job.raw = nil
			job.reservation.Release()
		}
		ticket.Close()
	}()
	for i := 0; i < len(plan.Chunks); i++ {
		// Completion alone never moves the window; i advances after emit returns.
		for len(jobs) < limit && len(jobs) < len(plan.Chunks)-i {
			if err := context.Cause(runCtx); err != nil {
				return err
			}
			index := i + len(jobs)
			chunk := plan.Chunks[index]
			cost, err := executor.Estimate(index, chunk)
			if err != nil {
				return err
			}
			if cost.ChunkSlots != 1 || cost.RawBytes < chunk.Ref.RawBytes {
				return ErrReadAheadInvalid
			}
			var reservation *Reservation
			if len(jobs) == 0 {
				reservation, err = admission.ReserveDemand(runCtx, ticket, cost)
			} else {
				reservation, err = admission.TryReservePrefetch(ticket, cost)
				if errors.Is(err, ErrExhausted) {
					break
				}
			}
			if err != nil {
				return err
			}
			job := &readJob{done: make(chan struct{}), reservation: reservation}
			jobs = append(jobs, job)
			go func() {
				defer close(job.done)
				if err := context.Cause(runCtx); err != nil {
					return
				}
				raw, err := executor.Execute(runCtx, index, chunk)
				if err == nil && (int64(len(raw)) != chunk.Ref.RawBytes || int64(cap(raw)) > cost.RawBytes) {
					err = ErrReadAheadInvalid
				}
				if err != nil {
					cancel(fmt.Errorf("occurrence %d: %w", index, err))
					return
				}
				job.raw = raw
			}()
		}
		job := jobs[0]
		select {
		case <-runCtx.Done():
			return context.Cause(runCtx)
		case <-job.done:
		}
		if err := context.Cause(runCtx); err != nil {
			return err
		}
		s := plan.Chunks[i].Slice
		if err := emit(runCtx, i, job.raw[s.Offset:s.Offset+s.Length:s.Offset+s.Length]); err != nil {
			return err
		}
		// No worker or consumer may retain the buffer after the callback returns.
		job.raw = nil
		job.reservation.Release()
		copy(jobs, jobs[1:])
		jobs[len(jobs)-1] = nil
		jobs = jobs[:len(jobs)-1]
	}
	return context.Cause(runCtx)
}

// Defensive slice/coverage checks, not manifest completeness or authorization.
func validateScheduledPlan(p engine.ReadPlan) error {
	if p.Object.Validate() != nil || p.HeadRevision == 0 || p.ManifestID == "" || p.Range.Offset < 0 || p.Range.Length < 0 || p.Range.Offset > storage.MaxObjectBytes || p.Range.Length > storage.MaxObjectBytes-p.Range.Offset {
		return ErrReadAheadInvalid
	}
	cursor, end := p.Range.Offset, p.Range.Offset+p.Range.Length
	for _, c := range p.Chunks {
		if c.Ref.Key.Validate() != nil || c.Ref.RawBytes <= 0 || c.Ref.RawBytes > engine.MaxChunkBytes || c.Ref.LogicalOffset < 0 || c.Ref.LogicalOffset > storage.MaxObjectBytes-c.Ref.RawBytes || c.Slice.Validate(c.Ref.RawBytes) != nil {
			return ErrReadAheadInvalid
		}
		if c.Ref.LogicalOffset+c.Slice.Offset != cursor || c.Slice.Length > end-cursor {
			return ErrReadAheadInvalid
		}
		cursor += c.Slice.Length
	}
	if cursor != end {
		return ErrReadAheadInvalid
	}
	return nil
}
