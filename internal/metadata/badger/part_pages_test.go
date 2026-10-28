package badger

import (
	"context"
	"testing"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	db "github.com/dgraph-io/badger/v4"
)

func TestPartPagesSurviveReopenAndRejectWrongKind(t *testing.T) {
	f := contracttest.NewFixture()
	m, path := fixtureStore(t, f)
	ctx := context.Background()
	p := s.ManifestPage{Version: 1, ManifestID: "durable-part", Domain: f.Bucket.Domain, Chunks: []s.ChunkRef{{Key: f.Chunk(f.Bucket.Domain).Key, RawBytes: 4}}}
	if err := m.WritePartPages(ctx, []s.ManifestPage{p}, f.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	m = reopen(t, m, path, f.Now)
	defer m.Close()
	got, err := m.GetPartPage(ctx, p.ManifestID, 0)
	if err != nil || got.Chunks[0].RawBytes != 4 {
		t.Fatal(got, err)
	}
	// Valid object-page checksum still cannot pass as a part-page record.
	body, err := manifest.Encode(manifest.ObjectPage, p)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.update(ctx, func(tx *db.Txn) error { return backend(tx.Set([]byte(partPageKey(p.ManifestID, 0)), body)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := m.GetPartPage(ctx, p.ManifestID, 0); err == nil {
		t.Fatal("wrong kind accepted")
	}
}
