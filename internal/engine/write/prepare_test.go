package write

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

type lostPageACK struct {
	PageStore
	once bool
}

func (s *lostPageACK) WriteManifestPages(ctx context.Context, pages []storage.ManifestPage, writer storage.WriterFence) error {
	if err := s.PageStore.WriteManifestPages(ctx, pages, writer); err != nil {
		return err
	}
	if !s.once {
		s.once = true
		return storage.ErrUnavailable
	}
	return nil
}

func TestPrepareObjectPublishesOnlyAfterPhysicalDependencies(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	meta, err := badger.Open(badger.Options{Path: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	object := storage.ObjectKey{Bucket: "bucket", BucketID: "bucket-1", Key: "object"}
	fence := storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "dev", Epoch: 1}
	if err := meta.Bootstrap(ctx, badger.Bootstrap{
		Buckets:     []storage.Bucket{{Name: "bucket", ID: "bucket-1", Domain: "default", OwnerID: "owner", Active: true}},
		Users:       []storage.User{{ID: "owner", Active: true}},
		Credentials: []storage.Credential{{ID: "owner-key", UserID: "owner", Active: true}},
		Quotas:      []storage.LogicalQuota{{OwnerID: "owner", Limit: 1 << 20}},
		Writers:     []storage.WriterLease{{Fence: fence, ExpiresAt: now.Add(time.Hour)}},
	}); err != nil {
		t.Fatal(err)
	}
	physical := fake.NewPacks(nil)
	pub := &StorePublisher{Metadata: meta, Packs: physical, Guard: testGuard{store: meta, allow: true},
		Writer: fence, ProtectedUntil: now.Add(time.Hour)}
	pipeline := Pipeline{Lookup: meta, Publisher: pub, Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	body := bytes.Repeat([]byte("data"), 10<<10)
	manifestID, err := NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	pub.Packs = failingSidecar{PackStore: physical}
	if _, err := PrepareObject(ctx, pipeline, meta, fence, bytes.NewReader(body), "default", manifestID); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("missing sidecar was accepted: %v", err)
	}
	if _, err := meta.GetObject(ctx, object); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("failed preparation exposed head: %v", err)
	}
	pub.Packs = physical
	manifestID, err = NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareObject(ctx, pipeline, meta, fence, bytes.NewReader(body), "default", manifestID)
	if err != nil || prepared.Size != int64(len(body)) || prepared.ChunkCount == 0 || prepared.PageCount == 0 {
		t.Fatalf("preparation: %+v %v", prepared, err)
	}
	if _, err := meta.GetObject(ctx, object); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("preparation itself exposed head: %v", err)
	}
	opID := fmt.Sprintf("%x", sha256.Sum256([]byte("operation-1")))[:32]
	historyID := "11111111111111111111111111111111"
	eventID, err := storage.EventID(historyID, opID, "Objects", "object_commit")
	if err != nil {
		t.Fatal(err)
	}
	reservation := storage.Reservation{Version: 1, ID: "reservation-1", OpID: opID, OwnerID: "owner", Object: object,
		Revision: 1, State: storage.ReservationActive, Deadline: now.Add(time.Hour), Bytes: int64(len(body)),
		StagedBytes: int64(len(body)), Writer: fence}
	if err := meta.ReserveLogicalQuota(ctx, reservation); err != nil {
		t.Fatal(err)
	}
	request := storage.CommitRequest{Head: storage.ObjectHead{Version: 1, Object: object, Domain: "default", ManifestID: manifestID,
		Size: prepared.Size, ETag: "test-etag", OpID: reservation.OpID, PageCount: prepared.PageCount,
		ChunkCount: prepared.ChunkCount, PageIndex: prepared.PageIndex},
		ExpectedRevision: 0, ReservationID: reservation.ID, ReservationRevision: 1, UserID: "owner", CredentialID: "owner-key", Writer: fence,
		Event: storage.OutboxEvent{Version: 1, ID: eventID, HistoryID: historyID, Epoch: 1, Entity: "Objects", Action: "object_commit", Object: object,
			Revision: 1, OpID: reservation.OpID, PayloadPointer: "prepared-test-payload", PayloadSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	committed, err := meta.CommitObject(ctx, request)
	if err != nil || committed.Head.ManifestID != manifestID || committed.Head.Size != int64(len(body)) {
		t.Fatalf("commit after preparation: %+v %v", committed, err)
	}
	var reconstructed []byte
	for n := uint32(0); n < prepared.PageCount; n++ {
		page, err := meta.GetManifestPage(ctx, manifestID, n)
		if err != nil {
			t.Fatal(err)
		}
		for _, ref := range page.Chunks {
			rows, err := meta.GetChunks(ctx, []storage.ChunkKey{ref.Key}, true)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := pack.ReadCanonicalChunk(ctx, physical, rows[ref.Key])
			if err != nil {
				t.Fatal(err)
			}
			reconstructed = append(reconstructed, decoded...)
		}
	}
	if !bytes.Equal(reconstructed, body) {
		t.Fatal("committed object cannot be reconstructed from canonical Warm packs")
	}
}

func TestPrepareEmptyObjectAndReconcilePageLostACK(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	fence := storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "dev", Epoch: 1}
	meta := fake.NewMetadata(fake.Seed{Now: func() time.Time { return now }, Writers: []storage.WriterLease{{Fence: fence, ExpiresAt: now.Add(time.Hour)}}})
	m := newMemoryPublisher()
	pipeline := Pipeline{Lookup: m, Publisher: m, Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression: engine.CompressionPolicy{Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	prepared, err := PrepareObject(ctx, pipeline, meta, fence, bytes.NewReader(nil), "default", "empty-manifest")
	if err != nil || prepared.Size != 0 || prepared.PageCount != 0 || prepared.PackCount != 0 {
		t.Fatalf("empty object: %+v %v", prepared, err)
	}
	key := storage.ChunkKey{Domain: "default", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	page := storage.ManifestPage{Version: 1, ManifestID: "page-lost-ack", Domain: "default", Number: 0,
		Chunks: []storage.ChunkRef{{Key: key, RawBytes: 16384, LogicalOffset: 0}}}
	body, err := manifest.Encode(manifest.ObjectPage, page)
	if err != nil {
		t.Fatal(err)
	}
	store := &lostPageACK{PageStore: meta}
	if err := writeVerifiedPage(ctx, manifest.ObjectPage, store, fence, page, sha256.Sum256(body)); err != nil || !store.once {
		t.Fatalf("lost page ACK was not reconciled: %v", err)
	}
}
