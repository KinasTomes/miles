package write

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/policy"
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

func TestWritePublisherExtendsDurableProtectionBeforeReuse(t *testing.T) {
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
			if e != nil || !reuse {
				t.Fatal(reuse, e)
			}
			if guard.calls != 1 {
				t.Fatal("missing generation pin")
			}
			infos, _ = metadata.GetPacks(ctx, []storage.PackRef{ref})
			want := p.ProtectedUntil
			if delta < 0 {
				want = pub.ProtectedUntil
			}
			if !infos[ref].ProtectedUntil.Equal(want) {
				t.Fatal("atomic max protection", infos[ref].ProtectedUntil, want)
			}
		})
	}
}

func TestCopyChunkRevivesDestinationWithoutRelabelingSource(t *testing.T) {
	pub, metadata, physical := publisherFixture(t, true)
	ctx := t.Context()
	ref, data, index, entries := onePack(t)
	sourceRows, err := pub.Publish(ctx, ref, data, index, entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	source := sourceRows[entries[0].Key]
	raw, err := pack.ReadCanonicalChunk(ctx, physical, source)
	if err != nil {
		t.Fatal(err)
	}
	sourceRef := storage.ChunkRef{Key: source.Key, RawBytes: source.RawBytes, LogicalOffset: 10}
	calls := 0
	load := func(context.Context) ([]byte, error) { calls++; return raw, nil }
	if _, err := copySingleForTest(pub, ctx, sourceRef, "destination", func(context.Context) ([]byte, error) { return []byte("invalid"), nil }); err == nil {
		t.Fatal("accepted corrupted source")
	}
	dest, err := copySingleForTest(pub, ctx, sourceRef, "destination", load)
	if err != nil || calls != 1 || dest.Key.Domain != "destination" || dest.Key.Hash != source.Key.Hash || dest.LogicalOffset != sourceRef.LogicalOffset {
		t.Fatal(dest, calls, err)
	}
	rows, err := metadata.GetChunks(ctx, []storage.ChunkKey{source.Key, dest.Key}, true)
	if err != nil || rows[source.Key] != source || rows[dest.Key].Location.Pack == ref {
		t.Fatal(rows, err)
	}
	old := rows[dest.Key]
	infos, err := metadata.GetPacks(ctx, []storage.PackRef{old.Location.Pack})
	if err != nil {
		t.Fatal(err)
	}
	cold := infos[old.Location.Pack]
	cold.State, cold.Class = "Cold", storage.Glacier
	cold.StateRevision++
	metadata.SetPack(cold)
	if _, err := copySingleForTest(pub, ctx, sourceRef, "destination", load); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
	rows, err = metadata.GetChunks(ctx, []storage.ChunkKey{source.Key, dest.Key}, true)
	revived := rows[dest.Key]
	if err != nil || rows[source.Key] != source || revived.LocationRevision != old.LocationRevision+1 || revived.Location.Pack == old.Location.Pack {
		t.Fatal(rows, err)
	}
	if _, err := copySingleForTest(pub, ctx, sourceRef, "destination", load); err != nil || calls != 2 {
		t.Fatal("protected destination should skip source", calls, err)
	}
}

func (g testGuard) PinCopy(ctx context.Context, row storage.Chunk, required time.Time) (bool, error) {
	current, err := g.store.GetChunks(ctx, []storage.ChunkKey{row.Key}, true)
	if err != nil || current[row.Key] != row {
		return false, err
	}
	packs, err := g.store.GetPacks(ctx, []storage.PackRef{row.Location.Pack})
	if err != nil {
		return false, err
	}
	eligible, err := policy.CopyReuseCandidate(packs[row.Location.Pack], required)
	return g.allow && eligible, err
}

func TestCopyRestoredExpiry(t *testing.T) {
	for _, delta := range []time.Duration{-1, 0, time.Hour} {
		t.Run(delta.String(), func(t *testing.T) {
			pub, metadata, _ := publisherFixture(t, true)
			ref, data, index, entries := onePack(t)
			rows, err := pub.Publish(t.Context(), ref, data, index, entries, nil)
			if err != nil {
				t.Fatal(err)
			}
			key := entries[0].Key
			packs, err := metadata.GetPacks(t.Context(), []storage.PackRef{ref})
			if err != nil {
				t.Fatal(err)
			}
			p := packs[ref]
			now := pub.ProtectedUntil.Add(-time.Hour)
			clk, err := clock.NewVirtual(now, 16)
			if err != nil {
				t.Fatal(err)
			}
			tick := clk.Now().Instant
			deadline, err := tick.Add(time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			request := policy.Request{Now: tick, Deadline: deadline}
			evidence := policy.UTCSource{ID: "test", Provenance: "fixture", RestartID: "1", Verified: policy.Valid}
			bound := policy.DurationBound{Known: true}
			proof, err := policy.ValidateUTCSample(evidence, policy.UTCSample{
				SourceID: evidence.ID, Provenance: evidence.Provenance, RestartID: evidence.RestartID,
				Observed: tick, ValidThrough: deadline, Lower: now, Upper: now,
				Precision: bound, MeasurementError: bound, NodeSkew: bound, Drift: policy.DriftBound{Known: true}, Sync: policy.Valid,
			}, request)
			if err != nil {
				t.Fatal(err)
			}
			required := now.Add(3 * time.Minute)
			p.State, p.Class, p.RestoredUntil = "Restored", storage.Glacier, required.Add(delta)
			p.StateRevision++
			metadata.SetPack(p)
			if reused, err := pub.Reuse(t.Context(), rows[key]); reused || err != nil {
				t.Fatal(reused, err)
			}
			source := storage.ChunkRef{Key: storage.ChunkKey{Domain: "source", Hash: key.Hash}, RawBytes: rows[key].RawBytes}
			calls := 0
			var dest storage.ChunkRef
			_, err = pub.CopyChunks(t.Context(), key.Domain, copyTestCompression(), request, proof, copyTestIterator([]storage.ChunkRef{source}),
				func(context.Context, storage.ChunkRef) ([]byte, error) { calls++; return nil, storage.ErrCold },
				func(ref storage.ChunkRef) error { dest = ref; return nil })
			if delta < 0 {
				if !errors.Is(err, storage.ErrCold) || calls != 1 {
					t.Fatal(calls, err)
				}
			} else if err != nil || calls != 0 || dest.Key != key {
				t.Fatal(dest, calls, err)
			}
		})
	}
}

func copySingleForTest(pub *StorePublisher, ctx context.Context, source storage.ChunkRef, domain string,
	load func(context.Context) ([]byte, error)) (storage.ChunkRef, error) {
	var dest storage.ChunkRef
	_, err := pub.CopyChunks(ctx, domain, copyTestCompression(), policy.Request{}, policy.UTCProof{},
		copyTestIterator([]storage.ChunkRef{source}),
		func(ctx context.Context, _ storage.ChunkRef) ([]byte, error) { return load(ctx) },
		func(ref storage.ChunkRef) error { dest = ref; return nil })
	return dest, err
}

func copyTestCompression() engine.CompressionPolicy {
	return engine.CompressionPolicy{Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}
}

func copyTestIterator(refs []storage.ChunkRef) func(context.Context) (storage.ChunkRef, error) {
	return func(context.Context) (storage.ChunkRef, error) {
		if len(refs) == 0 {
			return storage.ChunkRef{}, io.EOF
		}
		ref := refs[0]
		refs = refs[1:]
		return ref, nil
	}
}

func TestCopyChunksPacksCompressedMissesAndKeepsManifestOrder(t *testing.T) {
	ctx := t.Context()
	pub, metadata, physical := publisherFixture(t, true)
	ref, data, index, entries := onePack(t)
	if _, err := pub.Publish(ctx, ref, data, index, entries, nil); err != nil {
		t.Fatal(err)
	}
	reused := entries[0].Key
	reused.Domain = "source"
	a, b := bytes.Repeat([]byte("a"), 64<<10), bytes.Repeat([]byte("b"), 64<<10)
	ka, _ := engine.HashChunk("source", a)
	kb, _ := engine.HashChunk("source", b)
	refs := []storage.ChunkRef{
		{Key: ka, RawBytes: int64(len(a))},
		{Key: reused, RawBytes: int64(entries[0].RawLength), LogicalOffset: int64(len(a))},
		{Key: kb, RawBytes: int64(len(b)), LogicalOffset: int64(len(a)) + int64(entries[0].RawLength)},
		{Key: ka, RawBytes: int64(len(a)), LogicalOffset: int64(len(a)+len(b)) + int64(entries[0].RawLength)},
	}
	loads := map[storage.ChunkKey]int{}
	var emitted []storage.ChunkRef
	result, err := pub.CopyChunks(ctx, "default", engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)},
		policy.Request{}, policy.UTCProof{}, copyTestIterator(refs),
		func(_ context.Context, source storage.ChunkRef) ([]byte, error) {
			loads[source.Key]++
			if source.Key == ka {
				return a, nil
			}
			if source.Key == kb {
				return b, nil
			}
			return nil, storage.ErrCold
		},
		func(dest storage.ChunkRef) error { emitted = append(emitted, dest); return nil })
	if err != nil || result.PackCount != 1 || len(emitted) != len(refs) || loads[ka] != 1 || loads[kb] != 1 || len(loads) != 2 {
		t.Fatal(result, emitted, loads, err)
	}
	for i, source := range refs {
		source.Key.Domain = "default"
		if emitted[i] != source {
			t.Fatal(i, emitted[i], source)
		}
	}
	first, second := emitted[0].Key, emitted[2].Key
	rows, err := metadata.GetChunks(ctx, []storage.ChunkKey{first, second}, true)
	if err != nil || rows[first].Location.Pack != rows[second].Location.Pack || rows[first].Location.Pack == ref {
		t.Fatal(rows, err)
	}
	sidecar, err := physical.ReadSidecar(ctx, rows[first].Location.Pack)
	if err != nil {
		t.Fatal(err)
	}
	packed, _, err := pack.ParseIndex(sidecar, rows[first].Location.Pack)
	if err != nil || len(packed) != 2 {
		t.Fatal(packed, err)
	}
	for _, entry := range packed {
		if entry.Codec != "zstd" || entry.StoredLen >= entry.RawLength {
			t.Fatal(entry)
		}
	}
}
