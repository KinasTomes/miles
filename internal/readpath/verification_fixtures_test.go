package readpath_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"hash/crc32"
	"os"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

// Test-only metadata, not a new shared location or authoritative CRC API.
type verificationMetadata struct {
	Key      storage.ChunkKey
	Pack     storage.PackRef
	Range    storage.ByteRange
	Codec    string
	RawBytes int64
	CRC32C   uint32
}
type fixtureLimits struct{ StoredBytes, RawBytes int64 }
type verificationCategory string

const (
	verified            verificationCategory = "verified"
	storedLengthFailure verificationCategory = "stored_length"
	storedCRCFailure    verificationCategory = "stored_crc32c"
	decodeFailure       verificationCategory = "decode"
	rawLengthFailure    verificationCategory = "raw_length"
	rawHashFailure      verificationCategory = "raw_sha256"
	codecFailure        verificationCategory = "unsupported_codec"
	resourceFailure     verificationCategory = "resource_limit"
)

type verificationVector struct {
	name        string
	stored, raw []byte // raw is fixture oracle content, never passed to verifier
	metadata    verificationMetadata
	limits      fixtureLimits // small test budgets, not deployment defaults
	want        verificationCategory
}

// A future production adapter must classify actual errors and enforce allocation
// limits. No implementation of this function is supplied in Phase 3A.0.
type verifierAdapter func(context.Context, verificationMetadata, []byte, fixtureLimits) ([]byte, verificationCategory, error)

func baseVector(name, codec string, stored, raw []byte) verificationVector {
	sum := sha256.Sum256(raw)
	return verificationVector{name: name, stored: bytes.Clone(stored), raw: bytes.Clone(raw),
		metadata: verificationMetadata{Key: storage.ChunkKey{Domain: "default", Hash: hex.EncodeToString(sum[:])}, Pack: storage.PackRef{ID: "fixture-pack", Generation: "fixture-gen"}, Range: storage.ByteRange{Offset: 7, Length: int64(len(stored))}, Codec: codec, RawBytes: int64(len(raw)), CRC32C: crc32.Checksum(stored, crc32.MakeTable(crc32.Castagnoli))},
		limits:   fixtureLimits{StoredBytes: 8192, RawBytes: 8192}, want: verified}
}
func cloneVector(v verificationVector, name string) verificationVector {
	v.name = name
	v.stored = bytes.Clone(v.stored)
	v.raw = bytes.Clone(v.raw)
	return v
}
func rebindStored(v *verificationVector) {
	v.metadata.Range.Length = int64(len(v.stored))
	v.metadata.CRC32C = crc32.Checksum(v.stored, crc32.MakeTable(crc32.Castagnoli))
}

// Reuse the hand-checkable raw and one-block RLE zstd golden; do not regenerate
// writer output or add a strict zstd parser here.
func goldenChunks(t *testing.T) (verificationVector, verificationVector) {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/format/pack.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Packs []struct {
			Name   string
			Chunks []struct {
				Codec     string
				RawHex    string `json:"raw_hex"`
				StoredHex string `json:"stored_hex"`
				RawRepeat *struct {
					Byte  byte
					Count int
				} `json:"raw_repeat"`
			}
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, p := range fixture.Packs {
		if p.Name != "small" {
			continue
		}
		if len(p.Chunks) != 2 {
			t.Fatal("small golden shape")
		}
		vectors := make([]verificationVector, 0, 2)
		for _, c := range p.Chunks {
			raw, err := hex.DecodeString(c.RawHex)
			if err != nil {
				t.Fatal(err)
			}
			if c.RawRepeat != nil {
				if c.RawRepeat.Count <= 0 || c.RawRepeat.Count > 8192 {
					t.Fatal("fixture allocation limit")
				}
				raw = bytes.Repeat([]byte{c.RawRepeat.Byte}, c.RawRepeat.Count)
			}
			stored := raw
			if c.StoredHex != "" {
				stored, err = hex.DecodeString(c.StoredHex)
				if err != nil {
					t.Fatal(err)
				}
			}
			vectors = append(vectors, baseVector("valid "+c.Codec, c.Codec, stored, raw))
		}
		return vectors[0], vectors[1]
	}
	t.Fatal("missing small golden")
	return verificationVector{}, verificationVector{}
}

func verificationVectors(t *testing.T) []verificationVector {
	t.Helper()
	raw, z := goldenChunks(t)
	zero := baseVector("valid zero CRC32C", "raw", zeroCRCBytes(t), zeroCRCBytes(t))
	out := []verificationVector{raw, z, zero}
	add := func(v verificationVector) { out = append(out, v) }
	v := cloneVector(raw, "modified stored bytes")
	v.stored[0] ^= 1
	v.want = storedCRCFailure
	add(v)
	v = cloneVector(raw, "wrong stored length")
	v.metadata.Range.Length++
	v.want = storedLengthFailure
	add(v)
	v = cloneVector(raw, "wrong CRC32C")
	v.metadata.CRC32C ^= 1
	v.want = storedCRCFailure
	add(v)
	v = cloneVector(raw, "wrong raw length")
	v.metadata.RawBytes++
	v.want = rawLengthFailure
	add(v)
	v = cloneVector(raw, "wrong SHA256")
	v.metadata.Key.Hash = hex.EncodeToString(make([]byte, 32))
	v.want = rawHashFailure
	add(v)
	v = cloneVector(raw, "unsupported codec")
	v.metadata.Codec = "lz4"
	v.want = codecFailure
	add(v)
	v = cloneVector(z, "truncated zstd")
	v.stored = v.stored[:len(v.stored)-1]
	rebindStored(&v)
	v.want = decodeFailure
	add(v)
	v = cloneVector(z, "zstd trailing byte")
	v.stored = append(v.stored, 0)
	rebindStored(&v)
	v.want = decodeFailure
	add(v)
	v = cloneVector(z, "multiple zstd frames")
	v.stored = append(v.stored, z.stored...)
	rebindStored(&v)
	v.want = decodeFailure
	add(v)
	v = cloneVector(z, "decode output exceeds expected raw length")
	v.metadata.RawBytes = 1
	v.want = resourceFailure
	add(v)
	v = cloneVector(raw, "stored resource limit")
	v.limits.StoredBytes = int64(len(v.stored) - 1)
	v.want = resourceFailure
	add(v)
	v = cloneVector(z, "raw resource limit")
	v.limits.RawBytes = int64(len(v.raw) - 1)
	v.want = resourceFailure
	add(v)
	// Order vectors: earlier integrity failures must win over a later bad hash or
	// malformed frame. Resource/metadata prechecks may precede byte verification.
	v = cloneVector(z, "CRC precedes decode")
	v.stored = v.stored[:len(v.stored)-1]
	v.metadata.Range.Length = int64(len(v.stored))
	v.want = storedCRCFailure
	add(v)
	v = cloneVector(raw, "length precedes CRC")
	v.stored = v.stored[:len(v.stored)-1]
	v.metadata.CRC32C ^= 1
	v.want = storedLengthFailure
	add(v)
	v = cloneVector(raw, "raw length precedes hash")
	v.metadata.RawBytes++
	v.metadata.Key.Hash = hex.EncodeToString(make([]byte, 32))
	v.want = rawLengthFailure
	add(v)

	// Fixed framing probes: none is a production encoder/decoder. Keep raw as
	// the small expected canonical content, even when an untrusted frame claims
	// a larger output. Header-bound probes must never be decoded by self-checks.
	v = cloneVector(z, "unsupported dictionary ID")
	v.stored = append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x61, 7}, z.stored[5:]...)
	rebindStored(&v)
	v.want = decodeFailure
	add(v)
	v = cloneVector(z, "window exceeds contract bound")
	// Non-single segment; WD=0x78 gives 2^(10+15)=32 MiB; FCS remains 4096.
	v.stored = append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x40, 0x78}, z.stored[5:]...)
	rebindStored(&v)
	v.want = resourceFailure
	add(v)
	v = cloneVector(z, "content size exceeds contract bound")
	v.stored = oversizedContentFrame()
	rebindStored(&v)
	v.want = resourceFailure
	add(v)
	v = cloneVector(z, "no FCS output exceeds expected raw length")
	// Non-single segment, no FCS/dictionary, 4 KiB window, original RLE block.
	v.stored = append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0x10}, z.stored[7:]...)
	v.metadata.RawBytes = 1
	rebindStored(&v)
	v.want = resourceFailure
	add(v)
	v = cloneVector(z, "corrupt zstd with valid stored CRC")
	// Reserved block type 3; retain the well-formed frame header.
	v.stored[7] |= 6
	rebindStored(&v)
	v.want = decodeFailure
	add(v)
	return out
}

// A syntactically complete RLE frame with FCS=16 MiB+1, encoded in 525 bytes.
// No 16 MiB payload is allocated. Self-checks inspect header/block-size algebra
// only; they must not decode this header-rejection vector.
func oversizedContentFrame() []byte {
	const size = uint32(16<<20) + 1
	out := binary.LittleEndian.AppendUint32([]byte{0x28, 0xb5, 0x2f, 0xfd, 0xa0}, size)
	for remaining := size; remaining > 0; {
		n := min(remaining, uint32(128<<10))
		remaining -= n
		header := n<<3 | 2 // RLE block, one stored byte
		if remaining == 0 {
			header |= 1
		}
		out = append(out, byte(header), byte(header>>8), byte(header>>16), 0)
	}
	return out
}

// CRC32C is affine in a fixed four-byte message. Solve its 32-bit linear map
// for checksum 0, using checksum(zeros) as affine offset. All work is bounded.
func zeroCRCBytes(t *testing.T) []byte {
	t.Helper()
	table := crc32.MakeTable(crc32.Castagnoli)
	base := crc32.Checksum(make([]byte, 4), table)
	var basis, combination [32]uint32
	for i := 0; i < 32; i++ {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(1)<<i)
		v := crc32.Checksum(b[:], table) ^ base
		mask := uint32(1) << i
		for bit := 31; bit >= 0; bit-- {
			if v&(uint32(1)<<bit) == 0 {
				continue
			}
			if basis[bit] == 0 {
				basis[bit] = v
				combination[bit] = mask
				break
			}
			v ^= basis[bit]
			mask ^= combination[bit]
		}
	}
	target, solution := base, uint32(0)
	for bit := 31; bit >= 0; bit-- {
		if target&(uint32(1)<<bit) == 0 {
			continue
		}
		if basis[bit] == 0 {
			t.Fatal("singular CRC map")
		}
		target ^= basis[bit]
		solution ^= combination[bit]
	}
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, solution)
	if target != 0 || crc32.Checksum(out, table) != 0 || bitwiseCRC32C(out) != 0 {
		t.Fatal("zero CRC construction failed")
	}
	return out
}

// Independent bitwise Castagnoli oracle (reflected polynomial, standard init/xor).
func bitwiseCRC32C(data []byte) uint32 {
	crc := ^uint32(0)
	for _, b := range data {
		crc ^= uint32(b)
		for i := 0; i < 8; i++ {
			if crc&1 != 0 {
				crc = (crc >> 1) ^ 0x82f63b78
			} else {
				crc >>= 1
			}
		}
	}
	return ^crc
}
