package engine

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"math/bits"

	"github.com/KinasTomes/miles/internal/storage"
)

const (
	MinChunkBytes = 16 << 10
	MaxChunkBytes = 16 << 20
)

// ChunkingParams identifies the v1 FastCDC boundaries for one request.
type ChunkingParams struct {
	Min int `json:"min_bytes"`
	Avg int `json:"avg_bytes"`
	Max int `json:"max_bytes"`
}

func (p ChunkingParams) Validate() error {
	if p.Min < MinChunkBytes || p.Min > p.Avg || p.Avg > p.Max || p.Max > MaxChunkBytes || p.Avg&(p.Avg-1) != 0 {
		return fmt.Errorf("%w: FastCDC min/avg/max", storage.ErrInvalid)
	}
	return nil
}

var fastCDCGear = func() (g [256]uint64) {
	for i := range g {
		h := sha256.Sum256([]byte{byte(i)})
		g[i] = binary.BigEndian.Uint64(h[:8])
	}
	return g
}()

func fastCDCMasks(avg int) (small, large uint64) {
	b := bits.TrailingZeros(uint(avg))
	return ^uint64(0) << (64 - (b + 2)), ^uint64(0) << (64 - (b - 2))
}

// Chunk owns Bytes. The caller may retain it after the next call to Next.
type Chunk struct {
	Bytes  []byte
	Key    storage.ChunkKey
	Offset int64
}

// Chunker reads one logical object or multipart part. It never buffers more
// than Max bytes plus the reader's small buffer. A caller should process and
// release each returned chunk before calling Next again.
type Chunker struct {
	reader *bufio.Reader
	params ChunkingParams
	domain string
	offset int64
	done   bool
}

func NewChunker(r io.Reader, domain string, p ChunkingParams) (*Chunker, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: nil chunk input", storage.ErrInvalid)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := (storage.ChunkKey{Domain: domain, Hash: "0000000000000000000000000000000000000000000000000000000000000000"}).Validate(); err != nil {
		return nil, err
	}
	return &Chunker{reader: bufio.NewReaderSize(r, 32<<10), params: p, domain: domain}, nil
}

// Next returns io.EOF only after all chunks have been emitted. A non-EOF
// reader error discards the incomplete chunk and must fail the whole write.
func (c *Chunker) Next(ctx context.Context) (Chunk, error) {
	if c.done {
		return Chunk{}, io.EOF
	}
	p := c.params
	buf := make([]byte, 0, p.Min)
	small, large := fastCDCMasks(p.Avg)
	var fp uint64
	for len(buf) < p.Max {
		if len(buf)&4095 == 0 {
			if err := ctx.Err(); err != nil {
				c.done = true
				return Chunk{}, err
			}
		}
		b, err := c.reader.ReadByte()
		if err == io.EOF {
			c.done = true
			break
		}
		if err != nil {
			c.done = true
			return Chunk{}, err
		}
		i := len(buf)
		if i >= p.Min {
			fp = fp<<1 + fastCDCGear[b]
			mask := large
			if i < p.Avg {
				mask = small
			}
			if fp&mask == 0 {
				if err := c.reader.UnreadByte(); err != nil {
					c.done = true
					return Chunk{}, err
				}
				break
			}
		}
		buf = append(buf, b)
	}
	if len(buf) == 0 {
		return Chunk{}, io.EOF
	}
	if err := ctx.Err(); err != nil {
		c.done = true
		return Chunk{}, err
	}
	if c.offset > storage.MaxObjectBytes-int64(len(buf)) {
		c.done = true
		return Chunk{}, fmt.Errorf("%w: object exceeds size limit", storage.ErrInvalid)
	}
	key, err := HashChunk(c.domain, buf)
	if err != nil {
		c.done = true
		return Chunk{}, err
	}
	out := Chunk{Bytes: buf, Key: key, Offset: c.offset}
	c.offset += int64(len(buf))
	return out, nil
}

// HashChunk uses the SHA-256 of raw bytes; the domain is a separate part of
// identity and is never included in the content digest.
func HashChunk(domain string, raw []byte) (storage.ChunkKey, error) {
	if len(raw) == 0 || len(raw) > MaxChunkBytes {
		return storage.ChunkKey{}, fmt.Errorf("%w: raw chunk size", storage.ErrInvalid)
	}
	h := sha256.Sum256(raw)
	key := storage.ChunkKey{Domain: domain, Hash: hex.EncodeToString(h[:])}
	if err := key.Validate(); err != nil {
		return storage.ChunkKey{}, err
	}
	return key, nil
}
