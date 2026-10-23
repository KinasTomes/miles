package badger

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestPackLifecycleFencesAndWarmAdmission(t *testing.T) {
	now := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	m, err := Open(Options{Path: t.TempDir(), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx := context.Background()
	writer := storage.WriterFence{LeaseID: "writer", ServiceIncarnation: "dev", Epoch: 1}
	other := storage.WriterFence{LeaseID: "other", ServiceIncarnation: "dev", Epoch: 1}
	if err := m.Bootstrap(ctx, Bootstrap{Writers: []storage.WriterLease{{Fence: writer, ExpiresAt: now.Add(time.Hour)}, {Fence: other, ExpiresAt: now.Add(time.Hour)}}}); err != nil {
		t.Fatal(err)
	}
	ref := storage.PackRef{ID: "pack-lifecycle", Generation: "generation-1"}
	info := storage.PackInfo{Version: 1, Ref: ref, State: "Uploading", Class: storage.Standard, Size: 100,
		StateRevision: 1, Writer: writer, ProtectedUntil: now.Add(time.Hour)}
	stale := writer
	stale.Epoch++
	if err := m.RegisterUploadingPack(ctx, info, stale); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("stale registration: %v", err)
	}
	if err := m.RegisterUploadingPack(ctx, info, writer); err != nil {
		t.Fatal(err)
	}
	if err := m.RegisterUploadingPack(ctx, info, writer); err != nil {
		t.Fatal("exact retry:", err)
	}
	key := storage.ChunkKey{Domain: "default", Hash: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	chunk := storage.Chunk{Version: 1, Key: key, RawBytes: 10, Location: storage.ChunkLocation{Pack: ref,
		Range: storage.ByteRange{Offset: 10, Length: 10}, Codec: "raw"}, LocationRevision: 1}
	if _, _, err := m.PutChunkIfAbsent(ctx, chunk, writer); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("Uploading pack admitted: %v", err)
	}
	if _, err := m.MarkPackWarm(ctx, ref, 1, stale); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("stale Warm publication: %v", err)
	}
	if _, err := m.MarkPackWarm(ctx, ref, 1, other); !errors.Is(err, storage.ErrFenced) {
		t.Fatalf("different live writer published pack: %v", err)
	}
	warm, err := m.MarkPackWarm(ctx, ref, 1, writer)
	if err != nil || warm.State != "Warm" || warm.StateRevision != 2 {
		t.Fatalf("Warm publication: %+v %v", warm, err)
	}
	if _, err := m.MarkPackWarm(ctx, ref, 1, writer); err != nil {
		t.Fatal("Warm retry:", err)
	}
	if _, inserted, err := m.PutChunkIfAbsent(ctx, chunk, writer); err != nil || !inserted {
		t.Fatalf("Warm canonical admission: %t %v", inserted, err)
	}
}
