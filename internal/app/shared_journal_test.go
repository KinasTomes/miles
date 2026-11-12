package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/smithy-go"
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

func TestShutdownAttemptLogsClassifyWithoutRawErrors(t *testing.T) {
	var out bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, nil)))
	defer slog.SetDefault(old)
	private := "secret-token-private-bucket"
	authority := &rejectedRecoveryAuthority{err: fmt.Errorf("%s: %w", private, storage.ErrFenced)}
	journal := journalRuntime{authority: authority, clock: clock.NewReal()}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := journal.flush(ctx); !errors.Is(err, storage.ErrFenced) {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), private) {
		t.Fatal("raw error leaked")
	}
	var last map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "shutdown journal attempt" {
			last = record
		}
	}
	if last["attempt"] != float64(1) || last["stage"] != "acquire_authority" || last["work_failure"] != "fenced" || last["cleanup_failure"] != "none" || last["remaining_ms"].(float64) <= 0 {
		t.Fatal("missing classified attempt outcome", last)
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, "none"}, {storage.ErrR1Busy, "r1_gate_busy"}, {storage.ErrUnavailable, "backend_unavailable"},
		{fmt.Errorf("%w: %w", storage.ErrUnavailable, &smithy.GenericAPIError{Code: "ThrottlingException", Message: private}), "backend_throttled"},
		{fmt.Errorf("%w: %w", storage.ErrUnavailable, &smithy.GenericAPIError{Code: private, Message: private}), "backend_unavailable"},
		{storage.ErrClockUncertain, "clock_uncertainty_exceeded"}, {context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"}, {errors.New(private), "unknown"},
	} {
		if got := journalFailure(tc.err); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}
