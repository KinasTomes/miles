package manifest

import (
	"bytes"
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
