// Package manifest implements the frozen v1 object/part reference page format.
package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"

	s "github.com/KinasTomes/miles/internal/storage"
)

const MaxPageBytes = 384 << 10
const MaxPages = 1024
const ObjectPage byte = 1
const PartPage byte = 2

var validID = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// Encode binds the body to its kind, owner and page number. Empty objects have
// zero pages; an empty reference page is not a valid frozen-format record.
func Encode(kind byte, p s.ManifestPage) ([]byte, error) {
	if (kind != ObjectPage && kind != PartPage) || !validID.MatchString(p.ManifestID) || p.Number >= MaxPages || len(p.Chunks) == 0 || p.Validate() != nil {
		return nil, s.ErrInvalid
	}
	if len(p.Chunks) > (MaxPageBytes-67-len(p.ManifestID)-len(p.Domain))/36 {
		return nil, s.ErrInvalid
	}
	payload := append([]byte{byte(len(p.Domain))}, p.Domain...)
	payload = binary.LittleEndian.AppendUint64(payload, uint64(p.Chunks[0].LogicalOffset))
	payload = binary.LittleEndian.AppendUint32(payload, uint32(len(p.Chunks)))
	offset := p.Chunks[0].LogicalOffset
	for _, r := range p.Chunks {
		if r.LogicalOffset != offset || r.RawBytes > int64(^uint32(0)) || offset > s.MaxObjectBytes-r.RawBytes {
			return nil, s.ErrInvalid
		}
		hash, err := hex.DecodeString(r.Key.Hash)
		if err != nil {
			return nil, s.ErrInvalid
		}
		payload = append(payload, hash...)
		payload = binary.LittleEndian.AppendUint32(payload, uint32(r.RawBytes))
		offset += r.RawBytes
	}
	b := binary.LittleEndian.AppendUint32([]byte("MILESPAG"), s.ModelVersion)
	b = append(b, kind, byte(len(p.ManifestID)))
	b = append(b, p.ManifestID...)
	b = binary.LittleEndian.AppendUint32(b, p.Number)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
	b = append(b, payload...)
	sum := sha256.Sum256(b)
	return append(b, sum[:]...), nil
}

// Decode verifies the checksum and expected item key before allocating refs.
func Decode(b []byte, kind byte, owner string, number uint32) (s.ManifestPage, error) {
	var p s.ManifestPage
	invalid := func() (s.ManifestPage, error) { return s.ManifestPage{}, fmt.Errorf("%w: manifest page", s.ErrInvalid) }
	if len(b) > MaxPageBytes || len(b) < 55 || (kind != ObjectPage && kind != PartPage) || number >= MaxPages {
		return invalid()
	}
	body := b[:len(b)-32]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], b[len(body):]) {
		return invalid()
	}
	if string(body[:8]) != "MILESPAG" || binary.LittleEndian.Uint32(body[8:12]) != s.ModelVersion || body[12] != kind {
		return invalid()
	}
	n := int(body[13])
	if len(body) < 22+n || string(body[14:14+n]) != owner || !validID.MatchString(owner) {
		return invalid()
	}
	if binary.LittleEndian.Uint32(body[14+n:]) != number || uint64(binary.LittleEndian.Uint32(body[18+n:])) != uint64(len(body)-22-n) {
		return invalid()
	}
	payload := body[22+n:]
	if len(payload) < 1 {
		return invalid()
	}
	d := int(payload[0])
	if len(payload) < 13+d {
		return invalid()
	}
	domain := string(payload[1 : 1+d])
	if !validID.MatchString(domain) {
		return invalid()
	}
	offset := binary.LittleEndian.Uint64(payload[1+d:])
	count := binary.LittleEndian.Uint32(payload[9+d:])
	refs := payload[13+d:]
	if count == 0 || uint64(count)*36 != uint64(len(refs)) || offset > uint64(s.MaxObjectBytes) {
		return invalid()
	}
	p = s.ManifestPage{Version: s.ModelVersion, ManifestID: owner, Domain: domain, Number: number, Chunks: make([]s.ChunkRef, 0, int(count))}
	for len(refs) > 0 {
		raw := int64(binary.LittleEndian.Uint32(refs[32:36]))
		if raw == 0 || offset > uint64(s.MaxObjectBytes-raw) {
			return invalid()
		}
		p.Chunks = append(p.Chunks, s.ChunkRef{Key: s.ChunkKey{Domain: domain, Hash: hex.EncodeToString(refs[:32])}, RawBytes: raw, LogicalOffset: int64(offset)})
		offset += uint64(raw)
		refs = refs[36:]
	}
	return p, nil
}
