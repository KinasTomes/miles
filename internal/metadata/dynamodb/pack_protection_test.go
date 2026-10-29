package dynamodb

import (
	"context"
	"errors"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type protectionInterleave struct {
	API
	before func()
}

func (a *protectionInterleave) TransactWriteItems(ctx context.Context, r *ddb.TransactWriteItemsInput, o ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error) {
	if a.before != nil {
		fn := a.before
		a.before = nil
		fn()
	}
	return a.API.TransactWriteItems(ctx, r, o...)
}

func TestLocalProtectionRechecksStateAndLocationAtTransaction(t *testing.T) {
	for _, race := range []string{"retire", "location"} {
		t.Run(race, func(t *testing.T) {
			f := contracttest.NewFixture()
			f.Packs[0].StateRevision = 1
			f.Packs[0].ProtectedUntil = f.Now.Add(2 * time.Minute)
			m, _ := fixtureStore(t, f)
			ctx := context.Background()
			row := f.Chunk("default")
			if _, _, e := m.PutChunkIfAbsent(ctx, row, f.Lease.Fence); e != nil {
				t.Fatal(e)
			}
			// A max-no-op STILL requires an authoritative condition at commit.
			r := s.PackProtectionUpdate{Chunk: row, ExpectedStateRevision: 1, RequiredUntil: f.Now.Add(time.Minute), Writer: f.Lease.Fence}
			m.api = &protectionInterleave{API: m.api, before: func() {
				if err := m.update(ctx, func(tx *transaction) error {
					if race == "retire" {
						p := f.Packs[0]
						p.State = "Retiring"
						p.StateRevision++
						return put(tx, packKey(p.Ref), p)
					}
					changed := row
					changed.LocationRevision++
					return put(tx, chunkKey(row.Key), changed)
				}); err != nil {
					t.Fatal(err)
				}
			}}
			if _, err := m.ExtendPackProtection(ctx, r); !errors.Is(err, s.ErrConflict) {
				t.Fatal("stale binding authorized at transaction", race, err)
			}
			packs, e := m.GetPacks(ctx, []s.PackRef{row.Location.Pack})
			if e != nil || !packs[row.Location.Pack].ProtectedUntil.Equal(f.Packs[0].ProtectedUntil) {
				t.Fatal(packs, e)
			}
		})
	}
}
