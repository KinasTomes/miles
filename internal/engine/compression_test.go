package engine

import (
	"bytes"
	"errors"
	"math/big"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func TestCompressorRoundTripFallbackAndErrors(t *testing.T) {
	ratio := big.NewRat(1, 10)
	policy := CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: ratio}
	c, err := NewCompressor(policy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ratio.SetInt64(0) // the compressor must retain the admission snapshot.

	raw := bytes.Repeat([]byte("repeated-content-"), 1<<14)
	key, err := HashChunk("default", raw)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := c.Encode(Chunk{Bytes: raw, Key: key})
	if err != nil || encoded.Codec != "zstd" || encoded.EncoderFailed || encoded.RawBytes != len(raw) {
		t.Fatalf("compressed result: %+v err=%v", encoded, err)
	}
	reader, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxChunkBytes))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	decoded, err := reader.DecodeAll(encoded.Bytes, nil)
	if err != nil || !bytes.Equal(decoded, raw) {
		t.Fatalf("round trip err=%v", err)
	}

	// An encoder failure is a raw fallback, while a corrupt frame that was
	// selected for storage is an integrity error.
	c.encode = func([]byte) ([]byte, error) { return nil, errors.New("injected encoder failure") }
	fallback, err := c.Encode(Chunk{Bytes: raw, Key: key})
	if err != nil || fallback.Codec != "raw" || !fallback.EncoderFailed || !bytes.Equal(fallback.Bytes, raw) {
		t.Fatalf("encoder fallback: %+v err=%v", fallback, err)
	}
	c.encode = func([]byte) ([]byte, error) { return []byte("not zstd"), nil }
	if _, err := c.Encode(Chunk{Bytes: raw, Key: key}); err == nil {
		t.Fatal("corrupt selected frame accepted")
	}
	wrong := key
	wrong.Hash = HashForTest(t, []byte("other"))
	if _, err := c.Encode(Chunk{Bytes: raw, Key: wrong}); err == nil {
		t.Fatal("mismatched raw identity accepted")
	}
}

func HashForTest(t *testing.T, raw []byte) string {
	t.Helper()
	key, err := HashChunk("default", raw)
	if err != nil {
		t.Fatal(err)
	}
	return key.Hash
}

func TestCompressionDisabledDoesNotRunEncoder(t *testing.T) {
	policy := CompressionPolicy{Enabled: false, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}
	c, err := NewCompressor(policy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw := []byte("hello")
	key, _ := HashChunk("default", raw)
	result, err := c.Encode(Chunk{Bytes: raw, Key: key})
	if err != nil || result.Codec != "raw" || result.EncoderFailed || !bytes.Equal(result.Bytes, raw) {
		t.Fatalf("disabled result: %+v err=%v", result, err)
	}
	raw[0] = 'x'
	if string(result.Bytes) != "hello" {
		t.Fatal("returned bytes alias caller input")
	}
}

func TestIncompressibleChunkUsesRaw(t *testing.T) {
	policy := CompressionPolicy{Enabled: true, Codec: "zstd", Level: 1, MinSavingRatio: big.NewRat(1, 10)}
	c, err := NewCompressor(policy)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	raw := stream("compression-incompressible", 1<<20)
	key, _ := HashChunk("default", raw)
	result, err := c.Encode(Chunk{Bytes: raw, Key: key})
	if err != nil || result.Codec != "raw" || result.EncoderFailed || !bytes.Equal(result.Bytes, raw) {
		t.Fatalf("incompressible result: codec=%s failed=%v err=%v", result.Codec, result.EncoderFailed, err)
	}
}
