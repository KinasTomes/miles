package manifest

// tests/fixtures/format/pages.json pins page format v1 of
// docs/contracts/pack-and-pages.md: ManifestPages, UploadPartPages and
// CompletionPages (completion input and frozen part snapshot). No production
// codec exists yet (W02-HAI-03, W04-CHIEN/HAI), so these tests run every
// fixture through a reference encoder and reader written from that document.
// The production codec must pass the same fixtures; its tests then replace
// these functions.

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

const (
	formatVersion = 1
	maxPageBytes  = 384 << 10
	maxPages      = 1024
	refBytes      = 32 + 4
	maxParts      = 10000
	maxPartBytes  = 5 << 30
)

var (
	le         = binary.LittleEndian
	identifier = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	kinds      = map[string]byte{"object": 1, "part": 2, "completion_input": 3, "completion_snapshot": 4}
	// partition key attribute of the page item per kind (architecture 10.1).
	keyNames      = map[byte]string{1: "manifest_id", 2: "part_manifest_id", 3: "completion_id", 4: "completion_id"}
	checksumBytes = map[int]bool{4: true, 8: true, 20: true, 32: true} // CRC32/CRC32C, CRC64NVME, SHA1, SHA256
)

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

func appendID(b []byte, id string) []byte { return append(append(b, byte(len(id))), id...) }

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

// envelope is the stored body of every page item.
func envelope(kind byte, owner string, no uint32, payload []byte) []byte {
	b := appendID(append(le.AppendUint32([]byte("MILESPAG"), formatVersion), kind), owner)
	b = append(le.AppendUint32(le.AppendUint32(b, no), uint32(len(payload))), payload...)
	sum := sha256.Sum256(b)
	return append(b, sum[:]...)
}

func overhead(owner string) int { return len(envelope(0, owner, 0, nil)) }

type ref struct {
	hash [32]byte
	raw  uint32
}

// refPages fills each page with as many refs as fit in maxPageBytes, in order.
// It also returns each page's first logical offset: the object head's index.
func refPages(kind byte, owner, domain string, refs []ref) (pages [][]byte, firsts []uint64) {
	per := (maxPageBytes - overhead(owner) - (1 + len(domain) + 8 + 4)) / refBytes
	var first uint64
	for len(refs) > 0 {
		n := min(per, len(refs))
		firsts = append(firsts, first)
		p := le.AppendUint32(le.AppendUint64(appendID(nil, domain), first), uint32(n))
		for _, r := range refs[:n] {
			p = le.AppendUint32(append(p, r.hash[:]...), r.raw)
			first += uint64(r.raw)
		}
		pages = append(pages, envelope(kind, owner, uint32(len(pages)), p))
		refs = refs[n:]
	}
	return pages, firsts
}

// streamPages slices a completion stream into pages of at most maxPageBytes.
func streamPages(kind byte, owner string, stream []byte) [][]byte {
	per := maxPageBytes - overhead(owner)
	var pages [][]byte
	for i := 0; i < len(stream); i += per {
		pages = append(pages, envelope(kind, owner, uint32(len(pages)), stream[i:min(i+per, len(stream))]))
	}
	return pages
}

// openPage checks one stored body against the item key it was read under.
func openPage(b []byte, kind byte, owner string, no uint32) ([]byte, error) {
	if len(b) > maxPageBytes {
		return nil, fail("page", "%d bytes", len(b))
	}
	if len(b) < sha256.Size {
		return nil, fail("page_checksum", "%d bytes", len(b))
	}
	body := b[:len(b)-sha256.Size]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], b[len(body):]) {
		return nil, fail("page_checksum", "SHA-256 mismatch")
	}
	r := &reader{b: body}
	magic, version, k, id, n, size := string(r.take(8)), r.u32(), r.u8(), r.id(), r.u32(), r.u32()
	payload := r.take(int(size))
	switch {
	case r.bad || magic != "MILESPAG" || version != formatVersion || !identifier.MatchString(id) || size == 0 || len(r.b) != 0:
		return nil, fail("page", "envelope")
	case k != kind || id != owner || n != no:
		return nil, fail("page_key", "page kind %d %s/%d read as kind %d %s/%d", k, id, n, kind, owner, no)
	}
	return payload, nil
}

func parseRefs(p []byte) (domain string, first uint64, refs []ref, err error) {
	r := &reader{b: p}
	domain, first, n := r.id(), r.u64(), r.u32()
	if r.bad || !identifier.MatchString(domain) || n == 0 || len(r.b) != int(n)*refBytes {
		return "", 0, nil, fail("page", "ref payload")
	}
	for range n {
		var x ref
		copy(x.hash[:], r.take(32))
		if x.raw = r.u32(); x.raw == 0 {
			return "", 0, nil, fail("page", "zero raw_length")
		}
		refs = append(refs, x)
	}
	return domain, first, refs, nil
}

// head holds the fields of an Objects or UploadParts item that locate and
// bound the pages. ChunkCount and PageIndex exist on object heads only
// (-1 and nil for a part head).
type head struct {
	Kind       byte
	Owner      string
	Domain     string
	Size       uint64
	PageCount  uint32
	ChunkCount int64
	PageIndex  []uint64
}

// readManifest reads pages[i] as the item (owner, i) for i < PageCount; nil
// is a missing item. It returns the pages as the shared model sees them.
func readManifest(h head, pages [][]byte) ([]storage.ManifestPage, error) {
	if h.PageCount > maxPages || (h.PageCount == 0) != (h.Size == 0) || h.Size > uint64(storage.MaxObjectBytes) || (h.PageIndex != nil && len(h.PageIndex) != int(h.PageCount)) {
		return nil, fail("manifest", "head: %d pages, %d bytes", h.PageCount, h.Size)
	}
	var out []storage.ManifestPage
	var offset uint64
	var count int64
	for i := range h.PageCount {
		if int(i) >= len(pages) || pages[i] == nil {
			return nil, fail("manifest", "page %d missing", i)
		}
		payload, err := openPage(pages[i], h.Kind, h.Owner, i)
		if err != nil {
			return nil, err
		}
		domain, first, refs, err := parseRefs(payload)
		if err != nil {
			return nil, err
		}
		if domain != h.Domain || first != offset || (h.PageIndex != nil && h.PageIndex[i] != first) {
			return nil, fail("manifest", "page %d: domain %s, first offset %d after %d", i, domain, first, offset)
		}
		page := storage.ManifestPage{Version: storage.ModelVersion, ManifestID: h.Owner, Domain: domain, Number: i}
		for _, r := range refs {
			page.Chunks = append(page.Chunks, storage.ChunkRef{Key: storage.ChunkKey{Domain: domain, Hash: hex.EncodeToString(r.hash[:])}, RawBytes: int64(r.raw), LogicalOffset: int64(offset)})
			offset += uint64(r.raw)
		}
		count += int64(len(refs))
		out = append(out, page)
	}
	if offset != h.Size || (h.ChunkCount >= 0 && count != h.ChunkCount) {
		return nil, fail("manifest", "pages hold %d refs, %d bytes", count, offset)
	}
	return out, nil
}

// pointer is what Uploads stores for one completion stream.
type pointer struct {
	Kind      byte
	Owner     string
	Length    int
	Digest    string
	PageCount uint32
}

func joinStream(p pointer, pages [][]byte) ([]byte, error) {
	var out []byte
	for i := range p.PageCount {
		if int(i) >= len(pages) || pages[i] == nil {
			return nil, fail("stream", "page %d missing", i)
		}
		payload, err := openPage(pages[i], p.Kind, p.Owner, i)
		if err != nil {
			return nil, err
		}
		out = append(out, payload...)
	}
	if len(out) != p.Length || digest(out) != p.Digest {
		return nil, fail("stream", "%d bytes, SHA-256 %s", len(out), digest(out))
	}
	return out, nil
}

type partRecord struct {
	PartNumber     uint32 `json:"part_number"`
	Revision       uint64 `json:"revision"`
	Size           uint64 `json:"size"`
	PageCount      uint32 `json:"page_count"`
	ETag           string `json:"etag"`
	PartManifestID string `json:"part_manifest_id"`
	Checksum       string `json:"checksum"`
}

func encodeSnapshot(parts []partRecord) []byte {
	b := le.AppendUint32(nil, uint32(len(parts)))
	for _, p := range parts {
		b = le.AppendUint32(le.AppendUint64(le.AppendUint64(le.AppendUint32(b, p.PartNumber), p.Revision), p.Size), p.PageCount)
		etag, _ := hex.DecodeString(p.ETag)
		sum, _ := base64.StdEncoding.DecodeString(p.Checksum)
		b = append(appendID(append(b, etag...), p.PartManifestID), byte(len(sum)))
		b = append(b, sum...)
	}
	return b
}

func parseSnapshot(b []byte) ([]partRecord, error) {
	r := &reader{b: b}
	n := r.u32()
	if r.bad || n == 0 || n > maxParts {
		return nil, fail("stream", "%d parts", n)
	}
	var parts []partRecord
	checksumLen := 0
	for i := range n {
		p := partRecord{PartNumber: r.u32(), Revision: r.u64(), Size: r.u64(), PageCount: r.u32()}
		p.ETag, p.PartManifestID = hex.EncodeToString(r.take(md5.Size)), r.id()
		sum := r.take(int(r.u8()))
		p.Checksum = base64.StdEncoding.EncodeToString(sum)
		if i == 0 {
			checksumLen = len(sum) // one algorithm per upload
		}
		switch {
		case r.bad || !identifier.MatchString(p.PartManifestID) || p.Revision == 0 || p.Size > maxPartBytes || (p.Size == 0) != (p.PageCount == 0):
			return nil, fail("stream", "part record %d", i)
		case p.PartNumber == 0 || p.PartNumber > maxParts || (i > 0 && p.PartNumber <= parts[i-1].PartNumber):
			return nil, fail("stream", "part number %d at %d", p.PartNumber, i)
		case !checksumBytes[len(sum)] || len(sum) != checksumLen:
			return nil, fail("stream", "checksum of %d bytes at %d", len(sum), i)
		}
		parts = append(parts, p)
	}
	if len(r.b) != 0 {
		return nil, fail("stream", "%d bytes after the last part", len(r.b))
	}
	return parts, nil
}

// itemSize is the DynamoDB size of a page item {key: S, page_no: N, body: B}:
// attribute names plus values; a number counts 1 byte per two significant
// digits plus 1.
func itemSize(kind byte, owner string, no uint32, body []byte) int {
	digits := len(strconv.FormatUint(uint64(no), 10))
	return len(keyNames[kind]) + len(owner) + len("page_no") + (digits+1)/2 + 1 + len("body") + len(body)
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type pageCase struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Owner  string `json:"owner"`
	Domain string `json:"domain"`
	Refs   []struct {
		Hash string `json:"hash"`
		Raw  uint32 `json:"raw"`
	} `json:"refs"`
	Generate *struct {
		Refs  int    `json:"refs"`
		Raw   uint32 `json:"raw"`
		Parts int    `json:"parts"`
	} `json:"generate"`
	Canonical string       `json:"multipart_canonical"`
	Parts     []partRecord `json:"parts"`
	Stage     string       `json:"stage"`
	Expect    struct {
		Size        uint64   `json:"size"`
		ChunkCount  int64    `json:"chunk_count"`
		PageIndex   []uint64 `json:"page_index"`
		Length      int      `json:"length"`
		Digest      string   `json:"digest"`
		PageBytes   []int    `json:"page_bytes"`
		PagesSHA256 string   `json:"pages_sha256"`
		PageHex     []string `json:"page_hex"`
	} `json:"expect"`
}

type corruptCase struct {
	Name string `json:"name"`
	Base string `json:"base"`
	XOR  *struct {
		Page, At, Mask int
	}
	Append *struct {
		Page int    `json:"page"`
		Hex  string `json:"hex"`
	} `json:"append_resealed"`
	Drop *int  `json:"drop"`
	Swap []int `json:"swap"`
	Head *struct {
		Kind       string   `json:"kind"`
		Owner      string   `json:"owner"`
		Domain     string   `json:"domain"`
		Size       *uint64  `json:"size"`
		PageCount  *uint32  `json:"page_count"`
		ChunkCount *int64   `json:"chunk_count"`
		PageIndex  []uint64 `json:"page_index"`
		Length     *int     `json:"length"`
		Digest     string   `json:"digest"`
	} `json:"head"`
	Stage string `json:"stage"`
}

type fixture struct {
	Manifests []pageCase    `json:"manifests"`
	Streams   []pageCase    `json:"streams"`
	Invalid   []pageCase    `json:"invalid"`
	Corrupt   []corruptCase `json:"corrupt"`
}

func load(t *testing.T) fixture {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/format/pages.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// multipartCanonical returns a canonical input and its input_digest from the
// API 4.3.1 fixture, so completion pages carry the bytes that digest covers.
func multipartCanonical(t *testing.T, name string) (string, string) {
	t.Helper()
	data, err := os.ReadFile("../../tests/fixtures/s3/multipart.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Canonical []struct {
			Name        string `json:"name"`
			Canonical   string `json:"canonical"`
			InputDigest string `json:"input_digest"`
		} `json:"canonical"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Canonical {
		if c.Name == name {
			return c.Canonical, c.InputDigest
		}
	}
	t.Fatalf("no canonical input %q", name)
	return "", ""
}

func partETag(i int) []byte {
	sum := md5.Sum([]byte(strconv.Itoa(i)))
	return sum[:]
}

func partChecksum(i int) []byte {
	sum := sha256.Sum256([]byte(strconv.Itoa(i)))
	return sum[:]
}

// build returns the pages of a case and the head or pointer that reads them.
func (c pageCase) build(t *testing.T) ([][]byte, head, pointer) {
	t.Helper()
	kind, ok := kinds[c.Kind]
	if !ok {
		t.Fatalf("kind %q", c.Kind)
	}
	if kind <= 2 {
		var refs []ref
		for _, r := range c.Refs {
			x := ref{raw: r.Raw}
			b, err := hex.DecodeString(r.Hash)
			if err != nil || copy(x.hash[:], b) != 32 {
				t.Fatalf("ref hash %q", r.Hash)
			}
			refs = append(refs, x)
		}
		if g := c.Generate; g != nil {
			for i := range g.Refs {
				refs = append(refs, ref{sha256.Sum256(fmt.Appendf(nil, "ref-%d", i)), g.Raw})
			}
		}
		pages, firsts := refPages(kind, c.Owner, c.Domain, refs)
		h := head{Kind: kind, Owner: c.Owner, Domain: c.Domain, PageCount: uint32(len(pages)), ChunkCount: -1}
		for _, r := range refs {
			h.Size += uint64(r.raw)
		}
		if kind == 1 {
			h.ChunkCount, h.PageIndex = int64(len(refs)), append([]uint64{}, firsts...)
		}
		return pages, h, pointer{}
	}
	var stream []byte
	switch {
	case c.Canonical != "":
		canonical, _ := multipartCanonical(t, c.Canonical)
		stream = []byte(canonical)
	case kind == 3 && c.Generate != nil:
		var parts []string
		for i := 1; i <= c.Generate.Parts; i++ {
			parts = append(parts, fmt.Sprintf(`{"part_number":%d,"etag":"%x","checksum":"%s"}`, i, partETag(i), base64.StdEncoding.EncodeToString(partChecksum(i))))
		}
		stream = []byte(`{"v":1,"bucket_id":"bkt-demo-0001","key":"large.bin","checksum_algorithm":"SHA256","checksum_type":"COMPOSITE","object_checksum":null,"object_size":null,"parts":[` + strings.Join(parts, ",") + `]}`)
	case kind == 4:
		parts := c.Parts
		if c.Generate != nil {
			for i := 1; i <= c.Generate.Parts; i++ {
				size := uint64(5 << 20)
				if i == c.Generate.Parts {
					size = 32
				}
				parts = append(parts, partRecord{PartNumber: uint32(i), Revision: 1, Size: size, PageCount: 1, ETag: hex.EncodeToString(partETag(i)), PartManifestID: fmt.Sprintf("%032x", i), Checksum: base64.StdEncoding.EncodeToString(partChecksum(i))})
			}
		}
		stream = encodeSnapshot(parts)
	}
	pages := streamPages(kind, c.Owner, stream)
	return pages, head{}, pointer{Kind: kind, Owner: c.Owner, Length: len(stream), Digest: digest(stream), PageCount: uint32(len(pages))}
}

func read(h head, p pointer, pages [][]byte) error {
	if p.Kind == 0 {
		_, err := readManifest(h, pages)
		return err
	}
	stream, err := joinStream(p, pages)
	if err == nil && p.Kind == 4 {
		_, err = parseSnapshot(stream)
	}
	return err
}

func TestGoldenPages(t *testing.T) {
	f := load(t)
	for _, c := range append(f.Manifests, f.Streams...) {
		t.Run(c.Name, func(t *testing.T) {
			pages, h, p := c.build(t)
			e := c.Expect
			var sizes []int
			for i, b := range pages {
				sizes = append(sizes, len(b))
				if n := itemSize(kinds[c.Kind], c.Owner, uint32(i), b); len(b) > maxPageBytes || n >= 400_000 {
					t.Fatalf("page %d: body %d bytes, item %d bytes", i, len(b), n)
				}
			}
			all := bytes.Join(pages, nil)
			if fmt.Sprint(sizes) != fmt.Sprint(e.PageBytes) || digest(all) != e.PagesSHA256 {
				t.Fatalf("encoding changed: page_bytes %v, pages_sha256 %s", sizes, digest(all))
			}
			for i, want := range e.PageHex {
				if got := hex.EncodeToString(pages[i]); got != want {
					t.Fatalf("page %d: %s", i, got)
				}
			}
			if p.Kind != 0 {
				if p.Length != e.Length || p.Digest != e.Digest {
					t.Fatalf("stream: length %d, digest %s", p.Length, p.Digest)
				}
				if c.Canonical != "" {
					if _, inputDigest := multipartCanonical(t, c.Canonical); inputDigest != p.Digest {
						t.Fatalf("pages do not carry the bytes of input_digest %s", inputDigest)
					}
				}
			} else if h.Size != e.Size || (h.Kind == 1 && (h.ChunkCount != e.ChunkCount || fmt.Sprint(h.PageIndex) != fmt.Sprint(e.PageIndex))) {
				t.Fatalf("head: size %d, chunk_count %d, page_index %v", h.Size, h.ChunkCount, h.PageIndex)
			}
			if p.Kind != 0 {
				if err := read(h, p, pages); err != nil {
					t.Fatal(err)
				}
				return
			}
			got, err := readManifest(h, pages)
			if err != nil {
				t.Fatal(err)
			}
			for _, page := range got {
				if err := page.Validate(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestInvalidPages(t *testing.T) {
	for _, c := range load(t).Invalid {
		t.Run(c.Name, func(t *testing.T) {
			pages, h, p := c.build(t)
			if err := read(h, p, pages); stageOf(err) != c.Stage {
				t.Fatalf("stage %q (%v), want %q", stageOf(err), err, c.Stage)
			}
		})
	}
}

func TestCorruptPages(t *testing.T) {
	f := load(t)
	bases := map[string]pageCase{}
	for _, c := range append(f.Manifests, f.Streams...) {
		bases[c.Name] = c
	}
	for _, c := range f.Corrupt {
		t.Run(c.Name, func(t *testing.T) {
			base, ok := bases[c.Base]
			if !ok {
				t.Fatalf("no case %q", c.Base)
			}
			pages, h, p := base.build(t)
			for i := range pages {
				pages[i] = bytes.Clone(pages[i])
			}
			if x := c.XOR; x != nil {
				pages[x.Page][x.At] ^= byte(x.Mask)
			}
			if a := c.Append; a != nil { // a writer bug, not bit rot: the checksum is valid
				b, _ := hex.DecodeString(a.Hex)
				body := append(pages[a.Page][:len(pages[a.Page])-sha256.Size], b...)
				sum := sha256.Sum256(body)
				pages[a.Page] = append(body, sum[:]...)
			}
			if c.Drop != nil {
				pages[*c.Drop] = nil
			}
			if len(c.Swap) == 2 {
				pages[c.Swap[0]], pages[c.Swap[1]] = pages[c.Swap[1]], pages[c.Swap[0]]
			}
			if o := c.Head; o != nil {
				if o.Kind != "" {
					h.Kind = kinds[o.Kind]
				}
				if o.Owner != "" {
					h.Owner, p.Owner = o.Owner, o.Owner
				}
				if o.Domain != "" {
					h.Domain = o.Domain
				}
				if o.Size != nil {
					h.Size = *o.Size
				}
				if o.PageCount != nil {
					h.PageCount = *o.PageCount
				}
				if o.ChunkCount != nil {
					h.ChunkCount = *o.ChunkCount
				}
				if o.PageIndex != nil {
					h.PageIndex = o.PageIndex
				}
				if o.Length != nil {
					p.Length = *o.Length
				}
				if o.Digest != "" {
					p.Digest = o.Digest
				}
			}
			if err := read(h, p, pages); stageOf(err) != c.Stage {
				t.Fatalf("stage %q (%v), want %q", stageOf(err), err, c.Stage)
			}
		})
	}
}
