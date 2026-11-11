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
	next  error
	calls int
}

func (a *rejectedRecoveryAuthority) Acquire(context.Context, string, string) (storage.WriterFence, error) {
	a.calls++
	if a.calls > 1 && a.next != nil {
		return storage.WriterFence{}, a.next
	}
	return storage.WriterFence{}, a.err
}

func TestShutdownJournalRetriesUnavailableThenRetainsFencing(t *testing.T) {
	authority := &rejectedRecoveryAuthority{err: storage.ErrUnavailable, next: storage.ErrFenced}
	journal := journalRuntime{authority: authority, clock: clock.NewReal()}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := journal.flush(ctx); !errors.Is(err, storage.ErrFenced) || authority.calls != 2 {
		t.Fatal("transient backend failure did not retry exactly until fenced", err, authority.calls)
	}
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
		{"backend unavailable until deadline", storage.ErrUnavailable, context.DeadlineExceeded},
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
