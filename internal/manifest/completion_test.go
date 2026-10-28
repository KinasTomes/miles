package manifest

import (
	"bytes"
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
)

func TestProductionCompletionFrozenGoldens(t *testing.T) {
	f := load(t)
	for _, c := range f.Streams {
		t.Run(c.Name, func(t *testing.T) {
			pages, _, p := c.build(t)
			var raw []byte
			for n, b := range pages {
				part, err := DecodeCompletionPage(b, p.Kind, p.Owner, uint32(n))
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, part...)
			}
			var produced [][]byte
			d, err := WriteCompletionStream(p.Kind, p.Owner, bytes.NewReader(raw), func(_ uint32, b []byte) error { produced = append(produced, b); return nil })
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(bytes.Join(pages, nil), bytes.Join(produced, nil)) || d.SHA256 != p.Digest || d.Length != int64(p.Length) {
				t.Fatal("frozen encoding changed")
			}
			if err := VerifyCompletionStream(d, func(n uint32) ([]byte, error) { return produced[n], nil }, nil); err != nil {
				t.Fatal(err)
			}
			if p.Kind == CompletionSnapshot {
				parts, err := DecodeSnapshot(raw)
				if err != nil {
					t.Fatal(err)
				}
				b, err := EncodeSnapshot(parts)
				if err != nil || !bytes.Equal(raw, b) {
					t.Fatal(err)
				}
			}
			produced[0][len(produced[0])-1] ^= 1
			if err := VerifyCompletionStream(d, func(n uint32) ([]byte, error) { return produced[n], nil }, nil); err == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}
func TestCompletionOwnerAndBounds(t *testing.T) {
	if _, err := EncodeCompletionPage(3, "wrong", 0, []byte{1}); err == nil {
		t.Fatal("invalid owner")
	}
	if _, err := EncodeCompletionPage(4, "11111111111111111111111111111111", 0, make([]byte, MaxPageBytes)); err == nil {
		t.Fatal("oversize page")
	}
	if err := VerifyCompletionStream(s.StreamDescriptor{Kind: 3, Owner: "11111111111111111111111111111111", Length: 1, Pages: 1025}, nil, nil); err == nil {
		t.Fatal("oversize stream")
	}
}
