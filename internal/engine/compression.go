package engine

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/klauspost/compress/zstd"
)

type CompressionPolicy struct {
	Enabled        bool
	Codec          string
	Level          int
	MinSavingRatio *big.Rat
}

func (p CompressionPolicy) Validate() error {
	if p.Codec != "zstd" || p.MinSavingRatio == nil || p.MinSavingRatio.Sign() < 0 || p.MinSavingRatio.Cmp(big.NewRat(1, 1)) >= 0 {
		return fmt.Errorf("%w: compression codec or ratio", storage.ErrInvalid)
	}
	switch p.Level {
	case 1, 3, 7, 11:
		return nil
	default:
		return fmt.Errorf("%w: compression level", storage.ErrInvalid)
	}
}

// StoredCodec makes the threshold decision using exact rational arithmetic.
// When compression was skipped or failed, storedLen is nil and raw wins.
func StoredCodec(enabled bool, ratio *big.Rat, rawLen int64, storedLen *int64) (string, error) {
	if ratio == nil || ratio.Sign() < 0 || ratio.Cmp(big.NewRat(1, 1)) >= 0 || rawLen <= 0 || rawLen > MaxChunkBytes {
		return "", fmt.Errorf("%w: compression decision inputs", storage.ErrInvalid)
	}
	if !enabled || storedLen == nil {
		return "raw", nil
	}
	if *storedLen <= 0 {
		return "", fmt.Errorf("%w: stored length", storage.ErrInvalid)
	}
	// stored * denominator <= raw * (denominator - numerator).
	left := new(big.Int).Mul(big.NewInt(*storedLen), ratio.Denom())
	right := new(big.Int).Sub(ratio.Denom(), ratio.Num())
	right.Mul(right, big.NewInt(rawLen))
	if left.Cmp(right) <= 0 {
		return "zstd", nil
	}
	return "raw", nil
}

type EncodedChunk struct {
	Key           storage.ChunkKey
	Codec         string
	Bytes         []byte
	RawBytes      int
	EncoderFailed bool
}

// Compressor belongs to one stream. It is not safe for concurrent Encode calls.
// Close it when the stream finishes; never re-encode an exact pack-generation
// retry, since encoder output is not a stable identity across versions.
type Compressor struct {
	policy  CompressionPolicy
	encoder *zstd.Encoder
	decoder *zstd.Decoder
	encode  func([]byte) ([]byte, error)
	failed  bool
	closed  bool
}

func NewCompressor(policy CompressionPolicy) (*Compressor, error) {
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	policy.MinSavingRatio = new(big.Rat).Set(policy.MinSavingRatio)
	c := &Compressor{policy: policy}
	if !policy.Enabled {
		return c, nil
	}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(policy.Level)), zstd.WithEncoderConcurrency(1))
	if err != nil {
		c.failed = true
		return c, nil
	}
	c.encoder = encoder
	c.encode = func(raw []byte) ([]byte, error) { return encoder.EncodeAll(raw, nil), nil }
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(MaxChunkBytes))
	if err != nil {
		encoder.Close()
		c.encoder = nil
		c.encode = nil
		c.failed = true
		return c, nil
	}
	c.decoder = decoder
	return c, nil
}

func (c *Compressor) Close() {
	if c == nil || c.closed {
		return
	}
	c.closed = true
	if c.encoder != nil {
		c.encoder.Close()
	}
	if c.decoder != nil {
		c.decoder.Close()
	}
}

func (c *Compressor) Encode(chunk Chunk) (EncodedChunk, error) {
	if c == nil || c.closed {
		return EncodedChunk{}, fmt.Errorf("%w: nil or closed compressor", storage.ErrInvalid)
	}
	if len(chunk.Bytes) == 0 || len(chunk.Bytes) > MaxChunkBytes {
		return EncodedChunk{}, fmt.Errorf("%w: raw chunk size", storage.ErrInvalid)
	}
	key, err := HashChunk(chunk.Key.Domain, chunk.Bytes)
	if err != nil || key != chunk.Key {
		return EncodedChunk{}, fmt.Errorf("%w: raw chunk identity", storage.ErrInvalid)
	}
	rawResult := func(failed bool) EncodedChunk {
		return EncodedChunk{Key: key, Codec: "raw", Bytes: bytes.Clone(chunk.Bytes), RawBytes: len(chunk.Bytes), EncoderFailed: failed}
	}
	if !c.policy.Enabled {
		return rawResult(false), nil
	}
	if c.failed || c.encode == nil {
		return rawResult(true), nil
	}
	stored, err := c.encode(chunk.Bytes)
	if err != nil {
		return rawResult(true), nil
	}
	storedLen := int64(len(stored))
	codec, err := StoredCodec(true, c.policy.MinSavingRatio, int64(len(chunk.Bytes)), &storedLen)
	if err != nil {
		return EncodedChunk{}, err
	}
	if codec == "raw" {
		return rawResult(false), nil
	}
	if c.decoder == nil {
		return EncodedChunk{}, fmt.Errorf("%w: missing zstd verifier", storage.ErrInvalid)
	}
	decoded, err := c.decoder.DecodeAll(stored, nil)
	if err != nil || len(decoded) != len(chunk.Bytes) || !bytes.Equal(decoded, chunk.Bytes) {
		return EncodedChunk{}, fmt.Errorf("%w: zstd round trip for %s", storage.ErrInvalid, key.Hash)
	}
	return EncodedChunk{Key: key, Codec: "zstd", Bytes: stored, RawBytes: len(chunk.Bytes)}, nil
}
