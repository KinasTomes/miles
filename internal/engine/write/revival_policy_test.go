package write

import (
	"bytes"
	"context"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
)

type observedReuse struct{ calls int }

func (g *observedReuse) Pin(context.Context, storage.Chunk) (bool, error) {
	g.calls++
	return true, nil
}

type noRestoreSource struct {
	storage.PackStore
	reads, restores int
}

func (p *noRestoreSource) RangeGet(context.Context, storage.PackRef, storage.ByteRange) (io.ReadCloser, error) {
	p.reads++
	return nil, storage.ErrUnavailable
}
func (p *noRestoreSource) Restore(context.Context, storage.PackRef, storage.RestoreRequest) error {
	p.restores++
	return storage.ErrUnavailable
}

// Both PUT and UploadPart prepare through Pipeline; publishing their logical
// heads remains the respective fenced transaction, not this body-revival test.
func TestWritePipelineRevivesArchiveAndEveryRestoredFromBody(t *testing.T) {
	for _, operation := range []string{"PUT", "UploadPart"} {
		for _, state := range []string{"Cold", "Restored", "Restoring"} {
			t.Run(operation+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				pub, metadata, physical := publisherFixture(t, true)
				old, data, sidecar, entries := onePack(t)
				rows, err := pub.Publish(ctx, old, data, sidecar, entries, nil)
				if err != nil {
					t.Fatal(err)
				}
				key := entries[0].Key
				before := rows[key]
				infos, _ := metadata.GetPacks(ctx, []storage.PackRef{old})
				oldInfo := infos[old]
				oldInfo.State = state
				oldInfo.Class = storage.Glacier
				oldInfo.StateRevision++
				oldInfo.RestoredUntil = pub.ProtectedUntil.Add(365 * 24 * time.Hour)
				metadata.SetPack(oldInfo)
				guard := &observedReuse{}
				pub.Guard = guard
				noSource := &noRestoreSource{PackStore: physical}
				pub.Packs = noSource
				body := bytes.Repeat([]byte("hello"), 4000)
				var refs []storage.ChunkRef
				result, err := (Pipeline{Lookup: metadata, Publisher: pub, Chunking: engine.ChunkingParams{Min: 262144, Avg: 1048576, Max: 4194304}, Compression: engine.CompressionPolicy{Enabled: false, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}).Run(ctx, bytes.NewReader(body), key.Domain, func(r storage.ChunkRef) error { refs = append(refs, r); return nil })
				if err != nil || result.PackCount != 1 || len(refs) != 1 || refs[0].Key != key {
					t.Fatal("body not revived", result, refs, err)
				}
				current, _ := metadata.GetChunks(ctx, []storage.ChunkKey{key}, true)
				after := current[key]
				if after.Key != before.Key || after.Location.Pack == old || after.LocationRevision != before.LocationRevision+1 {
					t.Fatal("identity/location", before, after)
				}
				infos, _ = metadata.GetPacks(ctx, []storage.PackRef{after.Location.Pack, old})
				fresh := infos[after.Location.Pack]
				if fresh.State != "Warm" || fresh.Class != storage.Standard || !fresh.ProtectedUntil.Equal(pub.ProtectedUntil) || !storage.SamePackInfo(infos[old], oldInfo) {
					t.Fatal("pack state/protection changed", infos)
				}
				if noSource.reads != 0 || noSource.restores != 0 || guard.calls != 0 {
					t.Fatal("temporary archive reused", noSource.reads, noSource.restores, guard.calls)
				}
				raw, e := pack.ReadCanonicalChunk(ctx, physical, after)
				if e != nil || !bytes.Equal(raw, body) {
					t.Fatal("revived bytes", e)
				}
			})
		}
	}
}

func TestWritePublisherIRCanAttemptExactReuseWithoutRestore(t *testing.T) {
	pub, metadata, physical := publisherFixture(t, true)
	ctx := context.Background()
	ref, data, sidecar, entries := onePack(t)
	rows, err := pub.Publish(ctx, ref, data, sidecar, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	infos, _ := metadata.GetPacks(ctx, []storage.PackRef{ref})
	p := infos[ref]
	p.State = "Cold"
	p.Class = storage.GlacierIR
	p.StateRevision++
	metadata.SetPack(p)
	guard := &observedReuse{}
	pub.Guard = guard
	source := &noRestoreSource{PackStore: physical}
	pub.Packs = source
	if reused, e := pub.Reuse(ctx, rows[entries[0].Key]); e != nil || !reused || guard.calls != 1 || source.restores != 0 {
		t.Fatal(reused, e, guard.calls, source.restores)
	}
}

func TestWritePublisherRequiresDurableProtectionBeforeReuse(t *testing.T) {
	for _, delta := range []time.Duration{-1, 0, 1} {
		t.Run(delta.String(), func(t *testing.T) {
			pub, metadata, _ := publisherFixture(t, true)
			ctx := context.Background()
			ref, data, sidecar, entries := onePack(t)
			rows, e := pub.Publish(ctx, ref, data, sidecar, entries, nil)
			if e != nil {
				t.Fatal(e)
			}
			infos, _ := metadata.GetPacks(ctx, []storage.PackRef{ref})
			p := infos[ref]
			p.ProtectedUntil = pub.ProtectedUntil.Add(delta)
			metadata.SetPack(p)
			guard := &observedReuse{}
			pub.Guard = guard
			reuse, e := pub.Reuse(ctx, rows[entries[0].Key])
			if e != nil || reuse != (delta >= 0) {
				t.Fatal(reuse, e)
			}
			if delta < 0 && guard.calls != 0 {
				t.Fatal("insufficient protection reached pin")
			}
			infos, _ = metadata.GetPacks(ctx, []storage.PackRef{ref})
			if !infos[ref].ProtectedUntil.Equal(p.ProtectedUntil) {
				t.Fatal("invented protection persistence")
			}
		})
	}
}
