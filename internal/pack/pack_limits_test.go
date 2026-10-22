package pack

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"hash/crc32"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/klauspost/compress/zstd"
)

func TestReadBlobZstdOutputBudget(t *testing.T) {
	raw := bytes.Repeat([]byte{'x'}, 4096)
	withFCS := rle('x', len(raw))
	// A non-single-segment frame with a 4 KiB window and no FCS.
	withoutFCS := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0x00, 0x10}, withFCS[7:]...)
	key := storage.ChunkKey{Domain: "default", Hash: digest(raw)}
	for name, stored := range map[string][]byte{"with-fcs": withFCS, "without-fcs": withoutFCS} {
		t.Run(name, func(t *testing.T) {
			e := Entry{Key: key, Codec: "zstd", StoredLen: uint32(len(stored)), RawLength: 1,
				CRC32C: crc32.Checksum(stored, crcTable)}
			_, err := ReadBlob(e, stored)
			var formatErr *FormatError
			if !errors.As(err, &formatErr) || formatErr.Stage != "content" {
				t.Fatalf("oversized output accepted: %v", err)
			}
			e.RawLength = uint32(len(raw))
			got, err := ReadBlob(e, stored)
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatalf("valid frame rejected: %v", err)
			}
			e.RawLength++
			if _, err := ReadBlob(e, stored); err == nil {
				t.Fatal("short decoded output accepted")
			}
		})
	}
}

func TestReadBlobZstdWindowAllocationBudget(t *testing.T) {
	raw := bytes.Repeat([]byte{'x'}, 16<<20)
	var encoded bytes.Buffer
	encoder, err := zstd.NewWriter(&encoded, zstd.WithWindowSize(8<<20), zstd.WithEncoderConcurrency(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	stored := encoded.Bytes()
	var header zstd.Header
	if err := header.Decode(stored); err != nil || header.HasFCS || header.WindowSize != 8<<20 {
		t.Fatalf("probe must have no FCS and an 8 MiB window: %+v err=%v", header, err)
	}
	e := Entry{Key: storage.ChunkKey{Domain: "default", Hash: digest(raw)}, Codec: "zstd",
		StoredLen: uint32(len(stored)), RawLength: uint32(len(raw)), CRC32C: crc32.Checksum(stored, crcTable)}
	// Establish that the frame itself is valid before misdeclaring raw_len.
	if got, err := ReadBlob(e, stored); err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("valid large-window frame rejected: %v", err)
	}
	e.RawLength = 1
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			got, err := ReadBlob(e, stored)
			var formatErr *FormatError
			if got != nil || !errors.As(err, &formatErr) || formatErr.Stage != "content" {
				b.Fatalf("oversized frame accepted: bytes=%d err=%v", len(got), err)
			}
		}
	})
	// Measure the real decoder, including a fresh decoder per call. Allow
	// scratch-space overhead but reject allocation of the advertised window.
	const maxAllocatedBytes = 2 << 20
	if allocated := result.AllocedBytesPerOp(); allocated > maxAllocatedBytes {
		t.Fatalf("decoder allocated %d bytes/op for raw_len=1; budget=%d", allocated, maxAllocatedBytes)
	}
	t.Logf("no-FCS frame: stored=%d bytes, allocated=%d bytes/op", len(stored), result.AllocedBytesPerOp())
}

func TestParseIndexRejectsOversizedPackInventory(t *testing.T) {
	ref := storage.PackRef{ID: "pack-a", Generation: "gen-1"}
	makeIndex := func(lengths ...uint32) []byte {
		t.Helper()
		index := appendPackID(appendPackID(packLE.AppendUint32([]byte("MILESIDX"), Version), ref.ID), ref.Generation)
		index = packLE.AppendUint32(index, uint32(len(lengths)))
		offset := uint64(len(packHeader(ref)))
		for i, length := range lengths {
			hash := sha256.Sum256([]byte{byte(i)})
			index = append(appendPackID(index, "default"), hash[:]...)
			index = append(index, 1) // zstd permits stored_len != raw_len.
			index = packLE.AppendUint64(index, offset)
			index = packLE.AppendUint32(index, length)
			index = packLE.AppendUint32(index, 1)
			index = packLE.AppendUint32(index, 0)
			offset += uint64(length)
		}
		sum := sha256.Sum256(index)
		return append(index, sum[:]...)
	}
	for name, lengths := range map[string][]uint32{
		"single-range": {MaxPackBytes + 1},
		"aggregate":    {16 << 20, 16 << 20, 16 << 20, 16 << 20},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := ParseIndex(makeIndex(lengths...), ref); err == nil {
				t.Fatal("oversized pack inventory accepted")
			}
		})
	}
	index := makeIndex(1)
	limit := uint32(MaxPackBytes - len(packHeader(ref)) - len(index) - TrailerBytes)
	if _, end, err := ParseIndex(makeIndex(limit), ref); err != nil || end+uint64(len(index)+TrailerBytes) != MaxPackBytes {
		t.Fatalf("pack exactly at cap rejected: end=%d err=%v", end, err)
	}
	if _, _, err := ParseIndex(makeIndex(limit+1), ref); err == nil {
		t.Fatal("one byte beyond pack cap accepted")
	}
	if _, _, err := Trailer(make([]byte, TrailerBytes), MaxPackBytes+1); err == nil {
		t.Fatal("oversized physical pack accepted")
	}
}
