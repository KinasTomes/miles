package manifest

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
)

func TestProductionReferencePages(t *testing.T) {
	for _, kind := range []byte{ObjectPage, PartPage} {
		refs := make([]ref, 22000)
		for i := range refs {
			refs[i].hash[0] = byte(i)
			refs[i].raw = 64
		}
		pages, _ := refPages(kind, "manifest-test", "default", refs)
		for n, b := range pages {
			p, err := Decode(b, kind, "manifest-test", uint32(n))
			if err != nil {
				t.Fatal(err)
			}
			got, err := Encode(kind, p)
			if err != nil || !bytes.Equal(got, b) {
				t.Fatalf("frozen bytes changed: %v", err)
			}
			p2, err := Decode(got, kind, p.ManifestID, p.Number)
			if err != nil || !reflect.DeepEqual(p, p2) {
				t.Fatal(err)
			}
		}
	}
}

func TestGreedyStreamingPagination(t *testing.T) {
	capacity, err := RefCapacity("stream", "default")
	if err != nil {
		t.Fatal(err)
	}
	count := capacity + 1
	i := 0
	var pages []s.ManifestPage
	err = Paginate(ObjectPage, "stream", "default", func() (s.ChunkRef, error) {
		if i == count {
			return s.ChunkRef{}, io.EOF
		}
		r := s.ChunkRef{Key: s.ChunkKey{Domain: "default", Hash: strings.Repeat("a", 64)}, RawBytes: 4, LogicalOffset: int64(i * 4)}
		i++
		return r, nil
	}, func(p s.ManifestPage) error { pages = append(pages, p); return nil })
	if err != nil || len(pages) != 2 || len(pages[0].Chunks) != capacity || len(pages[1].Chunks) != 1 || pages[1].Chunks[0].LogicalOffset != int64(capacity*4) {
		t.Fatalf("pagination: pages=%d err=%v", len(pages), err)
	}
	b, err := Encode(ObjectPage, pages[0])
	if err != nil || len(b) > MaxPageBytes {
		t.Fatal(err)
	}
	pages[0].Chunks = append(pages[0].Chunks, pages[1].Chunks[0])
	if _, err = Encode(ObjectPage, pages[0]); !errors.Is(err, s.ErrInvalid) {
		t.Fatalf("oversize accepted: %v", err)
	}
	emitted := false
	if err = Paginate(ObjectPage, "empty", "default", func() (s.ChunkRef, error) { return s.ChunkRef{}, io.EOF }, func(s.ManifestPage) error { emitted = true; return nil }); err != nil || emitted {
		t.Fatal("empty emitted a page", err)
	}
	want := errors.New("source failure")
	if err = Paginate(ObjectPage, "failed", "default", func() (s.ChunkRef, error) { return s.ChunkRef{}, want }, func(s.ManifestPage) error { return nil }); !errors.Is(err, want) {
		t.Fatal(err)
	}
}

func FuzzPageDecode(f *testing.F) {
	p := s.ManifestPage{Version: 1, ManifestID: "fuzz", Domain: "default", Chunks: []s.ChunkRef{{Key: s.ChunkKey{Domain: "default", Hash: strings.Repeat("a", 64)}, RawBytes: 4}}}
	b, err := Encode(ObjectPage, p)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(b)
	f.Add([]byte("MILESPAG"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		p, err := Decode(b, ObjectPage, "fuzz", 0)
		if err != nil {
			return
		}
		encoded, err := Encode(ObjectPage, p)
		if err != nil || !bytes.Equal(encoded, b) {
			t.Fatalf("accepted non-roundtrippable page: %v", err)
		}
	})
}
func TestCodecRejectsInvalidPages(t *testing.T) {
	p := s.ManifestPage{Version: 1, ManifestID: "m", Domain: "default", Chunks: []s.ChunkRef{{Key: s.ChunkKey{Domain: "default", Hash: strings.Repeat("a", 64)}, RawBytes: 4}}}
	b, err := Encode(ObjectPage, p)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(b); i++ {
		if _, err := Decode(b[:i], ObjectPage, "m", 0); err == nil {
			t.Fatalf("accepted truncation %d", i)
		}
	}
	b[len(b)-1] ^= 1
	if _, err := Decode(b, ObjectPage, "m", 0); err == nil {
		t.Fatal("accepted corrupt checksum")
	}
	p.Number = MaxPages
	if _, err := Encode(ObjectPage, p); err == nil {
		t.Fatal("accepted page limit")
	}
}
