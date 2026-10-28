package manifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"io"

	s "github.com/KinasTomes/miles/internal/storage"
)

const CompletionInput byte = 3
const CompletionSnapshot byte = 4

func validCompletion(kind byte, owner string) bool {
	if kind != CompletionInput && kind != CompletionSnapshot || len(owner) != 32 {
		return false
	}
	_, err := hex.DecodeString(owner)
	return err == nil && owner == string(bytes.ToLower([]byte(owner)))
}

// Completion pages use the frozen common envelope, but byte payloads rather
// than domain/ref records. The returned bytes are owned by the caller.
func EncodeCompletionPage(kind byte, owner string, number uint32, payload []byte) ([]byte, error) {
	if !validCompletion(kind, owner) || number >= MaxPages || len(payload) == 0 || len(payload) > MaxPageBytes-54-len(owner) {
		return nil, s.ErrInvalid
	}
	b := append([]byte("MILESPAG"), 1, 0, 0, 0, kind, byte(len(owner)))
	b = append(b, owner...)
	b = binary.LittleEndian.AppendUint32(b, number)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(payload)))
	b = append(b, payload...)
	sum := sha256.Sum256(b)
	return append(b, sum[:]...), nil
}
func DecodeCompletionPage(body []byte, kind byte, owner string, number uint32) ([]byte, error) {
	if !validCompletion(kind, owner) || number >= MaxPages || len(body) < 55+len(owner) || len(body) > MaxPageBytes {
		return nil, s.ErrInvalid
	}
	n := len(body) - 32
	sum := sha256.Sum256(body[:n])
	if !bytes.Equal(sum[:], body[n:]) || string(body[:8]) != "MILESPAG" || binary.LittleEndian.Uint32(body[8:12]) != 1 || body[12] != kind || int(body[13]) != len(owner) || string(body[14:14+len(owner)]) != owner {
		return nil, s.ErrInvalid
	}
	off := 14 + len(owner)
	if binary.LittleEndian.Uint32(body[off:]) != number || int(binary.LittleEndian.Uint32(body[off+4:])) != n-off-8 {
		return nil, s.ErrInvalid
	}
	return bytes.Clone(body[off+8 : n]), nil
}

// WriteCompletionStream retains one page; descriptors hash raw stream bytes.
func WriteCompletionStream(kind byte, owner string, input io.Reader, emit func(uint32, []byte) error) (s.StreamDescriptor, error) {
	d := s.StreamDescriptor{Kind: kind, Owner: owner}
	if !validCompletion(kind, owner) || input == nil || emit == nil {
		return d, s.ErrInvalid
	}
	h := sha256.New()
	buffer := make([]byte, MaxPageBytes-54-len(owner))
	for {
		n, err := io.ReadFull(input, buffer)
		if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
			return d, err
		}
		if n > 0 {
			if d.Pages >= MaxPages {
				return d, s.ErrInvalid
			}
			body, e := EncodeCompletionPage(kind, owner, d.Pages, buffer[:n])
			if e != nil {
				return d, e
			}
			if e = emit(d.Pages, body); e != nil {
				return d, e
			}
			_, _ = h.Write(buffer[:n])
			d.Pages++
			d.Length += int64(n)
		}
		if err != nil {
			break
		}
	}
	if d.Length == 0 {
		return d, s.ErrInvalid
	}
	d.SHA256 = hex.EncodeToString(h.Sum(nil))
	return d, nil
}
func VerifyCompletionStream(d s.StreamDescriptor, read func(uint32) ([]byte, error), consume func([]byte) error) error {
	if !validCompletion(d.Kind, d.Owner) || d.Pages == 0 || d.Pages > MaxPages || d.Length <= 0 || len(d.SHA256) != 64 || read == nil {
		return s.ErrInvalid
	}
	capacity := int64(MaxPageBytes - 54 - len(d.Owner))
	if (d.Length+capacity-1)/capacity != int64(d.Pages) {
		return s.ErrInvalid
	}
	h := sha256.New()
	var length int64
	for i := uint32(0); i < d.Pages; i++ {
		body, err := read(i)
		if err != nil {
			return err
		}
		payload, err := DecodeCompletionPage(body, d.Kind, d.Owner, i)
		if err != nil {
			return err
		}
		if int64(len(payload)) != min(capacity, d.Length-length) {
			return s.ErrInvalid
		}
		_, _ = h.Write(payload)
		length += int64(len(payload))
		if consume != nil {
			if err := consume(payload); err != nil {
				return err
			}
		}
	}
	if length != d.Length || hex.EncodeToString(h.Sum(nil)) != d.SHA256 {
		return s.ErrInvalid
	}
	return nil
}

type SnapshotPart struct {
	Number                     uint32
	Revision                   uint64
	Size                       uint64
	Pages                      uint32
	ETag, ManifestID, Checksum string
}

func EncodeSnapshot(parts []SnapshotPart) ([]byte, error) {
	if len(parts) == 0 || len(parts) > 10000 {
		return nil, s.ErrInvalid
	}
	b := binary.LittleEndian.AppendUint32(nil, uint32(len(parts)))
	var previous uint32
	checksumLen := 0
	for _, p := range parts {
		tag, e := hex.DecodeString(p.ETag)
		sum, e2 := base64.StdEncoding.DecodeString(p.Checksum)
		if e != nil || e2 != nil || len(tag) != 16 || !validID.MatchString(p.ManifestID) || p.Number <= previous || p.Number > 10000 || p.Revision == 0 || p.Size > 5<<30 || p.Pages > MaxPages || (p.Size == 0) != (p.Pages == 0) {
			return nil, s.ErrInvalid
		}
		if checksumLen == 0 {
			checksumLen = len(sum)
		}
		if (len(sum) != 4 && len(sum) != 8 && len(sum) != 20 && len(sum) != 32) || len(sum) != checksumLen {
			return nil, s.ErrInvalid
		}
		previous = p.Number
		b = binary.LittleEndian.AppendUint32(b, p.Number)
		b = binary.LittleEndian.AppendUint64(b, p.Revision)
		b = binary.LittleEndian.AppendUint64(b, p.Size)
		b = binary.LittleEndian.AppendUint32(b, p.Pages)
		b = append(b, tag...)
		b = append(b, byte(len(p.ManifestID)))
		b = append(b, p.ManifestID...)
		b = append(b, byte(len(sum)))
		b = append(b, sum...)
	}
	return b, nil
}
func DecodeSnapshot(body []byte) ([]SnapshotPart, error) {
	if len(body) < 4 {
		return nil, s.ErrInvalid
	}
	n := binary.LittleEndian.Uint32(body)
	if n == 0 || n > 10000 {
		return nil, s.ErrInvalid
	}
	rest := body[4:]
	parts := make([]SnapshotPart, 0, n)
	for i := uint32(0); i < n; i++ {
		if len(rest) < 41 {
			return nil, s.ErrInvalid
		}
		p := SnapshotPart{Number: binary.LittleEndian.Uint32(rest), Revision: binary.LittleEndian.Uint64(rest[4:]), Size: binary.LittleEndian.Uint64(rest[12:]), Pages: binary.LittleEndian.Uint32(rest[20:]), ETag: hex.EncodeToString(rest[24:40])}
		l := int(rest[40])
		rest = rest[41:]
		if len(rest) < l+1 {
			return nil, s.ErrInvalid
		}
		p.ManifestID = string(rest[:l])
		c := int(rest[l])
		rest = rest[l+1:]
		if len(rest) < c {
			return nil, s.ErrInvalid
		}
		p.Checksum = base64.StdEncoding.EncodeToString(rest[:c])
		rest = rest[c:]
		parts = append(parts, p)
	}
	canonical, err := EncodeSnapshot(parts)
	if err != nil || !bytes.Equal(body, canonical) {
		return nil, s.ErrInvalid
	}
	return parts, nil
}
