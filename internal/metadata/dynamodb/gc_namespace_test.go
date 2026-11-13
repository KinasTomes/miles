package dynamodb

import (
	"context"
	"errors"
	"testing"

	"github.com/KinasTomes/miles/internal/lifecycle"
	s "github.com/KinasTomes/miles/internal/storage"
)

func TestLocalGCNamespaceIdentityAndTombstone(t *testing.T) {
	for _, mode := range []string{"live", "tombstone", "recreated-bucket"} {
		t.Run(mode, func(t *testing.T) {
			m, w, j, policy, _, physical, chunk := lifeFixture(t, "gc", s.Standard)
			ctx := t.Context()
			heads, err := m.ObservePage(ctx, s.ObservationRequest{Kind: "heads", Limit: 100})
			if err != nil || len(heads.Heads) != 1 {
				t.Fatal(heads, err)
			}
			head := heads.Heads[0]
			if err := m.update(ctx, func(tx *transaction) error {
				switch mode {
				case "tombstone":
					return put(tx, headKey(head.Object), s.ObjectHead{Version: 1, Object: head.Object, Domain: head.Domain, Revision: head.Revision + 1, OpID: head.OpID, Deleted: true})
				case "recreated-bucket":
					b, err := get[s.Bucket](tx, key("buckets", head.Object.Bucket))
					if err != nil {
						return err
					}
					b.ID, b.Revision = "new-bucket-identity", b.Revision+1
					return put(tx, key("buckets", b.Name), b)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			w.Marker = &lifecycle.Marker{Store: m, Pinned: m.ObservePackPins, MaxRecords: 1000, MaxReferences: 1000,
				RecoveryRoots: func(context.Context) ([]s.PackRef, error) { return nil, nil }}
			sweep := &sweepPhysicalFixture{}
			w.Sweep = sweep
			out, err := w.Step(ctx, j, policy)
			rows, readErr := m.GetChunks(ctx, []s.ChunkKey{chunk.Key}, true)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "live" {
				if !errors.Is(err, s.ErrR1Busy) || out.Done || rows[chunk.Key] != chunk {
					t.Fatal("live namespace lost canonical data", out, err, rows)
				}
			} else if err != nil || !out.Done || !out.Settled || len(rows) != 0 {
				t.Fatal("old namespace retained as a live object", out, err, rows)
			}
			// Retirement and physical deletion are separate phases. A namespace
			// change alone cannot authorize deleting the retained generation.
			if len(sweep.calls) != 0 {
				t.Fatal("namespace mutation dispatched physical delete", sweep.calls)
			}
			if _, err = physical.HeadPack(ctx, chunk.Location.Pack); err != nil {
				t.Fatal("retirement lost retained payload", err)
			}
		})
	}
}
