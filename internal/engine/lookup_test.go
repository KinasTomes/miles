package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

type lookupStore struct {
	storage.MetadataStore
	rows       map[storage.ChunkKey]storage.Chunk
	err        error
	consistent bool
}

func (s *lookupStore) GetChunks(_ context.Context, keys []storage.ChunkKey, consistent bool) (map[storage.ChunkKey]storage.Chunk, error) {
	s.consistent = consistent
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[storage.ChunkKey]storage.Chunk)
	for _, key := range keys {
		if row, ok := s.rows[key]; ok {
			out[key] = row
		}
	}
	return out, nil
}

func lookupRow(t *testing.T, domain, value string) storage.Chunk {
	t.Helper()
	key, err := HashChunk(domain, []byte(value))
	if err != nil {
		t.Fatal(err)
	}
	return storage.Chunk{
		Version: storage.ModelVersion, Key: key, RawBytes: int64(len(value)), LocationRevision: 1,
		Location: storage.ChunkLocation{Pack: storage.PackRef{ID: "p", Generation: "g"}, Range: storage.ByteRange{Length: int64(len(value))}, Codec: "raw"},
	}
}

func TestCanonicalLookupTreatsHintsAsCandidates(t *testing.T) {
	winner := lookupRow(t, "default", "same bytes")
	stale := winner
	stale.Location.Pack.Generation = "old"
	otherDomain := lookupRow(t, "tenant-b", "same bytes")
	if winner.Key.Hash != otherDomain.Key.Hash || winner.Key == otherDomain.Key {
		t.Fatal("chunk identity did not isolate domains")
	}
	bloom, _ := NewBloom(64)
	lru, _ := NewCandidateLRU(1)
	if err := lru.Put(stale); err != nil {
		t.Fatal(err)
	}
	store := &lookupStore{rows: map[storage.ChunkKey]storage.Chunk{winner.Key: winner}}
	index := LookupIndex{Store: store, Bloom: bloom, LRU: lru}

	// The filter starts negative while the store already contains the row.
	got, found, err := index.Canonical(t.Context(), winner.Key)
	if err != nil || !found || got != winner || !store.consistent {
		t.Fatalf("canonical result: %+v found=%v err=%v consistent=%v", got, found, err, store.consistent)
	}
	if cached, ok := lru.Get(winner.Key); !ok || cached != winner {
		t.Fatal("stale candidate was not refreshed")
	}
	if err := bloom.Add(otherDomain.Key); err != nil {
		t.Fatal(err)
	}
	_, found, err = index.Canonical(t.Context(), otherDomain.Key)
	if err != nil || found {
		t.Fatalf("Bloom positive became false existence: found=%v err=%v", found, err)
	}
	store.err = storage.ErrUnavailable
	_, found, err = index.Canonical(t.Context(), winner.Key)
	if found || !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("lookup failure became absence: found=%v err=%v", found, err)
	}
}

func TestBloomAndCandidateLRUBounds(t *testing.T) {
	bloom, err := NewBloom(64)
	if err != nil {
		t.Fatal(err)
	}
	first := lookupRow(t, "default", "first")
	if err := bloom.Add(first.Key); err != nil {
		t.Fatal(err)
	}
	if maybe, err := bloom.MaybeContains(first.Key); err != nil || !maybe {
		t.Fatalf("inserted key absent: %v %v", maybe, err)
	}
	inserted := map[storage.ChunkKey]bool{first.Key: true}
	for i := 0; i < 80; i++ {
		key := lookupRow(t, "default", fmt.Sprintf("inserted-%d", i)).Key
		inserted[key] = true
		if err := bloom.Add(key); err != nil {
			t.Fatal(err)
		}
	}
	// A deliberately tiny filter must admit a false positive without changing
	// the authoritative lookup result tested above.
	collision := false
	for i := 0; i < 10000; i++ {
		key := lookupRow(t, "default", fmt.Sprint(i)).Key
		if inserted[key] {
			continue
		}
		if maybe, _ := bloom.MaybeContains(key); maybe {
			collision = true
			break
		}
	}
	if !collision {
		t.Fatal("test did not encounter a Bloom false positive")
	}
	lru, _ := NewCandidateLRU(1)
	second := lookupRow(t, "default", "second")
	if err := lru.Put(first); err != nil {
		t.Fatal(err)
	}
	if err := lru.Put(second); err != nil {
		t.Fatal(err)
	}
	if _, ok := lru.Get(first.Key); ok {
		t.Fatal("LRU exceeded capacity")
	}
	if got, ok := lru.Get(second.Key); !ok || got != second {
		t.Fatal("most recent candidate missing")
	}
}
