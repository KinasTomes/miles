// Package pack implements the immutable v1 pack and sidecar byte format.
package pack

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"regexp"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/klauspost/compress/zstd"
)

const (
	Version       = 1
	TargetBytes   = 48 << 20
	MaxPackBytes  = 64 << 20
	MaxEntries    = 16384
	MaxIndexBytes = 2 << 20
	TrailerBytes  = 24
)

var (
	packIdentifier = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	packLE         = binary.LittleEndian
	crcTable       = crc32.MakeTable(crc32.Castagnoli)
	ErrFull        = errors.New("pack is full")
)

type FormatError struct {
	Stage string
	Err   error
}

func (e *FormatError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *FormatError) Unwrap() error { return e.Err }
func invalid(stage, format string, a ...any) error {
	return &FormatError{Stage: stage, Err: fmt.Errorf(format, a...)}
}

// Blob is one already-selected raw or zstd chunk. Key.Hash always names raw
// content, never the stored representation.
type Blob struct {
	Key       storage.ChunkKey
	Codec     string
	RawLength uint32
	Stored    []byte
}

type Entry struct {
	Key       storage.ChunkKey
	Codec     string
	Offset    uint64
	StoredLen uint32
	RawLength uint32
	CRC32C    uint32
}

func (e Entry) Location(ref storage.PackRef) storage.ChunkLocation {
	return storage.ChunkLocation{Pack: ref, Range: storage.ByteRange{Offset: int64(e.Offset), Length: int64(e.StoredLen)}, Codec: e.Codec, CRC32C: e.CRC32C}
}

func NewRef() (storage.PackRef, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return storage.PackRef{}, err
	}
	return storage.PackRef{ID: hex.EncodeToString(b[:16]), Generation: hex.EncodeToString(b[16:])}, nil
}

func validRef(ref storage.PackRef) bool {
	return packIdentifier.MatchString(ref.ID) && packIdentifier.MatchString(ref.Generation)
}

func appendPackID(dst []byte, id string) []byte { return append(append(dst, byte(len(id))), id...) }
func packHeader(ref storage.PackRef) []byte {
	return appendPackID(appendPackID(packLE.AppendUint32([]byte("MILESPAK"), Version), ref.ID), ref.Generation)
}

// Builder accumulates one bounded pack. A caller finishes it and starts a new
// generation when Add returns ErrFull or Ready becomes true.
type Builder struct {
	ref      storage.PackRef
	blobs    []Blob
	seen     map[storage.ChunkKey]struct{}
	stored   int
	index    int
	finished bool
}

func NewBuilder(ref storage.PackRef) (*Builder, error) {
	if !validRef(ref) {
		return nil, invalid("index", "invalid pack identity")
	}
	return &Builder{ref: ref, seen: make(map[storage.ChunkKey]struct{}),
		index: 8 + 4 + 1 + len(ref.ID) + 1 + len(ref.Generation) + 4 + sha256.Size}, nil
}

func (b *Builder) Ready() bool { return b.stored >= TargetBytes || len(b.blobs) >= MaxEntries }

func (b *Builder) Add(blob Blob) error {
	if b == nil {
		return invalid("index", "nil builder")
	}
	if b.finished {
		return storage.ErrConflict
	}
	if err := validateBlob(blob); err != nil {
		return err
	}
	if _, exists := b.seen[blob.Key]; exists {
		return invalid("index", "duplicate chunk identity")
	}
	entryBytes := 1 + len(blob.Key.Domain) + 32 + 1 + 8 + 4 + 4 + 4
	if len(b.blobs) == MaxEntries || b.index+entryBytes > MaxIndexBytes ||
		len(packHeader(b.ref))+b.stored+len(blob.Stored)+b.index+entryBytes+TrailerBytes > MaxPackBytes {
		return ErrFull
	}
	blob.Stored = bytes.Clone(blob.Stored)
	b.blobs = append(b.blobs, blob)
	b.seen[blob.Key] = struct{}{}
	b.stored += len(blob.Stored)
	b.index += entryBytes
	return nil
}

func validateBlob(blob Blob) error {
	if err := blob.Key.Validate(); err != nil || blob.RawLength == 0 || blob.RawLength > engine.MaxChunkBytes ||
		len(blob.Stored) == 0 || len(blob.Stored) > MaxPackBytes {
		return invalid("index", "invalid chunk identity or length")
	}
	if blob.Codec != "raw" && blob.Codec != "zstd" || blob.Codec == "raw" && len(blob.Stored) != int(blob.RawLength) {
		return invalid("index", "invalid codec or stored length")
	}
	e := Entry{Key: blob.Key, Codec: blob.Codec, RawLength: blob.RawLength,
		StoredLen: uint32(len(blob.Stored)), CRC32C: crc32.Checksum(blob.Stored, crcTable)}
	_, err := ReadBlob(e, blob.Stored)
	return err
}

// Finish returns the exact bytes to reuse for a retry of this generation.
// The sidecar is byte-identical to the footer index.
func (b *Builder) Finish() (packBytes, sidecar []byte, entries []Entry, err error) {
	if b == nil || len(b.blobs) == 0 {
		return nil, nil, nil, invalid("index", "empty pack")
	}
	if b.finished {
		return nil, nil, nil, storage.ErrConflict
	}
	packBytes = make([]byte, 0, len(packHeader(b.ref))+b.stored+b.index+TrailerBytes)
	packBytes = append(packBytes, packHeader(b.ref)...)
	sidecar = appendPackID(appendPackID(packLE.AppendUint32([]byte("MILESIDX"), Version), b.ref.ID), b.ref.Generation)
	sidecar = packLE.AppendUint32(sidecar, uint32(len(b.blobs)))
	for _, blob := range b.blobs {
		hash, _ := hex.DecodeString(blob.Key.Hash)
		codec := byte(0)
		if blob.Codec == "zstd" {
			codec = 1
		}
		e := Entry{Key: blob.Key, Codec: blob.Codec, Offset: uint64(len(packBytes)),
			StoredLen: uint32(len(blob.Stored)), RawLength: blob.RawLength, CRC32C: crc32.Checksum(blob.Stored, crcTable)}
		sidecar = append(appendPackID(sidecar, blob.Key.Domain), hash...)
		sidecar = append(sidecar, codec)
		sidecar = packLE.AppendUint64(sidecar, e.Offset)
		sidecar = packLE.AppendUint32(sidecar, e.StoredLen)
		sidecar = packLE.AppendUint32(sidecar, e.RawLength)
		sidecar = packLE.AppendUint32(sidecar, e.CRC32C)
		packBytes = append(packBytes, blob.Stored...)
		entries = append(entries, e)
	}
	sum := sha256.Sum256(sidecar)
	sidecar = append(sidecar, sum[:]...)
	if len(sidecar) > MaxIndexBytes {
		return nil, nil, nil, invalid("index", "index too large")
	}
	offset := len(packBytes)
	packBytes = append(packBytes, sidecar...)
	packBytes = packLE.AppendUint64(packBytes, uint64(offset))
	packBytes = packLE.AppendUint32(packBytes, uint32(len(sidecar)))
	packBytes = packLE.AppendUint32(packBytes, Version)
	packBytes = append(packBytes, "MILESEND"...)
	b.finished = true
	return packBytes, sidecar, entries, nil
}

type cursor struct {
	b   []byte
	bad bool
}

func (r *cursor) take(n int) []byte {
	if r.bad || n < 0 || n > len(r.b) {
		r.bad = true
		return make([]byte, min(max(n, 0), 8))
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}
func (r *cursor) u8() byte    { return r.take(1)[0] }
func (r *cursor) u32() uint32 { return packLE.Uint32(r.take(4)) }
func (r *cursor) u64() uint64 { return packLE.Uint64(r.take(8)) }
func (r *cursor) id() string  { return string(r.take(int(r.u8()))) }

// Trailer locates the footer index without reading the complete pack.
func Trailer(tail []byte, size int64) (uint64, uint32, error) {
	if len(tail) != TrailerBytes || size < TrailerBytes || string(tail[16:]) != "MILESEND" {
		return 0, 0, invalid("trailer", "missing trailer")
	}
	offset, length, version := packLE.Uint64(tail), packLE.Uint32(tail[8:]), packLE.Uint32(tail[12:])
	if version != Version || length > MaxIndexBytes || offset > uint64(size) ||
		uint64(length) > uint64(size)-offset || offset+uint64(length) != uint64(size-TrailerBytes) {
		return 0, 0, invalid("trailer", "invalid index position, length or version")
	}
	return offset, length, nil
}

// ParseIndex verifies a sidecar independently of pack availability.
func ParseIndex(index []byte, want storage.PackRef) ([]Entry, uint64, error) {
	if len(index) > MaxIndexBytes {
		return nil, 0, invalid("index", "index too large")
	}
	if len(index) < sha256.Size {
		return nil, 0, invalid("index_checksum", "truncated checksum")
	}
	body := index[:len(index)-sha256.Size]
	sum := sha256.Sum256(body)
	if !bytes.Equal(sum[:], index[len(body):]) {
		return nil, 0, invalid("index_checksum", "SHA-256 mismatch")
	}
	r := &cursor{b: body}
	magic, version, id, generation, count := string(r.take(8)), r.u32(), r.id(), r.id(), r.u32()
	if r.bad || magic != "MILESIDX" || version != Version || !packIdentifier.MatchString(id) || !packIdentifier.MatchString(generation) ||
		count == 0 || count > MaxEntries {
		return nil, 0, invalid("index", "invalid index header")
	}
	if id != want.ID || generation != want.Generation {
		return nil, 0, invalid("identity", "index identity mismatch")
	}
	next := uint64(len(packHeader(want)))
	seen := make(map[storage.ChunkKey]struct{}, count)
	entries := make([]Entry, 0, count)
	for i := uint32(0); i < count; i++ {
		domain := r.id()
		hash := hex.EncodeToString(r.take(32))
		codec := r.u8()
		e := Entry{Key: storage.ChunkKey{Domain: domain, Hash: hash}, Offset: r.u64(),
			StoredLen: r.u32(), RawLength: r.u32(), CRC32C: r.u32()}
		if r.bad {
			return nil, 0, invalid("index", "truncated entry")
		}
		switch codec {
		case 0:
			e.Codec = "raw"
		case 1:
			e.Codec = "zstd"
		default:
			return nil, 0, invalid("index", "unknown codec")
		}
		if _, duplicate := seen[e.Key]; e.Key.Validate() != nil || duplicate || e.Offset != next ||
			e.StoredLen == 0 || e.RawLength == 0 || e.RawLength > engine.MaxChunkBytes ||
			e.Codec == "raw" && e.StoredLen != e.RawLength {
			return nil, 0, invalid("index", "invalid entry %d", i)
		}
		seen[e.Key] = struct{}{}
		next += uint64(e.StoredLen)
		entries = append(entries, e)
	}
	if len(r.b) != 0 {
		return nil, 0, invalid("index", "trailing index bytes")
	}
	return entries, next, nil
}

// Inspect verifies a complete readable pack, including every stored blob.
func Inspect(data []byte, want storage.PackRef) ([]Entry, []byte, error) {
	if len(data) < TrailerBytes {
		return nil, nil, invalid("trailer", "short pack")
	}
	offset, length, err := Trailer(data[len(data)-TrailerBytes:], int64(len(data)))
	if err != nil {
		return nil, nil, err
	}
	index := data[offset : offset+uint64(length)]
	entries, end, err := ParseIndex(index, want)
	if err != nil {
		return nil, nil, err
	}
	if !validRef(want) || !bytes.HasPrefix(data, packHeader(want)) {
		return nil, nil, invalid("header", "header identity or version mismatch")
	}
	if end != offset {
		return nil, nil, invalid("index", "unindexed bytes before footer")
	}
	for _, e := range entries {
		if _, err := ReadBlob(e, data[e.Offset:e.Offset+uint64(e.StoredLen)]); err != nil {
			return nil, nil, err
		}
	}
	return entries, bytes.Clone(index), nil
}

// ReadBlob validates physical and logical integrity before releasing bytes.
func ReadBlob(e Entry, stored []byte) ([]byte, error) {
	if e.RawLength == 0 || e.RawLength > engine.MaxChunkBytes || e.StoredLen == 0 {
		return nil, invalid("index", "invalid chunk length")
	}
	if len(stored) != int(e.StoredLen) || crc32.Checksum(stored, crcTable) != e.CRC32C {
		return nil, invalid("blob_checksum", "stored bytes or CRC32C mismatch")
	}
	raw := stored
	if e.Codec == "zstd" {
		if err := oneFrame(stored); err != nil {
			return nil, invalid("content", "zstd frame: %v", err)
		}
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(engine.MaxChunkBytes))
		if err != nil {
			return nil, err
		}
		defer decoder.Close()
		raw, err = decoder.DecodeAll(stored, nil)
		if err != nil {
			return nil, invalid("content", "zstd decode: %v", err)
		}
	} else if e.Codec != "raw" {
		return nil, invalid("index", "unknown codec")
	}
	if len(raw) != int(e.RawLength) || e.Key.Validate() != nil || hex.EncodeToString(sha256Bytes(raw)) != e.Key.Hash {
		return nil, invalid("content", "raw length or SHA-256 mismatch")
	}
	return bytes.Clone(raw), nil
}

// ReadFooter uses two bounded physical ranges, not a full-object GET.
func ReadFooter(ctx context.Context, store storage.PackStore, ref storage.PackRef) ([]Entry, []byte, error) {
	info, err := store.HeadPack(ctx, ref)
	if err != nil {
		return nil, nil, err
	}
	if info.Size < TrailerBytes {
		return nil, nil, invalid("trailer", "short pack")
	}
	tail, err := readRange(ctx, store, ref, storage.ByteRange{Offset: info.Size - TrailerBytes, Length: TrailerBytes})
	if err != nil {
		return nil, nil, err
	}
	offset, length, err := Trailer(tail, info.Size)
	if err != nil {
		return nil, nil, err
	}
	index, err := readRange(ctx, store, ref, storage.ByteRange{Offset: int64(offset), Length: int64(length)})
	if err != nil {
		return nil, nil, err
	}
	entries, end, err := ParseIndex(index, ref)
	if err != nil {
		return nil, nil, err
	}
	if end != offset {
		return nil, nil, invalid("index", "unindexed bytes before footer")
	}
	return entries, index, nil
}

// ReadBlobRange verifies the exact stored bytes before returning raw content.
func ReadBlobRange(ctx context.Context, store storage.PackStore, ref storage.PackRef, e Entry) ([]byte, error) {
	stored, err := readRange(ctx, store, ref, storage.ByteRange{Offset: int64(e.Offset), Length: int64(e.StoredLen)})
	if err != nil {
		return nil, err
	}
	return ReadBlob(e, stored)
}

// ReadCanonicalChunk is the normal GET path: the canonical Chunks row carries
// the CRC and exact range, so no footer fetch is needed on each chunk read.
func ReadCanonicalChunk(ctx context.Context, store storage.PackStore, chunk storage.Chunk) ([]byte, error) {
	if err := chunk.Validate(); err != nil || chunk.RawBytes > engine.MaxChunkBytes || chunk.Location.Range.Length > MaxPackBytes {
		return nil, storage.ErrInvalid
	}
	e := Entry{Key: chunk.Key, Codec: chunk.Location.Codec, Offset: uint64(chunk.Location.Range.Offset),
		StoredLen: uint32(chunk.Location.Range.Length), RawLength: uint32(chunk.RawBytes), CRC32C: chunk.Location.CRC32C}
	return ReadBlobRange(ctx, store, chunk.Location.Pack, e)
}

func readRange(ctx context.Context, store storage.PackStore, ref storage.PackRef, span storage.ByteRange) ([]byte, error) {
	if span.Length <= 0 || span.Length > MaxPackBytes {
		return nil, storage.ErrInvalid
	}
	r, err := store.RangeGet(ctx, ref, span)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, span.Length+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != span.Length {
		return nil, invalid("blob_checksum", "short or oversized range")
	}
	return data, nil
}

func sha256Bytes(b []byte) []byte { sum := sha256.Sum256(b); return sum[:] }

// oneFrame rejects concatenated/skippable frames, dictionaries and trailing
// bytes; DecodeAll alone accepts concatenated frames.
func oneFrame(stored []byte) error {
	var h zstd.Header
	if err := h.Decode(stored); err != nil {
		return err
	}
	if h.Skippable || h.DictionaryID != 0 || h.WindowSize > engine.MaxChunkBytes ||
		h.HasFCS && h.FrameContentSize > engine.MaxChunkBytes {
		return fmt.Errorf("unsupported zstd frame: skip=%v dict=%d window=%d fcs=%d", h.Skippable, h.DictionaryID, h.WindowSize, h.FrameContentSize)
	}
	p := h.HeaderSize
	for {
		if len(stored)-p < 3 {
			return io.ErrUnexpectedEOF
		}
		block := uint32(stored[p]) | uint32(stored[p+1])<<8 | uint32(stored[p+2])<<16
		p += 3
		kind, length := (block>>1)&3, int(block>>3)
		if kind == 3 || length > 128<<10 {
			return errors.New("invalid zstd block")
		}
		if kind == 1 {
			length = 1
		}
		if length > len(stored)-p {
			return io.ErrUnexpectedEOF
		}
		p += length
		if block&1 != 0 {
			break
		}
	}
	if h.HasCheckSum {
		p += 4
	}
	if p != len(stored) {
		return errors.New("trailing bytes or frame")
	}
	return nil
}
