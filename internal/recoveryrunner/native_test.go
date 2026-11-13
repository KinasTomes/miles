package recoveryrunner

import (
	"context"
	"errors"
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
)

type leaseProbe struct {
	s.R1Store
	closed                bool
	checkErr, dispatchErr error
	released              bool
}

func (p *leaseProbe) AcquireR1(context.Context, s.R1Acquire) (s.R1Lease, error) {
	return s.R1Lease{}, nil
}
func (p *leaseProbe) CloseR1(context.Context, s.WriterFence) error { p.closed = true; return nil }
func (p *leaseProbe) CheckR1(context.Context, s.WriterFence) (s.R1Lease, error) {
	return s.R1Lease{}, p.checkErr
}
func (p *leaseProbe) BeginR1Action(context.Context, s.WriterFence, string) error {
	return p.dispatchErr
}
func (p *leaseProbe) ReleaseR1(context.Context, s.WriterFence) error { p.released = true; return nil }

func TestLeaseFaultNeedsNativeFencedDispatchNotUnavailable(t *testing.T) {
	for _, fault := range []error{nil, s.ErrUnavailable, s.ErrFenced} {
		p := &leaseProbe{checkErr: s.ErrFenced, dispatchErr: fault}
		ok, dispatch, err := InjectLeaseFault(t.Context(), p, s.R1Acquire{Kind: "control", Fence: s.WriterFence{Epoch: 1}})
		if errors.Is(fault, s.ErrFenced) {
			if err != nil || !ok || !dispatch || !p.released {
				t.Fatal(ok, dispatch, err)
			}
		} else if err == nil || p.released {
			t.Fatal("unverified dispatch result granted release", err)
		}
	}
}

func TestLeaseFaultCannotCloseRuntimeWriter(t *testing.T) {
	p := &leaseProbe{}
	if _, _, err := InjectLeaseFault(t.Context(), p, s.R1Acquire{Kind: "writer", Fence: s.WriterFence{Epoch: 1}}); err == nil || p.closed {
		t.Fatal("runtime writer was closed")
	}
}
