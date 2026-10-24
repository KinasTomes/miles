package pack

import (
	"bytes"
	"encoding/json"
	"hash/crc32"
	"math"
	"os/exec"
	"reflect"
	"runtime"
	"testing"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/klauspost/compress/fse"
	"github.com/klauspost/compress/huff0"
	"github.com/klauspost/compress/zstd"
)

func memoryEntry(codec string, raw, stored []byte) Entry {
	return Entry{Key: storage.ChunkKey{Domain: "default", Hash: digest(raw)}, Codec: codec,
		RawLength: uint32(len(raw)), StoredLen: uint32(len(stored)), CRC32C: crc32.Checksum(stored, crcTable)}
}

func TestEstimateDecodeMemory(t *testing.T) {
	for _, codec := range []string{"raw", "zstd"} {
		e := memoryEntry(codec, []byte("owned"), []byte("owned"))
		m, err := EstimateDecodeMemory(e)
		if err != nil || m.InputBytes != 5 || m.OutputBytes != 5 || (m.WorkspaceBytes == 0) != (codec == "raw") {
			t.Fatalf("%s: %+v %v", codec, m, err)
		}
		// Repeated successful estimates allocate no payload or decoder state.
		if n := testing.AllocsPerRun(100, func() {
			if _, err := EstimateDecodeMemory(e); err != nil {
				panic(err)
			}
		}); n != 0 {
			t.Fatalf("metadata-only estimate allocates: %g", n)
		}
		e.RawLength = engine.MaxChunkBytes
		e.StoredLen = engine.MaxChunkBytes
		if m, err := EstimateDecodeMemory(e); err != nil || m.OutputBytes+m.WorkspaceBytes <= 0 {
			t.Fatalf("max raw: %+v %v", m, err)
		}
		if codec == "zstd" {
			e.StoredLen = MaxPackBytes
			if m, err := EstimateDecodeMemory(e); err != nil || m.InputBytes != MaxPackBytes {
				t.Fatalf("max stored: %+v %v", m, err)
			}
		}
	}
	base := memoryEntry("raw", []byte("x"), []byte("x"))
	for name, mutate := range map[string]func(*Entry){
		"zero raw":        func(e *Entry) { e.RawLength = 0 },
		"large raw":       func(e *Entry) { e.RawLength = math.MaxUint32 },
		"zero stored":     func(e *Entry) { e.StoredLen = 0 },
		"large stored":    func(e *Entry) { e.StoredLen = math.MaxUint32 },
		"offset overflow": func(e *Entry) { e.Offset = math.MaxUint64 },
		"outside pack":    func(e *Entry) { e.Offset = MaxPackBytes },
		"raw mismatch":    func(e *Entry) { e.StoredLen++ },
		"codec":           func(e *Entry) { e.Codec = "other" },
		"hash":            func(e *Entry) { e.Key.Hash = "bad" },
		"domain":          func(e *Entry) { e.Key.Domain = "BAD" },
	} {
		t.Run(name, func(t *testing.T) {
			e := base
			mutate(&e)
			if m, err := EstimateDecodeMemory(e); err == nil || m != (DecodeMemory{}) {
				t.Fatalf("invalid estimate: %+v %v", m, err)
			}
			if got, err := ReadBlob(e, []byte("x")); err == nil || got != nil {
				t.Fatalf("ReadBlob bypasses validation: %x %v", got, err)
			}
		})
	}
}

func TestReadBlobOutputOwnership(t *testing.T) {
	for _, size := range []int{1, 4097, 128 << 10} {
		raw := bytes.Repeat([]byte{'x'}, size)
		for _, codec := range []string{"raw", "zstd"} {
			stored := bytes.Clone(raw)
			if codec == "zstd" {
				enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
				if err != nil {
					t.Fatal(err)
				}
				stored = enc.EncodeAll(raw, nil)
				enc.Close()
			}
			before := bytes.Clone(stored)
			e := memoryEntry(codec, raw, stored)
			a, err := ReadBlob(e, stored)
			if err != nil || !bytes.Equal(a, raw) || cap(a) != len(raw) {
				t.Fatalf("%s/%d: cap=%d err=%v", codec, size, cap(a), err)
			}
			b, err := ReadBlob(e, stored)
			if err != nil {
				t.Fatal(err)
			}
			a[0] ^= 0xff
			if !bytes.Equal(before, stored) || !bytes.Equal(b, raw) {
				t.Fatalf("%s output aliases input or another decode", codec)
			}
			if _, err := EstimateDecodeMemory(e); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestDecoderMemoryAuditPin(t *testing.T) {
	// This is an upgrade gate, not a runtime subprocess in the estimator.
	// Resolve MVS and replacements too: a go.mod text check misses both.
	out, err := exec.Command("go", "list", "-m", "-json", "github.com/klauspost/compress").Output()
	if err != nil {
		t.Fatal(err)
	}
	var module struct {
		Version string
		Replace json.RawMessage
	}
	if err := json.Unmarshal(out, &module); err != nil {
		t.Fatal(err)
	}
	if module.Version != "v1.20.3" || len(module.Replace) != 0 || runtime.Version() != "go1.27.2" {
		t.Fatalf("decoder memory audit must be renewed: module=%s replace=%s go=%s", module.Version, module.Replace, runtime.Version())
	}
	d, err := newBlobDecoder()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.Reset(bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	v := reflect.ValueOf(d).Elem()
	options := v.FieldByName("frame").Elem().FieldByName("o")
	if !options.FieldByName("lowMem").Bool() ||
		options.FieldByName("maxDecodedSize").Uint() != engine.MaxChunkBytes ||
		options.FieldByName("maxWindowSize").Uint() != engine.MaxChunkBytes ||
		!options.FieldByName("limitToCap").Bool() || options.FieldByName("dicts").Len() != 0 {
		t.Fatal("decoder options no longer match memory audit")
	}
	if v.FieldByName("decoders").Cap() != 1 {
		t.Fatal("decoder concurrency changed")
	}
	// Check fixed-size source-audit envelopes, including private upstream types.
	block, _ := v.Type().FieldByName("decoders")
	blockType := block.Type.Elem().Elem()
	frame, _ := blockType.FieldByName("localFrame")
	frameType := frame.Type.Elem()
	history, _ := frameType.FieldByName("history")
	decs, _ := history.Type.FieldByName("decoders")
	ll, _ := decs.Type.FieldByName("litLengths")
	f, _ := ll.Type.FieldByName("fse")
	if f.Type.Elem().Size() > 8192 {
		t.Fatal("FSE table exceeds audit")
	}
	if v.Type().Size()+blockType.Size()+2*frameType.Size() > 8192 ||
		reflect.TypeFor[huff0.Scratch]().Size()+reflect.TypeFor[fse.Scratch]().Size()+
			513*8+2048*2+256*4+64*4+256*2+256+1024 > 32<<10 {
		t.Fatal("decoder or Huffman state exceeds audit")
	}
}
