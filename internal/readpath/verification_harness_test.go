package readpath_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"reflect"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestVerificationFixtureSelfChecks(t *testing.T) {
	vectors := verificationVectors(t)
	if err := validateFixtureInventory(vectors); err != nil {
		t.Fatal(err)
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(8192))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			if len(v.stored) > 8192 || len(v.raw) > 8192 {
				t.Fatal("oversized test fixture")
			}
			crc := bitwiseCRC32C(v.stored)
			if crc != crc32.Checksum(v.stored, crc32.MakeTable(crc32.Castagnoli)) {
				t.Fatal("CRC oracles disagree")
			}
			sum := sha256.Sum256(v.raw)
			switch v.name {
			case "valid raw", "valid zero CRC32C":
				if err := validatePositiveFixtureCodec(v); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(v.stored, v.raw) || v.metadata.Range.Length != int64(len(v.stored)) || v.metadata.RawBytes != int64(len(v.raw)) || v.metadata.CRC32C != crc || v.metadata.Key.Hash != hex.EncodeToString(sum[:]) || v.want != verified {
					t.Fatal("invalid positive fixture")
				}
				if v.name == "valid zero CRC32C" {
					if crc != 0 || len(v.stored) != 4 {
						t.Fatal("not a real nonempty CRC zero fixture")
					}
					t.Logf("CRC32C=0 payload: %x", v.stored)
				}
			case "valid zstd":
				if err := validatePositiveFixtureCodec(v); err != nil {
					t.Fatal(err)
				}
				decoded, e := decoder.DecodeAll(v.stored, nil)
				if e != nil || !bytes.Equal(decoded, v.raw) || v.metadata.CRC32C != crc || v.metadata.RawBytes != int64(len(decoded)) || v.metadata.Range.Length != int64(len(v.stored)) || v.metadata.Key.Hash != hex.EncodeToString(sum[:]) || v.want != verified {
					t.Fatal("invalid zstd golden", e)
				}
			case "modified stored bytes", "wrong CRC32C", "CRC precedes decode":
				if v.metadata.CRC32C == crc || v.metadata.Range.Length != int64(len(v.stored)) || v.want != storedCRCFailure {
					t.Fatal("CRC failure not isolated")
				}
			case "wrong stored length", "length precedes CRC":
				if v.metadata.Range.Length == int64(len(v.stored)) || v.want != storedLengthFailure {
					t.Fatal("no length mismatch")
				}
			case "wrong raw length", "raw length precedes hash":
				if v.metadata.RawBytes == int64(len(v.raw)) || v.metadata.CRC32C != crc || v.want != rawLengthFailure {
					t.Fatal("no raw length mismatch")
				}
			case "wrong SHA256":
				if v.metadata.Key.Hash == hex.EncodeToString(sum[:]) || v.metadata.CRC32C != crc || v.metadata.RawBytes != int64(len(v.raw)) || v.want != rawHashFailure {
					t.Fatal("no hash mismatch")
				}
			case "unsupported codec":
				if v.metadata.Codec == "raw" || v.metadata.Codec == "zstd" || v.want != codecFailure {
					t.Fatal("supported codec")
				}
			case "truncated zstd":
				_, e := decoder.DecodeAll(v.stored, nil)
				if e == nil || v.metadata.CRC32C != crc || v.metadata.Range.Length != int64(len(v.stored)) || v.want != decodeFailure {
					t.Fatal("truncation not isolated", e)
				}
			case "zstd trailing byte", "multiple zstd frames":
				_, gold := goldenChunks(t)
				if v.metadata.CRC32C != crc || v.metadata.Range.Length != int64(len(v.stored)) || v.want != decodeFailure {
					t.Fatal("frame failure not isolated")
				}
				if v.name == "zstd trailing byte" && !bytes.Equal(v.stored, append(bytes.Clone(gold.stored), 0)) {
					t.Fatal("missing trailing byte")
				}
				if v.name == "multiple zstd frames" {
					if !bytes.Equal(v.stored, append(bytes.Clone(gold.stored), gold.stored...)) {
						t.Fatal("not two frames")
					}
					decoded, e := decoder.DecodeAll(v.stored, nil)
					if e != nil || len(decoded) != 2*len(gold.raw) {
						t.Fatal("library concatenation premise failed", e)
					}
				}
			case "decode output exceeds expected raw length":
				var header zstd.Header
				if err := header.Decode(v.stored); err != nil {
					t.Fatal(err)
				}
				if !header.HasFCS || header.FrameContentSize != uint64(len(v.raw)) || int64(len(v.raw)) <= v.metadata.RawBytes || v.metadata.CRC32C != crc || v.want != resourceFailure {
					t.Fatal("not an output-cap vector")
				}
			case "stored resource limit":
				if int64(len(v.stored)) <= v.limits.StoredBytes || v.want != resourceFailure {
					t.Fatal("stored budget not exceeded")
				}
			case "raw resource limit":
				if v.metadata.RawBytes <= v.limits.RawBytes || v.want != resourceFailure {
					t.Fatal("raw budget not exceeded")
				}
			case "unsupported dictionary ID", "window exceeds contract bound", "content size exceeds contract bound", "no FCS output exceeds expected raw length", "corrupt zstd with valid stored CRC":
				if err := validateZstdProbe(v); err != nil {
					t.Fatal(err)
				}
				// Only the no-FCS probe is decoded: its independently counted RLE
				// output is 4096 bytes. Large header probes are never decoded.
				if v.name == "no FCS output exceeds expected raw length" {
					decoded, err := decoder.DecodeAll(v.stored, nil)
					if err != nil || !bytes.Equal(decoded, v.raw) {
						t.Fatal("bounded no-FCS decode mismatch", err)
					}
				}
				if v.name == "corrupt zstd with valid stored CRC" {
					if _, err := decoder.DecodeAll(v.stored, nil); err == nil {
						t.Fatal("reserved block unexpectedly decoded")
					}
				}
			default:
				t.Fatal("vector has no independent self-check")
			}
		})
	}
}

// Pure helpers validate fixture evidence, not production verification outcomes.
func validatePositiveFixtureCodec(v verificationVector) error {
	var want string
	switch v.name {
	case "valid raw", "valid zero CRC32C":
		want = "raw"
	case "valid zstd":
		want = "zstd"
	default:
		return fmt.Errorf("not a positive fixture: %s", v.name)
	}
	if v.metadata.Codec != want {
		return fmt.Errorf("%s codec=%q want=%q", v.name, v.metadata.Codec, want)
	}
	return nil
}

func requiredVectorNames() []string {
	return []string{"valid raw", "valid zstd", "valid zero CRC32C", "modified stored bytes", "wrong stored length", "wrong CRC32C", "wrong raw length", "wrong SHA256", "unsupported codec", "truncated zstd", "zstd trailing byte", "multiple zstd frames", "decode output exceeds expected raw length", "stored resource limit", "raw resource limit", "CRC precedes decode", "length precedes CRC", "raw length precedes hash", "unsupported dictionary ID", "window exceeds contract bound", "content size exceeds contract bound", "no FCS output exceeds expected raw length", "corrupt zstd with valid stored CRC"}
}
func validateFixtureInventory(vectors []verificationVector) error {
	required := requiredVectorNames()
	if len(vectors) != len(required) {
		return fmt.Errorf("inventory has %d vectors; want %d", len(vectors), len(required))
	}
	seen := make(map[string]bool)
	for _, v := range vectors {
		if v.name == "" || seen[v.name] {
			return fmt.Errorf("empty or duplicate fixture name: %q", v.name)
		}
		seen[v.name] = true
	}
	for _, name := range required {
		if !seen[name] {
			return fmt.Errorf("missing required fixture: %s", name)
		}
	}
	return nil
}

// This checks only our fixed byte probes using literal field layout and RLE
// block arithmetic, corroborated by Header.Decode. It is not a strict decoder.
func validateZstdProbe(v verificationVector) error {
	b := v.stored
	if len(b) < 10 || v.metadata.Codec != "zstd" || v.metadata.Range.Length != int64(len(b)) || v.metadata.CRC32C != bitwiseCRC32C(b) {
		return fmt.Errorf("invalid stored metadata: %s", v.name)
	}
	sum := sha256.Sum256(v.raw)
	if v.metadata.Key.Hash != hex.EncodeToString(sum[:]) || len(v.raw) != 4096 || int64(len(b)) > v.limits.StoredBytes || int64(len(v.raw)) > v.limits.RawBytes {
		return fmt.Errorf("probe canonical oracle or explicit budgets inconsistent")
	}
	if v.name != "no FCS output exceeds expected raw length" && v.metadata.RawBytes != int64(len(v.raw)) {
		return fmt.Errorf("unexpected raw metadata mismatch")
	}
	if !bytes.Equal(b[:4], []byte{0x28, 0xb5, 0x2f, 0xfd}) {
		return fmt.Errorf("wrong zstd magic")
	}
	var h zstd.Header
	if err := h.Decode(b); err != nil {
		return fmt.Errorf("probe header invalid: %w", err)
	}
	const hard = uint64(16 << 20)
	blockOffset := 0
	var expectedOutput uint64 = 4096
	switch v.name {
	case "unsupported dictionary ID":
		if b[4] != 0x61 || b[5] != 7 || binary.LittleEndian.Uint16(b[6:8])+256 != 4096 || !h.SingleSegment || !h.HasFCS || h.FrameContentSize != 4096 || h.DictionaryID != 7 || v.want != decodeFailure {
			return fmt.Errorf("dictionary premise missing")
		}
		// ID=7 is requested, not supplied. The body is an RLE block that does
		// not need dictionary matches; the metadata is valid framing, not a
		// truncated header pretending to be an unsupported dictionary.
		blockOffset = 8
	case "window exceeds contract bound":
		window := uint64(1) << (10 + (b[5] >> 3))
		window += (window >> 3) * uint64(b[5]&7)
		if b[4] != 0x40 || b[5] != 0x78 || window != 32<<20 || h.SingleSegment || h.WindowSize != window || h.WindowSize <= hard || !h.HasFCS || h.FrameContentSize != 4096 || binary.LittleEndian.Uint16(b[6:8])+256 != 4096 || h.DictionaryID != 0 || v.want != resourceFailure {
			return fmt.Errorf("window premise missing")
		}
		blockOffset = 8
	case "content size exceeds contract bound":
		size := uint64(binary.LittleEndian.Uint32(b[5:9]))
		if b[4] != 0xa0 || size != hard+1 || !h.SingleSegment || !h.HasFCS || h.FrameContentSize != size || h.DictionaryID != 0 || v.want != resourceFailure {
			return fmt.Errorf("content bound premise missing")
		}
		blockOffset = 9
		expectedOutput = size
	case "no FCS output exceeds expected raw length":
		if b[4] != 0 || b[5] != 0x10 || h.SingleSegment || h.HasFCS || h.WindowSize != 4096 || h.DictionaryID != 0 || v.metadata.RawBytes != 1 || v.want != resourceFailure {
			return fmt.Errorf("no-FCS premise missing")
		}
		blockOffset = 6
	case "corrupt zstd with valid stored CRC":
		if b[4] != 0x60 || binary.LittleEndian.Uint16(b[5:7])+256 != 4096 || !h.SingleSegment || !h.HasFCS || h.FrameContentSize != 4096 || h.DictionaryID != 0 || (b[7]>>1)&3 != 3 || v.want != decodeFailure {
			return fmt.Errorf("reserved-block corruption missing")
		}
		return nil // Reserved type is the intentional decode-stage error.
	default:
		return fmt.Errorf("unknown zstd probe")
	}
	if h.HeaderSize != blockOffset {
		return fmt.Errorf("header size disagrees with fixed byte layout")
	}
	var output uint64
	for blocks := 0; blocks < 130; blocks++ {
		if len(b)-blockOffset < 4 {
			return fmt.Errorf("truncated RLE probe")
		}
		header := uint32(b[blockOffset]) | uint32(b[blockOffset+1])<<8 | uint32(b[blockOffset+2])<<16
		n := header >> 3
		if (header>>1)&3 != 1 || n == 0 || n > 128<<10 || b[blockOffset+3] != 0 {
			return fmt.Errorf("invalid fixed RLE block")
		}
		output += uint64(n)
		blockOffset += 4
		if header&1 != 0 {
			if blockOffset != len(b) || output != expectedOutput {
				return fmt.Errorf("RLE framing/output premise mismatch")
			}
			return nil
		}
	}
	return fmt.Errorf("too many fixture blocks")
}

func TestZstdProbePremiseMutations(t *testing.T) {
	for _, v := range verificationVectors(t) {
		switch v.name {
		case "unsupported dictionary ID", "window exceeds contract bound", "content size exceeds contract bound", "no FCS output exceeds expected raw length", "corrupt zstd with valid stored CRC":
		default:
			continue
		}
		t.Run(v.name, func(t *testing.T) {
			if err := validateZstdProbe(v); err != nil {
				t.Fatal(err)
			}
			bad := cloneVector(v, v.name)
			bad.stored[4] |= 8 // reserved header bit; keep CRC/length valid
			rebindStored(&bad)
			if err := validateZstdProbe(bad); err == nil {
				t.Fatal("malformed header passed premise check")
			}
			if bytes.Equal(v.stored, bad.stored) {
				t.Fatal("mutation aliased source vector")
			}
		})
	}
}

func TestPositiveFixtureCodecMutations(t *testing.T) {
	for _, v := range verificationVectors(t) {
		if v.want != verified {
			continue
		}
		t.Run(v.name, func(t *testing.T) {
			if err := validatePositiveFixtureCodec(v); err != nil {
				t.Fatal(err)
			}
			bad := cloneVector(v, v.name)
			if bad.metadata.Codec == "raw" {
				bad.metadata.Codec = "zstd"
			} else {
				bad.metadata.Codec = "raw"
			}
			if err := validatePositiveFixtureCodec(bad); err == nil {
				t.Fatal("wrong positive codec accepted")
			}
			if v.metadata.Codec == bad.metadata.Codec {
				t.Fatal("mutation leaked into source vector")
			}
		})
	}
}

func TestFixtureInventoryMutations(t *testing.T) {
	good := verificationVectors(t)
	if err := validateFixtureInventory(good); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"count", "duplicate", "empty", "missing baseline"} {
		t.Run(kind, func(t *testing.T) {
			bad := append([]verificationVector(nil), good...)
			switch kind {
			case "count":
				bad = bad[:len(bad)-1]
			case "duplicate":
				bad[0].name = bad[1].name
			case "empty":
				bad[0].name = ""
			case "missing baseline":
				bad[0].name = "unknown replacement"
			}
			if err := validateFixtureInventory(bad); err == nil {
				t.Fatal("invalid inventory accepted")
			}
		})
	}
}

// Future consumers call this helper with a real verifier adapter. Phase 3A.0
// deliberately does not call it with a fake verifier that merely echoes want.
func runVerificationContract(t *testing.T, verify verifierAdapter) {
	t.Helper()
	if verify == nil {
		t.Fatal("real verifier adapter required")
	}
	for _, v := range verificationVectors(t) {
		t.Run(v.name, func(t *testing.T) {
			before := bytes.Clone(v.stored)
			raw, category, err := verify(context.Background(), v.metadata, v.stored, v.limits)
			if !bytes.Equal(before, v.stored) {
				t.Fatal("verifier mutated stored input")
			}
			if category != v.want {
				t.Fatalf("category=%s want=%s err=%v", category, v.want, err)
			}
			if v.want == verified {
				if err != nil || !bytes.Equal(raw, v.raw) {
					t.Fatal("verified raw mismatch", err)
				}
			} else if err == nil || len(raw) != 0 {
				t.Fatal("failure returned success or partial raw bytes")
			}
		})
	}
}
func TestProductionVerificationPending(t *testing.T) {
	t.Skip("full 23-vector category/resource contract pending: VerifyPackEntry has lower-level production tests, but no exact category mapping or proven workspace budget")
}

func TestVerificationFixtureDeterminism(t *testing.T) {
	a, b := verificationVectors(t), verificationVectors(t)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("fixture generation is not deterministic")
	}
}
