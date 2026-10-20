package storage_test

import (
	"errors"
	"math"
	"strings"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
)

func TestChunkIdentity(t *testing.T) {
	hash := strings.Repeat("a", 64)
	key := s.ChunkKey{Domain: "default", Hash: hash}
	pk, err := key.PartitionKey()
	if err != nil || pk != "default#"+hash {
		t.Fatal(pk, err)
	}
	for _, key := range []s.ChunkKey{{Domain: "", Hash: hash}, {Domain: "a#b", Hash: hash}, {Domain: "a/b", Hash: hash}, {Domain: strings.Repeat("a", 65), Hash: hash}, {Domain: "default", Hash: strings.Repeat("A", 64)}, {Domain: "default", Hash: hash[:63]}} {
		if _, err := key.PartitionKey(); !errors.Is(err, s.ErrInvalid) {
			t.Fatalf("bad identity accepted: %+v %v", key, err)
		}
	}
}
func TestBucketIncarnations(t *testing.T) {
	a, err := s.NewBucketID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.NewBucketID()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 32 || len(b) != 32 || a == b || strings.ToLower(a) != a {
		t.Fatal("bucket ID format or incarnation reuse", a, b)
	}
}
func TestRangeValidation(t *testing.T) {
	if err := (s.ByteRange{Offset: 1, Length: 3}).Validate(4); err != nil {
		t.Fatal(err)
	}
	for _, r := range []s.ByteRange{{Offset: -1, Length: 1}, {Offset: 0, Length: -1}, {Offset: 0, Length: 0}, {Offset: math.MaxInt64, Length: 1}, {Offset: math.MaxInt64 - 1, Length: 3}} {
		if err := r.Validate(math.MaxInt64); !errors.Is(err, s.ErrInvalid) {
			t.Fatal("invalid/overflow range", r, err)
		}
	}
}
func TestVersionAndDomainValidation(t *testing.T) {
	page := s.ManifestPage{Version: s.ModelVersion, ManifestID: "manifest", Domain: "default", Chunks: []s.ChunkRef{{Key: s.ChunkKey{Domain: "other", Hash: strings.Repeat("a", 64)}, RawBytes: 4}}}
	if err := page.Validate(); !errors.Is(err, s.ErrInvalid) {
		t.Fatal("cross-domain manifest accepted", err)
	}
	page.Chunks[0].Key.Domain = "default"
	page.Version++
	if err := page.Validate(); !errors.Is(err, s.ErrInvalid) {
		t.Fatal("unknown model version accepted", err)
	}
}
