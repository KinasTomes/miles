package pack

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"runtime"
	"testing"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/klauspost/compress/zstd"
)

// A streaming encoder emits no FCS, exercising the worst supported window
// independently of metadata R. Encoder allocations are outside measurements.
func memoryFrame(t *testing.T, raw []byte, window int) []byte {
	t.Helper()
	var out bytes.Buffer
	enc, err := zstd.NewWriter(&out, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(window))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestDecoderMemoryLimits(t *testing.T) {
	raw := bytes.Repeat([]byte("abcde"), engine.MaxChunkBytes/5+1)[:engine.MaxChunkBytes]
	stored := memoryFrame(t, raw, engine.MaxChunkBytes)
	var h zstd.Header
	if err := h.Decode(stored); err != nil || h.HasFCS || h.WindowSize != engine.MaxChunkBytes {
		t.Fatalf("need max-window/no-FCS frame: %+v %v", h, err)
	}
	for _, codec := range []string{"raw", "zstd"} {
		t.Run(codec, func(t *testing.T) {
			input := raw
			if codec == "zstd" {
				input = stored
			}
			e := memoryEntry(codec, raw, input)
			m, err := EstimateDecodeMemory(e)
			if err != nil {
				t.Fatal(err)
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			got, err := ReadBlob(e, input)
			runtime.ReadMemStats(&after)
			if err != nil || !bytes.Equal(got, raw) || cap(got) != len(raw) {
				t.Fatalf("max-bound decode: cap=%d err=%v", cap(got), err)
			}
			// Total allocation is a regression probe, NOT the proof of peak
			// live memory. Source-derived envelopes include replacement overlap.
			// Raw is checked by exact capacity and ownership; TotalAlloc also
			// includes shared regexp-pool/metadata overhead outside the contract.
			if n := after.TotalAlloc - before.TotalAlloc; codec == "zstd" && n > uint64(m.OutputBytes+m.WorkspaceBytes) {
				t.Fatalf("allocation regression: %d > %+v", n, m)
			}
			t.Logf("additional reservation=%d; total allocated=%d", m.OutputBytes+m.WorkspaceBytes, after.TotalAlloc-before.TotalAlloc)
			runtime.KeepAlive(got)
		})
	}
}

func TestDecoderMemoryRejectsBadFrames(t *testing.T) {
	raw := bytes.Repeat([]byte{'x'}, 4096)
	fcs := rle('x', len(raw))
	noFCS := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0x10}, fcs[7:]...)
	badWindow := bytes.Clone(noFCS)
	badWindow[5] = 0x78 // 32 MiB, over the supported 16 MiB.
	dictionary := append([]byte{0x28, 0xb5, 0x2f, 0xfd, 1, 0x10, 1}, noFCS[6:]...)
	skippable := append([]byte{0x50, 0x2a, 0x4d, 0x18, 0, 0, 0, 0}, noFCS...)
	for name, input := range map[string][]byte{
		"truncated": noFCS[:len(noFCS)-1], "trailing": append(bytes.Clone(noFCS), 0),
		"concatenated": append(bytes.Clone(noFCS), noFCS...), "dictionary": dictionary,
		"skippable": skippable, "window": badWindow,
	} {
		t.Run(name, func(t *testing.T) {
			e := memoryEntry("zstd", raw, input)
			if _, err := EstimateDecodeMemory(e); err != nil {
				t.Fatalf("estimate inspected payload: %v", err)
			}
			if got, err := ReadBlob(e, input); got != nil || err == nil {
				t.Fatalf("bad frame returned bytes: %x %v", got, err)
			}
		})
	}
	for _, input := range [][]byte{fcs, noFCS} {
		for _, change := range []string{"crc", "sha", "short stored", "long stored", "short raw", "long raw"} {
			t.Run(change, func(t *testing.T) {
				e := memoryEntry("zstd", raw, input)
				switch change {
				case "crc":
					e.CRC32C++
				case "sha":
					e.Key.Hash = digest([]byte("other"))
				case "short stored":
					e.StoredLen--
				case "long stored":
					e.StoredLen++
				case "short raw":
					e.RawLength--
				case "long raw":
					e.RawLength++
				}
				if got, err := ReadBlob(e, input); got != nil || err == nil {
					t.Fatalf("bad lengths/checksum returned bytes: %d %v", len(got), err)
				}
			})
		}
	}
}

func TestDecoderMemoryMalformedBlockGrowth(t *testing.T) {
	// Every complete block fits, but raw_len ends within the final block.
	// DecodeAll can grow the destination before rejecting the offending block.
	// Construct raw/RLE/literals-only compressed blocks (including no-FCS),
	// rather than assuming WithDecodeAllCapLimit prevents all such allocation.
	const b = 128 << 10
	for _, kind := range []uint32{0, 1, 2} {
		var input []byte
		input = append(input, 0x28, 0xb5, 0x2f, 0xfd, 0, 0x70) // no FCS, 16 MiB window
		for i := range 4 {
			var payload []byte
			size := b
			switch kind {
			case 0:
				payload = bytes.Repeat([]byte{'x'}, b)
			case 1:
				payload = []byte{'x'}
			case 2:
				// RLE literals (3-byte length header), then zero sequences.
				n := uint32(b)<<4 | 1 | 3<<2
				payload = []byte{byte(n), byte(n >> 8), byte(n >> 16), 'x', 0}
				size = len(payload)
			}
			header := uint32(size)<<3 | kind<<1
			if i == 3 {
				header |= 1
			}
			var bytes [4]byte
			binary.LittleEndian.PutUint32(bytes[:], header)
			input = append(input, bytes[:3]...)
			input = append(input, payload...)
		}
		e := memoryEntry("zstd", []byte{'x'}, input)
		e.RawLength = 3*b + 1
		if err := oneFrame(input, e.RawLength); err != nil {
			t.Fatal(err)
		}
		m, err := EstimateDecodeMemory(e)
		if err != nil {
			t.Fatal(err)
		}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got, err := ReadBlob(e, input)
		runtime.ReadMemStats(&after)
		if err == nil || got != nil {
			t.Fatalf("kind=%d oversized output accepted", kind)
		}
		if n := after.TotalAlloc - before.TotalAlloc; n > uint64(m.OutputBytes+m.WorkspaceBytes) {
			t.Fatalf("kind=%d allocation regression: %d > %+v", kind, n, m)
		}
		t.Logf("kind=%d reservation=%d allocated=%d", kind, m.OutputBytes+m.WorkspaceBytes, after.TotalAlloc-before.TotalAlloc)
	}
}

func FuzzDecodeMemory(f *testing.F) {
	raw := bytes.Repeat([]byte{'x'}, 4096)
	frame := rle('x', len(raw))
	f.Add(frame, uint32(len(raw)))
	f.Add([]byte("raw"), uint32(3))
	f.Fuzz(func(t *testing.T, stored []byte, rawLen uint32) {
		// Keep the fuzz campaign small; max bounds have dedicated tests.
		if len(stored) > 1<<20 || rawLen > 1<<20 {
			t.Skip()
		}
		for _, codec := range []string{"raw", "zstd"} {
			e := memoryEntry(codec, raw, stored)
			e.RawLength = rawLen
			e.CRC32C = crc32.Checksum(stored, crcTable)
			m, estimateErr := EstimateDecodeMemory(e)
			got, err := ReadBlob(e, stored)
			if err != nil && got != nil || estimateErr != nil && err == nil {
				t.Fatal("failure returned bytes or decode bypassed admission validation")
			}
			if err == nil && (cap(got) != int(m.OutputBytes) || !bytes.Equal(got, raw)) {
				t.Fatal("output ownership/length/hash contract violated")
			}
		}
	})
}
