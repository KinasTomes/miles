package dynamodb

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type closeFault struct {
	API
	before, after bool
	hook          func()
}

func (a *closeFault) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	closing := false
	for _, action := range in.TransactItems {
		if action.Put != nil && operationClosed(action.Put.Item) {
			closing = true
		}
	}
	if closing && a.hook != nil {
		hook := a.hook
		a.hook = nil
		hook()
	}
	if closing && a.before {
		return nil, s.ErrUnavailable
	}
	out, err := a.API.TransactWriteItems(ctx, in, opts...)
	if closing && a.after && err == nil {
		a.after = false
		return nil, s.ErrUnavailable
	}
	return out, err
}

func savedAttempt(t *testing.T, m *Store, op string) *ddb.TransactWriteItemsInput {
	t.Helper()
	row, err := m.transaction(context.Background()).load(key("outcomes", op))
	if err != nil {
		t.Fatal(err)
	}
	body, err := CloneBinary(row.after["pending_request"])
	if err != nil {
		t.Fatal(err)
	}
	attemptCodec()
	var input ddb.TransactWriteItemsInput
	if err := gob.NewDecoder(bytes.NewReader(body)).Decode(&input); err != nil {
		t.Fatal(err)
	}
	return &input
}

func TestLocalClosureRacesWithOriginalFinalTransaction(t *testing.T) {
	for _, commitWins := range []bool{false, true} {
		name := "closure-wins"
		if commitWins {
			name = "commit-wins"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := contracttest.NewFixture()
			m, client := fixtureStore(t, f)
			r := f.Request("close-race", 0)
			contracttest.Prepare(t, m, f, r, 4)
			m.api = &dispatchFault{API: client, objectTable: m.tables["Objects"], before: true}
			if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
				t.Fatal(err)
			}
			original := savedAttempt(t, m, r.Head.OpID)
			fault := &closeFault{API: client}
			m.api = fault
			if commitWins {
				fault.hook = func() {
					if _, err := client.TransactWriteItems(ctx, original); err != nil {
						t.Fatal(err)
					}
				}
			}
			claim := s.ReservationClaim{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: r.Writer}
			resolution, err := m.CloseCommitAttempt(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			if commitWins {
				if resolution.Committed == nil || resolution.Committed.Head.OpID != r.Head.OpID {
					t.Fatal(resolution)
				}
				if err := m.ReleaseLogicalQuota(ctx, s.ReservationRelease(claim)); !errors.Is(err, s.ErrConflict) {
					t.Fatal(err)
				}
			} else {
				if resolution.Committed != nil {
					t.Fatal(resolution)
				}
				if _, err := client.TransactWriteItems(ctx, original); !isKnownCancellation(err) {
					t.Fatal("late original request escaped closure", err)
				}
				if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrFenced) {
					t.Fatal("closed operation re-opened", err)
				}
				if err := m.ReleaseLogicalQuota(ctx, s.ReservationRelease(claim)); err != nil {
					t.Fatal(err)
				}
				contracttest.NoPublication(t, m, r)
			}
		})
	}
}

func TestLocalUnknownClosureCannotAuthorizeCleanup(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, client := fixtureStore(t, f)
	r := f.Request("unknown-close", 0)
	contracttest.Prepare(t, m, f, r, 4)
	m.api = &dispatchFault{API: client, objectTable: m.tables["Objects"], before: true}
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	revokeRequest(t, m, f, r, false)
	fault := &closeFault{API: client, before: true}
	m.api = fault
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	claim := s.ReservationClaim{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: r.Writer}
	if err := m.ReleaseLogicalQuota(ctx, s.ReservationRelease(claim)); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal("unknown closure released quota", err)
	}
	q, err := m.GetQuota(ctx, f.Quota.OwnerID)
	if err != nil || q.Reserved != 4 || q.Staged != 4 {
		t.Fatal(q, err)
	}
	fault.before = false
	fault.after = true
	if _, err := m.CloseCommitAttempt(ctx, claim); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	// Only a subsequent authoritative read of the Closed row resolves a lost ACK.
	if _, err := m.CloseCommitAttempt(ctx, claim); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := m.ReleaseLogicalQuota(ctx, s.ReservationRelease(claim)); err != nil {
			t.Fatal(err)
		}
	}
	q, err = m.GetQuota(ctx, f.Quota.OwnerID)
	if err != nil || q.Reserved != 0 || q.Staged != 0 || q.Used != 0 {
		t.Fatal(q, err)
	}
}
