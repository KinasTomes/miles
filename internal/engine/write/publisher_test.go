package write

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

type testGuard struct {
	store CanonicalStore
	allow bool
}

func (g testGuard) Pin(ctx context.Context, row storage.Chunk) (bool, error) {
	current, err := g.store.GetChunks(ctx, []storage.ChunkKey{row.Key}, true)
	if err != nil {
		return false, err
	}
	if current[row.Key] != row {
		return false, nil
	}
	packs, err := g.store.GetPacks(ctx, []storage.PackRef{row.Location.Pack})
	if err != nil {
		return false, err
	}
	return g.allow && packs[row.Location.Pack].State == "Warm", nil
}

func publisherFixture(t *testing.T, allow bool) (*StorePublisher, *fake.Metadata, *fake.Packs) {
	t.Helper()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	fence := storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "dev", Epoch: 1}
	m := fake.NewMetadata(fake.Seed{Now: func() time.Time { return now }, Writers: []storage.WriterLease{{Fence: fence, ExpiresAt: now.Add(time.Hour)}}})
	p := fake.NewPacks(func() time.Time { return now })
	pub := &StorePublisher{Metadata: m, Packs: p, Guard: testGuard{store: m, allow: allow}, Writer: fence, ProtectedUntil: now.Add(time.Hour)}
	return pub, m, p
}

func onePack(t *testing.T) (storage.PackRef, []byte, []byte, []pack.Entry) {
	t.Helper()
	raw := bytes.Repeat([]byte("hello"), 4000)
	key, err := engine.HashChunk("default", raw)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := pack.NewRef()
	if err != nil {
		t.Fatal(err)
	}
	b, err := pack.NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Add(pack.Blob{Key: key, Codec: "raw", RawLength: uint32(len(raw)), Stored: raw}); err != nil {
		t.Fatal(err)
	}
	data, sidecar, entries, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return ref, data, sidecar, entries
}

func TestPublisherRequiresCompletePhysicalPackBeforeCanonical(t *testing.T) {
	pub, metadata, packs := publisherFixture(t, true)
	ref, data, sidecar, entries := onePack(t)
	pub.Packs = failingSidecar{PackStore: packs}
	_, err := pub.Publish(context.Background(), ref, data, sidecar, entries, nil)
	if !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	states, err := metadata.GetPacks(context.Background(), []storage.PackRef{ref})
	if err != nil || states[ref].State != "Uploading" {
		t.Fatalf("pack prematurely Warm: %+v %v", states[ref], err)
	}
	rows, err := metadata.GetChunks(context.Background(), []storage.ChunkKey{entries[0].Key}, true)
	if err != nil || len(rows) != 0 {
		t.Fatalf("premature canonical row: %+v %v", rows, err)
	}
}

type failingSidecar struct{ storage.PackStore }

func (f failingSidecar) PutSidecar(context.Context, storage.PackRef, int64, io.Reader) error {
	return storage.ErrUnavailable
}

func TestPublisherInsertAndReviveWithLocationCAS(t *testing.T) {
	ctx := context.Background()
	pub, metadata, _ := publisherFixture(t, false)
	ref, data, sidecar, entries := onePack(t)
	first, err := pub.Publish(ctx, ref, data, sidecar, entries, nil)
	if err != nil || first[entries[0].Key].LocationRevision != 1 {
		t.Fatalf("insert: %+v %v", first, err)
	}
	newRef, newData, newSidecar, newEntries := onePack(t)
	second, err := pub.Publish(ctx, newRef, newData, newSidecar, newEntries, first)
	if err != nil || second[entries[0].Key].Location.Pack != newRef || second[entries[0].Key].LocationRevision != 2 {
		t.Fatalf("CAS: %+v %v", second, err)
	}
	rows, err := metadata.GetChunks(ctx, []storage.ChunkKey{entries[0].Key}, true)
	if err != nil || rows[entries[0].Key] != second[entries[0].Key] {
		t.Fatalf("canonical after CAS: %+v %v", rows, err)
	}
}

type insertRace struct {
	*fake.Metadata
	winner storage.Chunk
	writer storage.WriterFence
	fired  bool
}

func (m *insertRace) PutChunkIfAbsent(ctx context.Context, row storage.Chunk, writer storage.WriterFence) (storage.Chunk, bool, error) {
	if !m.fired {
		m.fired = true
		if _, _, err := m.Metadata.PutChunkIfAbsent(ctx, m.winner, m.writer); err != nil {
			return storage.Chunk{}, false, err
		}
	}
	return m.Metadata.PutChunkIfAbsent(ctx, row, writer)
}

func TestDuplicateRaceKeepsWinnerAndLeavesLoserOrphan(t *testing.T) {
	ctx := context.Background()
	pub, metadata, physical := publisherFixture(t, true)
	ref, data, sidecar, entries := onePack(t)
	winnerRef, err := pack.NewRef()
	if err != nil {
		t.Fatal(err)
	}
	metadata.SetPack(storage.PackInfo{Version: 1, Ref: winnerRef, State: "Warm", Class: storage.Standard, Size: int64(len(data))})
	winner := storage.Chunk{Version: 1, Key: entries[0].Key, RawBytes: int64(entries[0].RawLength),
		Location: storage.ChunkLocation{Pack: winnerRef, Range: storage.ByteRange{Offset: 0, Length: int64(entries[0].StoredLen)}, Codec: "raw", CRC32C: entries[0].CRC32C}, LocationRevision: 1}
	race := &insertRace{Metadata: metadata, winner: winner, writer: pub.Writer}
	pub.Metadata = race
	pub.Guard = testGuard{store: race, allow: true}
	resolved, err := pub.Publish(ctx, ref, data, sidecar, entries, nil)
	if err != nil || resolved[entries[0].Key].Location.Pack != winnerRef {
		t.Fatalf("race did not retain winner: %+v %v", resolved, err)
	}
	if _, err := physical.HeadPack(ctx, ref); err != nil {
		t.Fatal("loser pack should remain as an orphan candidate:", err)
	}
	current, err := metadata.GetChunks(ctx, []storage.ChunkKey{entries[0].Key}, true)
	if err != nil || current[entries[0].Key].Location.Pack != winnerRef {
		t.Fatalf("loser became canonical: %+v %v", current, err)
	}
}

func TestPublisherRejectsStaleWriterBeforePhysicalIO(t *testing.T) {
	pub, _, physical := publisherFixture(t, true)
	pub.Writer.Epoch++
	ref, data, sidecar, entries := onePack(t)
	_, err := pub.Publish(context.Background(), ref, data, sidecar, entries, nil)
	if !errors.Is(err, storage.ErrFenced) {
		t.Fatal(err)
	}
	if _, err := physical.HeadPack(context.Background(), ref); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("stale writer reached physical store: %v", err)
	}
}
