package lifecycle

import (
	"context"

	"github.com/KinasTomes/miles/internal/jobs"
	s "github.com/KinasTomes/miles/internal/storage"
)

type SweepStorage interface {
	ListSweepVersions(context.Context, s.PackRef) ([]s.SweepVersion, error)
	DeleteSweepVersion(context.Context, s.PackRef, s.SweepVersion) error
}

type sweepDispatch interface {
	DispatchSweep(context.Context, jobs.Job, []s.SweepVersion) (Action, error)
	RetireCanonical(context.Context, jobs.Job, s.Chunk) error
	RetireSweep(context.Context, jobs.Job) (Action, error)
}

// The store reserves maintenance before this scan. The later dispatch binds
// the intent to that same gate revision. Dry-run reports never reach here.
func (w *Worker) sweepPlan(ctx context.Context, a Action) ([]s.SweepVersion, []s.Chunk, error) {
	if w.Marker == nil || w.Sweep == nil {
		return nil, nil, s.ErrInvalid
	}
	mark, e := w.Marker.Collect(ctx)
	if e != nil {
		return nil, nil, e
	}
	if mark.Roots[a.Source.Ref] != 0 {
		return nil, nil, s.ErrR1Busy
	}
	canonical := mark.CanonicalFor(a.Source.Ref)
	if a.Source.State == "Warm" {
		for _, chunk := range canonical {
			if mark.Referenced[chunk.Key] {
				return nil, nil, s.ErrR1Busy
			}
		}
		return nil, canonical, nil
	}
	if len(canonical) != 0 {
		return nil, nil, s.ErrR1Busy
	}
	if !a.RetiredAt.IsZero() {
		if w.VerifyCutAfter == nil {
			return nil, nil, s.ErrUnavailable
		}
		if e = w.VerifyCutAfter(ctx, a.RetiredAt); e != nil {
			return nil, nil, e
		}
	}
	versions, e := w.Sweep.ListSweepVersions(ctx, a.Source.Ref)
	if e != nil {
		return nil, nil, e
	}
	if !s.ValidSweepVersions(versions) {
		return nil, nil, s.ErrUnavailable
	}
	return versions, nil, nil
}
