package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

type rejectedRecoveryAuthority struct {
	err   error
	calls int
}

func (a *rejectedRecoveryAuthority) Acquire(context.Context, string, string) (storage.WriterFence, error) {
	a.calls++
	return storage.WriterFence{}, a.err
}
func (*rejectedRecoveryAuthority) Renew(context.Context, storage.WriterFence) error {
	return storage.ErrInvalid
}
func (*rejectedRecoveryAuthority) Close(context.Context, storage.WriterFence) error {
	return storage.ErrInvalid
}

func TestShutdownJournalRetryPreservesDeadlineAndFencing(t *testing.T) {
	for _, test := range []struct {
		name    string
		failure error
		want    error
	}{
		{"busy until deadline", storage.ErrR1Busy, context.DeadlineExceeded},
		{"fenced", storage.ErrFenced, storage.ErrFenced},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority := &rejectedRecoveryAuthority{err: test.failure}
			journal := journalRuntime{authority: authority, clock: clock.NewReal()}
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			if err := journal.flush(ctx); !errors.Is(err, test.want) {
				t.Fatal("shutdown changed authority/deadline failure", err)
			}
			if authority.calls != 1 {
				t.Fatal("retried after fencing or the shutdown deadline", authority.calls)
			}
		})
	}
}
