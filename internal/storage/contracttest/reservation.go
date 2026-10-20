package contracttest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
)

func runReservations(t *testing.T, newStore MetadataFactory) {
	ctx := context.Background()
	for _, committed := range []bool{false, true} {
		name := "unknown outcome before commit"
		if committed {
			name = "unknown outcome after commit"
		}
		t.Run(name, func(t *testing.T) {
			f := NewFixture()
			h := newStore(t, f)
			r := f.Request("unknown", 0)
			Prepare(t, h.Store, f, r, 4)
			original, err := h.Store.GetReservation(ctx, r.ReservationID)
			if err != nil || original.OpID != r.Head.OpID || original.BaseRevision != 0 || original.BaseSize != 0 || original.Revision != 1 || original.State != s.ReservationActive {
				t.Fatal("incomplete reservation", original, err)
			}
			h.FailCommit(!committed, committed)
			_, err = h.Store.CommitObject(ctx, r)
			requireError(t, err, s.ErrUnavailable)
			h.FailCommit(false, false)
			next := f.Lease
			next.Fence.Epoch++
			h.SetWriter(next)
			claim := s.ReservationClaim{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: next.Fence}
			before, _ := h.Store.GetQuota(ctx, "owner")
			h.FailReconcile(true)
			_, err = h.Store.ClaimReservation(ctx, claim)
			requireError(t, err, s.ErrUnavailable)
			// Cleanup must also fail closed when an outcome read is unavailable.
			requireError(t, h.Store.ReleaseLogicalQuota(ctx, s.ReservationRelease{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: f.Lease.Fence}), s.ErrFenced)
			after, _ := h.Store.GetQuota(ctx, "owner")
			if before != after {
				t.Fatal("unknown outcome changed quota")
			}
			h.FailReconcile(false)
			resolved, err := h.Store.ClaimReservation(ctx, claim)
			if err != nil {
				t.Fatal(err)
			}
			if committed {
				if resolved.Committed == nil || !resolved.Committed.AlreadyCommitted || resolved.Committed.Head.Revision != 1 || resolved.Reservation.State != s.ReservationCommitted || resolved.Reservation.Revision != 2 || resolved.Reservation.Writer != f.Lease.Fence {
					t.Fatal("lost ACK not resolved", resolved)
				}
			} else {
				NoPublication(t, h.Store, r)
				want := original
				want.Writer = next.Fence
				want.Revision++
				if resolved.Committed != nil || resolved.Reservation != want {
					t.Fatal("takeover changed operation binding", resolved)
				}
				_, err = h.Store.ClaimReservation(ctx, claim)
				requireError(t, err, s.ErrConflict)
				r.ReservationRevision = resolved.Reservation.Revision
			}
			after, _ = h.Store.GetQuota(ctx, "owner")
			if before != after {
				t.Fatal("claim counted quota twice")
			}
			_, err = h.Store.CommitObject(ctx, r)
			requireError(t, err, s.ErrFenced)
			r.Writer = next.Fence
			result, err := h.Store.CommitObject(ctx, r)
			if err != nil || result.AlreadyCommitted != committed || result.Head.Revision != 1 {
				t.Fatal("resume failed", result, err)
			}
			again, err := h.Store.CommitObject(ctx, r)
			if err != nil || !again.AlreadyCommitted || !reflect.DeepEqual(result.Head, again.Head) {
				t.Fatal("retry applied twice", again, err)
			}
			quota, err := h.Store.GetQuota(ctx, "owner")
			if err != nil || quota.Used != 4 || quota.Reserved != 0 || quota.Staged != 0 {
				t.Fatal("double quota", quota, err)
			}
			ledger, err := h.Store.GetReservation(ctx, r.ReservationID)
			if err != nil || ledger.State != s.ReservationCommitted {
				t.Fatal("terminal outcome lost", ledger, err)
			}
			// Resolve first; a terminal committed ledger can never be released.
			release := s.ReservationRelease{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: ledger.Revision, Writer: next.Fence}
			if committed {
				requireError(t, h.Store.ReleaseLogicalQuota(ctx, release), s.ErrFenced)
			} else {
				requireError(t, h.Store.ReleaseLogicalQuota(ctx, release), s.ErrConflict)
			}
		})
	}
	t.Run("orphan claim release CAS and retained terminal state", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		r := f.Request("orphan", 0)
		Prepare(t, h.Store, f, r, 4)
		worker := f.Lease
		worker.Fence.LeaseID = "second-worker"
		h.SetWriter(worker)
		claim := s.ReservationClaim{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: worker.Fence}
		_, err := h.Store.ClaimReservation(ctx, claim)
		requireError(t, err, s.ErrConflict) // The original writer is still alive.
		old := f.Lease
		old.ExpiresAt = f.Now
		h.SetWriter(old)
		wrong := claim
		wrong.OpID = "another-op"
		_, err = h.Store.ClaimReservation(ctx, wrong)
		requireError(t, err, s.ErrConflict)
		got, err := h.Store.ClaimReservation(ctx, claim)
		if err != nil || got.Reservation.Revision != 2 {
			t.Fatal(got, err)
		}
		release := s.ReservationRelease{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: worker.Fence}
		requireError(t, h.Store.ReleaseLogicalQuota(ctx, release), s.ErrConflict)
		release.ExpectedRevision = 2
		h.FailReconcile(true)
		requireError(t, h.Store.ReleaseLogicalQuota(ctx, release), s.ErrUnavailable)
		quota, _ := h.Store.GetQuota(ctx, "owner")
		if quota.Reserved != 4 || quota.Staged != 4 {
			t.Fatal("unproven release", quota)
		}
		h.FailReconcile(false)
		for range 2 {
			if err := h.Store.ReleaseLogicalQuota(ctx, release); err != nil {
				t.Fatal(err)
			}
		}
		ledger, err := h.Store.GetReservation(ctx, r.ReservationID)
		if err != nil || ledger.State != s.ReservationReleased || ledger.Revision != 3 {
			t.Fatal("release lost terminal state", ledger, err)
		}
		quota, _ = h.Store.GetQuota(ctx, "owner")
		if quota.Used != 0 || quota.Reserved != 0 || quota.Staged != 0 {
			t.Fatal("double release", quota)
		}
		r.Writer, r.ReservationRevision = worker.Fence, ledger.Revision
		_, err = h.Store.CommitObject(ctx, r)
		requireError(t, err, s.ErrConflict)
		ledger.State, ledger.Revision = s.ReservationActive, 1
		requireError(t, h.Store.ReserveLogicalQuota(ctx, ledger), s.ErrConflict)
	})
	t.Run("reservation binds operation and base head snapshot", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		first := f.Request("original", 0)
		Prepare(t, h.Store, f, first, 4)
		if _, err := h.Store.CommitObject(ctx, first); err != nil {
			t.Fatal(err)
		}
		next := f.Request("overwrite", 1)
		Prepare(t, h.Store, f, next, 0)
		ledger, err := h.Store.GetReservation(ctx, next.ReservationID)
		if err != nil || ledger.BaseRevision != 1 || ledger.BaseSize != 4 || ledger.Bytes != 0 {
			t.Fatal("overwrite base not captured", ledger, err)
		}
		stale := ledger
		stale.ID = "stale-base"
		stale.OpID = "stale-op"
		stale.BaseRevision = 0
		stale.BaseSize = 0
		stale.Bytes = 4
		requireError(t, h.Store.ReserveLogicalQuota(ctx, stale), s.ErrConflict)
		forged := next
		forged.Head.OpID = "different-op"
		forged.Event.OpID = forged.Head.OpID
		_, err = h.Store.CommitObject(ctx, forged)
		requireError(t, err, s.ErrInvalid)
		if _, err := h.Store.CommitObject(ctx, next); err != nil {
			t.Fatal(err)
		}
		quota, _ := h.Store.GetQuota(ctx, "owner")
		if quota.Used != 4 || quota.Reserved != 0 || quota.Staged != 0 {
			t.Fatal("overwrite credit changed quota", quota)
		}
	})
	t.Run("concurrent orphan claims have one winner", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		r := f.Request("claim-race", 0)
		Prepare(t, h.Store, f, r, 4)
		old := f.Lease
		old.ExpiresAt = f.Now
		h.SetWriter(old)
		a, b := f.Lease, f.Lease
		a.Fence.LeaseID, b.Fence.LeaseID = "worker-a", "worker-b"
		h.SetWriter(a)
		h.SetWriter(b)
		wins := make(chan s.Reservation, 2)
		var wg sync.WaitGroup
		for _, w := range []s.WriterLease{a, b} {
			wg.Go(func() {
				result, err := h.Store.ClaimReservation(ctx, s.ReservationClaim{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: w.Fence})
				if err == nil {
					wins <- result.Reservation
				} else if !errors.Is(err, s.ErrConflict) {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		close(wins)
		if len(wins) != 1 {
			t.Fatalf("claim winners=%d", len(wins))
		}
		winner := <-wins
		r.Writer, r.ReservationRevision = winner.Writer, winner.Revision
		if _, err := h.Store.CommitObject(ctx, r); err != nil {
			t.Fatal(err)
		}
		quota, _ := h.Store.GetQuota(ctx, "owner")
		if quota.Used != 4 || quota.Reserved != 0 || quota.Staged != 0 {
			t.Fatal(quota)
		}
	})
}
