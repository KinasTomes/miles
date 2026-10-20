package pack

// tests/fixtures/format/pack.json pins pack, footer index and sidecar format v1
// of docs/contracts/pack-and-pages.md. No production codec exists yet
// (W02-HAI-02), so these tests run every fixture through a reference encoder
// and verifier written from that document. The production codec must pass the
// same fixtures; its tests then replace these functions.

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"regexp"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

const (
	formatVersion = 1
	trailerLen    = 24
	maxEntries    = 16384
	maxIndexBytes = 2 << 20
	codecRaw      = 0
	codecZstd     = 1
)

var (
	le         = binary.LittleEndian
	identifier = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	castagnoli = crc32.MakeTable(crc32.Castagnoli)
	codecs     = map[string]byte{"raw": codecRaw, "zstd": codecZstd}
	codecNames = map[byte]string{codecRaw: "raw", codecZstd: "zstd"}
)

// formatError names the verification stage that rejected the bytes. Fixtures
// pin the stage, not the message.
type formatError struct{ stage, detail string }

func (e *formatError) Error() string { return e.stage + ": " + e.detail }

func fail(stage, format string, args ...any) error {
	return &formatError{stage, fmt.Sprintf(format, args...)}
}

func stageOf(err error) string {
	var f *formatError
	if errors.As(err, &f) {
		return f.stage
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

type blob struct {
	domain      string
	codec       byte
	raw, stored []byte
	hash        [32]byte
}

type entry struct {
	domain              string
	hash                [32]byte
	codec               byte
	offset              uint64
	stored, raw, crc32c uint32
}

func appendID(b []byte, id string) []byte { return append(append(b, byte(len(id))), id...) }

func header(id, generation string) []byte {
	return appendID(appendID(le.AppendUint32([]byte("MILESPAK"), formatVersion), id), generation)
}

// encode writes the blobs in order and returns the pack object and its
// sidecar, which is byte-identical to the footer index. It validates nothing:
// invalid fixtures go through it so that the verifier must reject them.
func encode(id, generation string, blobs []blob) (pack, index []byte) {
	pack = header(id, generation)
	index = appendID(appendID(le.AppendUint32([]byte("MILESIDX"), formatVersion), id), generation)
	index = le.AppendUint32(index, uint32(len(blobs)))
	for _, b := range blobs {
		index = append(appendID(index, b.domain), b.hash[:]...)
		index = append(index, b.codec)
		index = le.AppendUint64(index, uint64(len(pack)))
		index = le.AppendUint32(index, uint32(len(b.stored)))
		index = le.AppendUint32(index, uint32(len(b.raw)))
		index = le.AppendUint32(index, crc32.Checksum(b.stored, castagnoli))
		pack = append(pack, b.stored...)
	}
	sum := sha256.Sum256(index)
	index = append(index, sum[:]...)
	offset := len(pack)
	pack = le.AppendUint64(append(pack, index...), uint64(offset))
	pack = le.AppendUint32(le.AppendUint32(pack, uint32(len(index))), formatVersion)
	return append(pack, "MILESEND"...), index
}

type reader struct {
	b   []byte
	bad bool
}

func (r *reader) take(n int) []byte {
	if r.bad || n > len(r.b) {
		r.bad = true
		return make([]byte, min(n, 8))
	}
	v := r.b[:n]
	r.b = r.b[n:]
	return v
}
func (r *reader) u8() byte    { return r.take(1)[0] }
func (r *reader) u32() uint32 { return le.Uint32(r.take(4)) }
func (r *reader) u64() uint64 { return le.Uint64(r.take(8)) }
func (r *reader) id() string  { return string(r.take(int(r.u8()))) }

// footer locates the index through the fixed trailer. A reader fetches the
// last 24 bytes (or one larger suffix range) and then the index range.
func footer(pack []byte) (index []byte, offset uint64, err error) {
	if len(pack) < trailerLen || string(pack[len(pack)-8:]) != "MILESEND" {
		return nil, 0, fail("trailer", "missing trailer magic")
	}
	t := pack[len(pack)-trailerLen:]
	offset, n, version := le.Uint64(t), uint64(le.Uint32(t[8:])), le.Uint32(t[12:])
	if version != formatVersion || n > maxIndexBytes || offset > uint64(len(pack)) || offset+n+trailerLen != uint64(len(pack)) {
		return nil, 0, fail("trailer", "version %d, index %d+%d in %d bytes", version, offset, n, len(pack))
	}
	return pack[offset : offset+n], offset, nil
}

// parseIndex verifies a footer index or a sidecar (the same bytes) against the
// PackRef it was addressed by. It returns the entries in offset order and the
// end of the last blob.
func parseIndex(index []byte, want storage.PackRef) ([]entry, uint64, error) {
	if len(index) > maxIndexBytes {
		return nil, 0, fail("index", "%d bytes", len(index))
	}
	if len(index) < sha256.Size {
		return nil, 0, fail("index_checksum", "%d bytes", len(index))
	}
	body := index[:len(index)-sha256.Size]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], index[len(body):]) {
		return nil, 0, fail("index_checksum", "SHA-256 mismatch")
	}
	r := &reader{b: body}
	magic, version, id, generation, n := string(r.take(8)), r.u32(), r.id(), r.id(), r.u32()
	switch {
	case r.bad || magic != "MILESIDX" || version != formatVersion || !identifier.MatchString(id) || !identifier.MatchString(generation):
		return nil, 0, fail("index", "magic, version or identity")
	case n == 0 || n > maxEntries:
		return nil, 0, fail("index", "%d entries", n)
	case id != want.ID || generation != want.Generation:
		return nil, 0, fail("identity", "index of %s/%s read as %s/%s", id, generation, want.ID, want.Generation)
	}
	next := uint64(len(header(id, generation)))
	seen := map[storage.ChunkKey]bool{}
	entries := make([]entry, 0, n)
	for i := range n {
		e := entry{domain: r.id()}
		copy(e.hash[:], r.take(32))
		e.codec, e.offset, e.stored, e.raw, e.crc32c = r.u8(), r.u64(), r.u32(), r.u32(), r.u32()
		key := keyOf(e)
		switch {
		case r.bad:
			return nil, 0, fail("index", "entry %d truncated", i)
		case key.Validate() != nil || seen[key]:
			return nil, 0, fail("index", "entry %d: invalid or duplicate domain/hash", i)
		case e.offset != next || e.stored == 0 || e.raw == 0:
			return nil, 0, fail("index", "entry %d: blob %d+%d, expected offset %d", i, e.offset, e.stored, next)
		case e.codec != codecRaw && e.codec != codecZstd, e.codec == codecRaw && e.stored != e.raw:
			return nil, 0, fail("index", "entry %d: codec %d stored %d raw %d", i, e.codec, e.stored, e.raw)
		}
		seen[key] = true
		next += uint64(e.stored)
		entries = append(entries, e)
	}
	if len(r.b) != 0 {
		return nil, 0, fail("index", "%d bytes after the last entry", len(r.b))
	}
	return entries, next, nil
}

func keyOf(e entry) storage.ChunkKey {
	return storage.ChunkKey{Domain: e.domain, Hash: hex.EncodeToString(e.hash[:])}
}

// verifyPack is the scrub of one pack generation: trailer, index, header and
// every blob.
func verifyPack(pack []byte, want storage.PackRef) error {
	index, offset, err := footer(pack)
	if err != nil {
		return err
	}
	entries, end, err := parseIndex(index, want)
	if err != nil {
		return err
	}
	if !bytes.HasPrefix(pack, header(want.ID, want.Generation)) {
		return fail("header", "magic, version or identity")
	}
	if end != offset {
		return fail("index", "blobs end at %d, index starts at %d", end, offset)
	}
	for _, e := range entries {
		if _, err := readChunk(pack, e); err != nil {
			return err
		}
	}
	return nil
}

// readChunk is the GET path of one entry: RangeGet(offset, stored) bytes are
// checked before decoding and hashed after.
func readChunk(pack []byte, e entry) ([]byte, error) {
	stored := pack[e.offset : e.offset+uint64(e.stored)]
	if crc32.Checksum(stored, castagnoli) != e.crc32c {
		return nil, fail("blob_checksum", "CRC32C mismatch at %d", e.offset)
	}
	raw := stored
	if e.codec == codecZstd {
		var err error
		if raw, err = unzstd(stored); err != nil {
			return nil, fail("content", "%v", err)
		}
	}
	if len(raw) != int(e.raw) || sha256.Sum256(raw) != e.hash {
		return nil, fail("content", "decoded %d bytes do not match raw_len %d/SHA-256", len(raw), e.raw)
	}
	return raw, nil
}

// unzstd decodes only the frames the fixtures use: one RFC 8878 frame, single
// segment with content size, no dictionary or checksum, Raw/RLE blocks. The
// production reader uses a complete zstd decoder.
func unzstd(b []byte) ([]byte, error) {
	if len(b) < 5 || le.Uint32(b) != 0xFD2FB528 || b[4]&0x2f != 0x20 {
		return nil, errors.New("frame outside the reference subset")
	}
	n := [4]int{1, 2, 4, 8}[b[4]>>6]
	if len(b) < 5+n {
		return nil, errors.New("truncated frame header")
	}
	var size uint64
	for i := range n {
		size |= uint64(b[5+i]) << (8 * i)
	}
	if n == 2 {
		size += 256
	}
	var out []byte
	p := 5 + n
	for last := false; !last; {
		if p+3 > len(b) {
			return nil, errors.New("truncated block header")
		}
		h := uint32(b[p]) | uint32(b[p+1])<<8 | uint32(b[p+2])<<16
		p, last = p+3, h&1 == 1
		switch m := int(h >> 3); h >> 1 & 3 {
		case 0:
			if p+m > len(b) {
				return nil, errors.New("truncated raw block")
			}
			out, p = append(out, b[p:p+m]...), p+m
		case 1:
			if p >= len(b) {
				return nil, errors.New("truncated RLE block")
			}
			out, p = append(out, bytes.Repeat(b[p:p+1], m)...), p+1
		default:
			return nil, errors.New("compressed block outside the reference subset")
		}
	}
	if p != len(b) || uint64(len(out)) != size {
		return nil, errors.New("frame size or trailing bytes")
	}
	return out, nil
}

// rle is the one-block zstd frame for n copies of c (single segment, 2-byte
// content size, RLE block), 256 <= n <= 65791.
func rle(c byte, n int) []byte {
	f := le.AppendUint16(append(le.AppendUint32(nil, 0xFD2FB528), 0x60), uint16(n-256))
	h := uint32(n)<<3 | 1<<1 | 1
	return append(f, byte(h), byte(h>>8), byte(h>>16), c)
}

// recipe builds the generated packs described in the fixture note.
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
		n := maxEntries
		if name == "over-max-entries" {
			n++
		}
		for i := range n {
			raw := le.AppendUint32(nil, uint32(i))
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
	Packs   []packCase    `json:"packs"`
	Invalid []packCase    `json:"invalid"`
	Corrupt []corruptCase `json:"corrupt"`
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

func (c packCase) build(t *testing.T) (pack, index []byte) {
	t.Helper()
	blobs := recipe(c.Recipe)
	for _, s := range c.Chunks {
		raw := unhex(t, s.RawHex)
		if s.RawRepeat != nil {
			raw = bytes.Repeat([]byte{s.RawRepeat.Byte}, s.RawRepeat.Count)
		}
		b := blob{domain: s.Domain, codec: 0xff, raw: raw, stored: raw, hash: sha256.Sum256(raw)}
		if codec, ok := codecs[s.Codec]; ok {
			b.codec = codec
		}
		if s.StoredHex != "" {
			b.stored = unhex(t, s.StoredHex)
		}
		if s.Hash != "" {
			copy(b.hash[:], unhex(t, s.Hash))
		}
		blobs = append(blobs, b)
	}
	return encode(c.PackID, c.Generation, blobs)
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestGoldenPacks(t *testing.T) {
	for _, c := range load(t).Packs {
		t.Run(c.Name, func(t *testing.T) {
			pack, index := c.build(t)
			e := c.Expect
			if len(pack) != e.Size || digest(pack) != e.PackSHA256 || digest(index) != e.IndexSHA256 || (e.PackHex != "" && hex.EncodeToString(pack) != e.PackHex) {
				shown := ""
				if e.PackHex != "" {
					shown = hex.EncodeToString(pack)
				}
				t.Fatalf("encoding changed: size %d, pack_sha256 %s, index_sha256 %s, pack_hex %s", len(pack), digest(pack), digest(index), shown)
			}
			ref := storage.PackRef{ID: c.PackID, Generation: c.Generation}
			if err := verifyPack(pack, ref); err != nil {
				t.Fatal(err)
			}
			if footerIndex, _, _ := footer(pack); !bytes.Equal(footerIndex, index) {
				t.Fatal("sidecar is not the footer index")
			}
			// Rebuild reads the sidecar alone; every entry must map to a valid
			// shared-model location (the revision comes from the journal).
			entries, _, err := parseIndex(index, ref)
			if err != nil || len(entries) != e.Entries {
				t.Fatalf("sidecar: %d entries, %v", len(entries), err)
			}
			for _, en := range entries {
				chunk := storage.Chunk{Version: storage.ModelVersion, Key: keyOf(en), RawBytes: int64(en.raw), LocationRevision: 1, Location: storage.ChunkLocation{
					Pack: ref, Range: storage.ByteRange{Offset: int64(en.offset), Length: int64(en.stored)}, Codec: codecNames[en.codec],
				}}
				if err := chunk.Validate(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestInvalidPacks(t *testing.T) {
	for _, c := range load(t).Invalid {
		t.Run(c.Name, func(t *testing.T) {
			pack, _ := c.build(t)
			err := verifyPack(pack, storage.PackRef{ID: c.PackID, Generation: c.Generation})
			if got := stageOf(err); got != c.Stage {
				t.Fatalf("stage %q (%v), want %q", got, err, c.Stage)
			}
		})
	}
}

func TestCorruptPacks(t *testing.T) {
	f := load(t)
	bases := map[string]packCase{}
	for _, c := range f.Packs {
		bases[c.Name] = c
	}
	for _, c := range f.Corrupt {
		t.Run(c.Name, func(t *testing.T) {
			base, ok := bases[c.Base]
			if !ok {
				t.Fatalf("no pack %q", c.Base)
			}
			pack, index := base.build(t)
			b := pack
			if c.Target == "sidecar" {
				b = index
			}
			b = bytes.Clone(b)
			if c.XOR != nil {
				at := c.XOR.At
				if at < 0 {
					at += len(b)
				}
				b[at] ^= byte(c.XOR.Mask)
			}
			b = b[:len(b)-c.Truncate]
			if c.Patch != nil { // a writer bug, not bit rot: the index checksum is valid
				idx := b
				if c.Target == "pack" {
					idx = b[len(b)-trailerLen-len(index) : len(b)-trailerLen]
				}
				copy(idx[c.Patch.At:], unhex(t, c.Patch.Hex))
				sum := sha256.Sum256(idx[:len(idx)-sha256.Size])
				copy(idx[len(idx)-sha256.Size:], sum[:])
			}
			ref := storage.PackRef{ID: base.PackID, Generation: base.Generation}
			if c.ReadAs != nil {
				ref = storage.PackRef{ID: c.ReadAs.PackID, Generation: c.ReadAs.Generation}
			}
			var err error
			if c.Target == "sidecar" {
				_, _, err = parseIndex(b, ref)
			} else {
				err = verifyPack(b, ref)
			}
			if got := stageOf(err); got != c.Stage {
				t.Fatalf("stage %q (%v), want %q", got, err, c.Stage)
			}
		})
	}
}
