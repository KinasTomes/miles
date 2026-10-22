package pack

// The v1 byte goldens are immutable compatibility evidence. Positive cases
// use the production writer and reader in pack_production_test.go; this file
// only interprets fixture recipes and constructs deliberately invalid input.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"os"
	"testing"
)

const (
	codecRaw  = 0
	codecZstd = 1
)

var (
	codecs     = map[string]byte{"raw": codecRaw, "zstd": codecZstd}
	codecNames = map[byte]string{codecRaw: "raw", codecZstd: "zstd"}
)

type blob struct {
	domain      string
	codec       byte
	raw, stored []byte
	hash        [32]byte
}

// rle creates the small, version-pinned zstd fixture frames. It is not a
// production encoder; production writes use engine.Compressor.
func rle(c byte, n int) []byte {
	f := binary.LittleEndian.AppendUint16(append(binary.LittleEndian.AppendUint32(nil, 0xFD2FB528), 0x60), uint16(n-256))
	h := uint32(n)<<3 | 1<<1 | 1
	return append(f, byte(h), byte(h>>8), byte(h>>16), c)
}

func recipe(name string) []blob {
	var out []blob
	add := func(domain string, codec byte, raw, stored []byte) {
		out = append(out, blob{domain, codec, raw, stored, sha256.Sum256(raw)})
	}
	switch name {
	case "large":
		for i := range 2048 {
			if i%2 == 1 {
				raw := bytes.Repeat([]byte{byte(i)}, 4096+i)
				add("default", codecZstd, raw, rle(byte(i), 4096+i))
				continue
			}
			var raw []byte
			for j := 0; len(raw) < 1000+i; j++ {
				sum := sha256.Sum256(fmt.Appendf(nil, "large-%d/%d", i, j))
				raw = append(raw, sum[:]...)
			}
			add("default", codecRaw, raw[:1000+i], raw[:1000+i])
		}
	case "max-entries", "over-max-entries":
		n := MaxEntries
		if name == "over-max-entries" {
			n++
		}
		for i := range n {
			raw := binary.LittleEndian.AppendUint32(nil, uint32(i))
			add("default", codecRaw, raw, raw)
		}
	}
	return out
}

type chunkSpec struct {
	Domain    string `json:"domain"`
	Codec     string `json:"codec"`
	RawHex    string `json:"raw_hex"`
	RawRepeat *struct {
		Byte  byte `json:"byte"`
		Count int  `json:"count"`
	} `json:"raw_repeat"`
	StoredHex string `json:"stored_hex"`
	Hash      string `json:"hash"`
}

type packCase struct {
	Name       string      `json:"name"`
	PackID     string      `json:"pack_id"`
	Generation string      `json:"generation"`
	Recipe     string      `json:"recipe"`
	Chunks     []chunkSpec `json:"chunks"`
	Stage      string      `json:"stage"`
	Expect     struct {
		Size        int    `json:"size"`
		Entries     int    `json:"entries"`
		PackSHA256  string `json:"pack_sha256"`
		IndexSHA256 string `json:"index_sha256"`
		PackHex     string `json:"pack_hex"`
	} `json:"expect"`
}

type corruptCase struct {
	Name     string `json:"name"`
	Base     string `json:"base"`
	Target   string `json:"target"`
	XOR      *struct{ At, Mask int }
	Truncate int `json:"truncate"`
	Patch    *struct {
		At  int    `json:"at"`
		Hex string `json:"hex"`
	} `json:"patch"`
	ReadAs *struct {
		PackID     string `json:"pack_id"`
		Generation string `json:"generation"`
	} `json:"read_as"`
	Stage string `json:"stage"`
}

type fixture struct {
	FormatVersion int           `json:"format_version"`
	Packs         []packCase    `json:"packs"`
	Invalid       []packCase    `json:"invalid"`
	Corrupt       []corruptCase `json:"corrupt"`
}

func load(t *testing.T) fixture {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/format/pack.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.FormatVersion != Version {
		t.Fatalf("fixture version %d, production %d", f.FormatVersion, Version)
	}
	return f
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func fixtureBlobs(t *testing.T, c packCase) []blob {
	t.Helper()
	out := recipe(c.Recipe)
	for _, s := range c.Chunks {
		raw := unhex(t, s.RawHex)
		if s.RawRepeat != nil {
			raw = bytes.Repeat([]byte{s.RawRepeat.Byte}, s.RawRepeat.Count)
		}
		codec, ok := codecs[s.Codec]
		if !ok {
			codec = 0xff
		}
		b := blob{domain: s.Domain, codec: codec, raw: raw, stored: raw, hash: sha256.Sum256(raw)}
		if s.StoredHex != "" {
			b.stored = unhex(t, s.StoredHex)
		}
		if s.Hash != "" {
			copy(b.hash[:], unhex(t, s.Hash))
		}
		out = append(out, b)
	}
	return out
}

// malformedPack is intentionally non-validating. It exists only to feed
// adversarial fixture bytes into the production reader.
func malformedPack(refID, generation string, blobs []blob) ([]byte, []byte) {
	putID := func(dst []byte, id string) []byte { return append(append(dst, byte(len(id))), id...) }
	data := putID(putID(binary.LittleEndian.AppendUint32([]byte("MILESPAK"), Version), refID), generation)
	index := putID(putID(binary.LittleEndian.AppendUint32([]byte("MILESIDX"), Version), refID), generation)
	index = binary.LittleEndian.AppendUint32(index, uint32(len(blobs)))
	for _, b := range blobs {
		index = append(putID(index, b.domain), b.hash[:]...)
		index = append(index, b.codec)
		index = binary.LittleEndian.AppendUint64(index, uint64(len(data)))
		index = binary.LittleEndian.AppendUint32(index, uint32(len(b.stored)))
		index = binary.LittleEndian.AppendUint32(index, uint32(len(b.raw)))
		index = binary.LittleEndian.AppendUint32(index, crc32.Checksum(b.stored, crc32.MakeTable(crc32.Castagnoli)))
		data = append(data, b.stored...)
	}
	sum := sha256.Sum256(index)
	index = append(index, sum[:]...)
	offset := len(data)
	data = append(data, index...)
	data = binary.LittleEndian.AppendUint64(data, uint64(offset))
	data = binary.LittleEndian.AppendUint32(data, uint32(len(index)))
	data = binary.LittleEndian.AppendUint32(data, Version)
	return append(data, "MILESEND"...), index
}
