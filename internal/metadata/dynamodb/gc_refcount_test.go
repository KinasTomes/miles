package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestLocalGCRefcountIsDerivedNotDeletionAuthority(t *testing.T) {
	for _, mode := range []string{"live-zero", "deleted-stale-count", "recovery-zero"} {
		t.Run(mode, func(t *testing.T) {
			m, w, j, policy, _, _, chunk := lifeFixture(t, "gc", s.Standard)
			ctx := t.Context()
			if err := m.update(ctx, func(tx *transaction) error {
				count := int64(0)
				if mode == "deleted-stale-count" {
					count = 1000
				}
				if err := r1Put(tx, stringKey("SystemConfig", "config_id", "recovery/refcount/"+chunk.Key.Domain+"/"+chunk.Key.Hash), struct{ Count int64 }{count}); err != nil {
					return err
				}
				if mode != "live-zero" {
					page, err := m.ObservePage(ctx, s.ObservationRequest{Kind: "heads", Limit: 100})
					if err != nil {
						return err
					}
					h := page.Heads[0]
					return put(tx, headKey(h.Object), s.ObjectHead{Version: 1, Object: h.Object, Domain: h.Domain, Revision: h.Revision + 1, OpID: h.OpID, Deleted: true})
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000,
				RecoveryRoots: func(context.Context) ([]s.PackRef, error) {
					if mode == "recovery-zero" {
						return []s.PackRef{chunk.Location.Pack}, nil
					}
					return nil, nil
				}}
			sweep := &sweepPhysicalFixture{}
			w.Sweep = sweep
			out, err := w.Step(ctx, j, policy)
			rows, readErr := m.GetChunks(ctx, []s.ChunkKey{chunk.Key}, true)
			if readErr != nil || len(sweep.calls) != 0 {
				t.Fatal(rows, readErr, sweep.calls)
			}
			if mode == "deleted-stale-count" {
				if err != nil || !out.Done || len(rows) != 0 {
					t.Fatal("stale count became authoritative", out, err, rows)
				}
			} else if !errors.Is(err, s.ErrR1Busy) || out.Done || rows[chunk.Key] != chunk {
				t.Fatal("zero count deleted a protected winner", out, err, rows)
			}
		})
	}
}

type gcCountTransactionAPI struct {
	API
	calls int
}

func (a *gcCountTransactionAPI) TransactWriteItems(ctx context.Context, in *ddb.TransactWriteItemsInput, opts ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	a.calls++
	return a.API.TransactWriteItems(ctx, in, opts...)
}

func TestLocalOversizedTransactionNeverDispatchesOrPartiallyPublishes(t *testing.T) {
	m, _, _, _ := sharedFixture(t)
	ctx := t.Context()
	api := &gcCountTransactionAPI{API: m.api}
	m.api = api
	for attempt := 0; attempt < 2; attempt++ {
		err := m.update(ctx, func(tx *transaction) error {
			for i := 0; i <= MaxTransactionActions; i++ {
				if err := r1Put(tx, stringKey("SystemConfig", "config_id", fmt.Sprintf("fixture/oversized/%03d", i)), struct{ Value int }{i}); err != nil {
					return err
				}
			}
			return nil
		})
		if !errors.Is(err, s.ErrInvalid) || api.calls != 0 {
			t.Fatal("oversized retry reached backend", attempt, err, api.calls)
		}
		for i := 0; i <= MaxTransactionActions; i++ {
			err = m.view(ctx, func(tx *transaction) error {
				_, err := r1Read[struct{ Value int }](tx, stringKey("SystemConfig", "config_id", fmt.Sprintf("fixture/oversized/%03d", i)))
				return err
			})
			if !errors.Is(err, s.ErrNotFound) {
				t.Fatal("partial transaction visible", attempt, i, err)
			}
		}
	}
}
