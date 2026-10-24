package badger

import (
	"context"
	"errors"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
)

func TestUploadingPackCannotPublishHead(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	f.Packs[0].State = "Uploading"
	m, path := fixtureStore(t, f)
	defer func() { _ = m.Close() }()
	r := f.Request("unfinished-pack", 0)
	c := f.Chunk("default")
	if _, won, err := m.PutChunkIfAbsent(ctx, c, r.Writer); !errors.Is(err, s.ErrConflict) || won {
		t.Fatalf("Uploading insert: won=%v err=%v", won, err)
	}
	if got, err := m.GetChunks(ctx, []s.ChunkKey{c.Key}, true); err != nil || len(got) != 0 {
		t.Fatalf("canonical persisted: %+v %v", got, err)
	}
	// Even a caller that ignores the failed insert cannot publish its manifest.
	p := s.ManifestPage{Version: 1, ManifestID: r.Head.ManifestID, Domain: r.Head.Domain, Chunks: []s.ChunkRef{{Key: c.Key, RawBytes: 4}}}
	if err := m.WriteManifestPages(ctx, []s.ManifestPage{p}, r.Writer); err != nil {
		t.Fatal(err)
	}
	reservation := s.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: "owner", Object: r.Head.Object, Bytes: 4, StagedBytes: 4, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(time.Minute), Writer: r.Writer}
	if err := m.ReserveLogicalQuota(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("missing canonical committed: %v", err)
	}
	m = reopen(t, m, path, f.Now)
	contracttest.NoPublication(t, m, r)
	q, err := m.GetQuota(ctx, "owner")
	if err != nil || q.Used != 0 || q.Reserved != 4 || q.Staged != 4 {
		t.Fatalf("quota changed: %+v %v", q, err)
	}
	ledger, err := m.GetReservation(ctx, r.ReservationID)
	if err != nil || ledger.State != s.ReservationActive || ledger.Revision != 1 {
		t.Fatalf("reservation changed: %+v %v", ledger, err)
	}
}
