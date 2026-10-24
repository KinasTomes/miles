package engine

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func readFixture() (storage.ObjectHead, []storage.ManifestPage) {
	h := storage.ObjectHead{Version: storage.ModelVersion, Object: storage.ObjectKey{Bucket: "demo", BucketID: "bucket-1", Key: "object"}, Revision: 3, Domain: "default", ManifestID: "manifest", Size: 10, Metadata: map[string]string{"a": "b"}}
	ref := func(hash string, n, offset int64) storage.ChunkRef {
		return storage.ChunkRef{Key: storage.ChunkKey{Domain: h.Domain, Hash: strings.Repeat(hash, 64)}, RawBytes: n, LogicalOffset: offset}
	}
	return h, []storage.ManifestPage{{Version: 1, ManifestID: h.ManifestID, Domain: h.Domain, Number: 0, Chunks: []storage.ChunkRef{ref("a", 3, 0), ref("b", 4, 3)}}, {Version: 1, ManifestID: h.ManifestID, Domain: h.Domain, Number: 1, Chunks: []storage.ChunkRef{ref("a", 3, 7)}}}
}
func TestPlanReadRanges(t *testing.T) {
	type segment struct{ logical, raw, offset, length int64 }
	for _, tc := range []struct {
		name string
		span *storage.ByteRange
		want []segment
	}{
		{"full", nil, []segment{{0, 3, 0, 3}, {3, 4, 0, 4}, {7, 3, 0, 3}}},
		{"single partial", &storage.ByteRange{Offset: 4, Length: 2}, []segment{{3, 4, 1, 2}}},
		{"across pages", &storage.ByteRange{Offset: 2, Length: 6}, []segment{{0, 3, 2, 1}, {3, 4, 0, 4}, {7, 3, 0, 1}}},
		{"exact chunk", &storage.ByteRange{Offset: 3, Length: 4}, []segment{{3, 4, 0, 4}}},
		{"first byte", &storage.ByteRange{Length: 1}, []segment{{0, 3, 0, 1}}},
		{"last byte", &storage.ByteRange{Offset: 9, Length: 1}, []segment{{7, 3, 2, 1}}},
		{"last boundary", &storage.ByteRange{Offset: 7, Length: 3}, []segment{{7, 3, 0, 3}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pages := readFixture()
			before, _ := json.Marshal(pages)
			plan, err := PlanRead(h.Object, h, pages, tc.span)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Object != h.Object || plan.HeadRevision != 3 || plan.ManifestID != h.ManifestID {
				t.Fatal("lost binding", plan)
			}
			var got []segment
			var emitted int64
			for _, s := range plan.Chunks {
				got = append(got, segment{s.Ref.LogicalOffset, s.Ref.RawBytes, s.Slice.Offset, s.Slice.Length})
				emitted += s.Slice.Length
			}
			if !reflect.DeepEqual(got, tc.want) || emitted != plan.Range.Length {
				t.Fatalf("got %+v want %+v", got, tc.want)
			}
			if plan.Validation.DescriptorVerified || plan.Validation.ObservedChunks != 3 || plan.Validation.ObservedPages != 2 || plan.Validation.LogicalBytes != 10 {
				t.Fatal(plan.Validation)
			}
			again, e := PlanRead(h.Object, h, pages, tc.span)
			if e != nil || !reflect.DeepEqual(plan, again) {
				t.Fatal("nondeterministic")
			}
			after, _ := json.Marshal(pages)
			if string(before) != string(after) {
				t.Fatal("mutated input")
			}
			// Result owns its slice and contains no input maps/slices.
			original := pages[0].Chunks[0]
			plan.Chunks[0].Ref.RawBytes = 99
			if pages[0].Chunks[0] != original {
				t.Fatal("output aliases input")
			}
			if tc.span != nil {
				saved := plan.Range
				tc.span.Offset = 99
				if plan.Range != saved {
					t.Fatal("range pointer alias")
				}
			}
		})
	}
}
func TestPlanReadEmptyAndRepeatedHashes(t *testing.T) {
	h, p := readFixture()
	plan, err := PlanRead(h.Object, h, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Chunks[0].Ref.Key != plan.Chunks[2].Ref.Key || len(plan.Chunks) != 3 {
		t.Fatal("repeated hash collapsed")
	}
	h.Size = 0
	plan, err = PlanRead(h.Object, h, nil, nil)
	if err != nil || len(plan.Chunks) != 0 || plan.Range.Length != 0 || plan.Validation.DescriptorVerified {
		t.Fatal(plan, err)
	}
	if _, err = PlanRead(h.Object, h, nil, &storage.ByteRange{Length: 1}); !errors.Is(err, ErrInvalidReadRange) {
		t.Fatal(err)
	}
	if _, err = PlanRead(h.Object, h, p, nil); !errors.Is(err, ErrInvalidManifest) {
		t.Fatal(err)
	}
}
func TestPlanReadInvalidRanges(t *testing.T) {
	for _, r := range []storage.ByteRange{{Offset: -1, Length: 1}, {Length: 0}, {Length: -1}, {Offset: 10, Length: 1}, {Offset: 9, Length: 2}, {Offset: math.MaxInt64, Length: 1}, {Offset: 1, Length: math.MaxInt64}} {
		h, p := readFixture()
		plan, err := PlanRead(h.Object, h, p, &r)
		if !errors.Is(err, ErrInvalidReadRange) || !errors.Is(err, storage.ErrInvalid) || !reflect.DeepEqual(plan, ReadPlan{}) {
			t.Fatal(r, plan, err)
		}
	}
}
func TestPlanReadInvalidManifests(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*storage.ObjectHead, *[]storage.ManifestPage)
		want error
	}{
		{"no pages", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { *p = nil }, ErrIncompleteManifest},
		{"missing first page", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { *p = (*p)[1:] }, ErrIncompleteManifest},
		{"missing last page", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { *p = (*p)[:1] }, ErrIncompleteManifest},
		{"duplicate page number", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Number = 0 }, ErrInvalidManifest},
		{"page gap", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Number = 2 }, ErrIncompleteManifest},
		{"reordered pages", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[0], (*p)[1] = (*p)[1], (*p)[0] }, ErrIncompleteManifest},
		{"duplicate chunk", func(h *storage.ObjectHead, p *[]storage.ManifestPage) {
			(*p)[0].Chunks = append((*p)[0].Chunks, (*p)[0].Chunks[0])
		}, ErrInvalidManifest},
		{"overlap", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Chunks[0].LogicalOffset = 6 }, ErrInvalidManifest},
		{"gap", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Chunks[0].LogicalOffset = 8 }, ErrIncompleteManifest},
		{"wrong manifest", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].ManifestID = "other" }, ErrInvalidManifest},
		{"wrong page domain", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Domain = "tenant" }, ErrInvalidManifest},
		{"wrong chunk domain", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Chunks[0].Key.Domain = "tenant" }, ErrInvalidManifest},
		{"invalid hash outside requested range", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Chunks[0].Key.Hash = "bad" }, ErrInvalidManifest},
		{"empty page", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Chunks = nil }, ErrIncompleteManifest},
		{"page version", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Version = 2 }, ErrInvalidManifest},
		{"head version", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { h.Version = 2 }, ErrInvalidManifest},
		{"head revision", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { h.Revision = 0 }, ErrInvalidManifest},
		{"small head size", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { h.Size = 9 }, ErrInvalidManifest},
		{"large head size", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { h.Size = 11 }, ErrIncompleteManifest},
		{"negative size", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { h.Size = -1 }, ErrInvalidManifest},
		{"size overflow", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { h.Size = math.MaxInt64 }, ErrInvalidManifest},
		{"offset overflow", func(h *storage.ObjectHead, p *[]storage.ManifestPage) {
			(*p)[1].Chunks[0].LogicalOffset = math.MaxInt64
		}, ErrInvalidManifest},
		{"length overflow", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Chunks[0].RawBytes = math.MaxInt64 }, ErrInvalidManifest},
		{"zero raw", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Chunks[0].RawBytes = 0 }, ErrInvalidManifest},
		{"oversize raw", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { (*p)[1].Chunks[0].RawBytes = MaxChunkBytes + 1 }, ErrInvalidManifest},
		{"too many pages", func(h *storage.ObjectHead, p *[]storage.ManifestPage) { *p = make([]storage.ManifestPage, 1025) }, ErrInvalidManifest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, p := readFixture()
			tc.edit(&h, &p)
			plan, err := PlanRead(h.Object, h, p, &storage.ByteRange{Length: 1})
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(plan, ReadPlan{}) {
				t.Fatalf("plan=%+v err=%v want=%v", plan, err, tc.want)
			}
		})
	}
}
func TestPlanReadObjectBinding(t *testing.T) {
	for _, change := range []func(*storage.ObjectKey){func(k *storage.ObjectKey) { k.Bucket = "other" }, func(k *storage.ObjectKey) { k.BucketID = "recreated" }, func(k *storage.ObjectKey) { k.Key = "other" }, func(k *storage.ObjectKey) { k.BucketID = "" }} {
		h, p := readFixture()
		expected := h.Object
		change(&expected)
		if _, err := PlanRead(expected, h, p, nil); !errors.Is(err, ErrInvalidManifest) {
			t.Fatal(err)
		}
	}
	h, p := readFixture()
	h.Deleted = true
	if _, err := PlanRead(h.Object, h, p, nil); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
}

type manifestReadStore struct {
	storage.MetadataStore
	pages  []storage.ManifestPage
	err    error
	calls  int
	id     string
	cancel context.CancelFunc
}

func (s *manifestReadStore) GetManifest(ctx context.Context, id string) ([]storage.ManifestPage, error) {
	s.calls++
	s.id = id
	if s.cancel != nil {
		s.cancel()
	}
	return s.pages, s.err
}
func TestResolveReadPlan(t *testing.T) {
	h, p := readFixture()
	s := &manifestReadStore{pages: p}
	plan, err := ResolveReadPlan(context.Background(), s, h.Object, h, nil)
	if err != nil || len(plan.Chunks) != 3 || s.calls != 1 || s.id != h.ManifestID {
		t.Fatal(plan, err, s)
	}
	for _, backend := range []error{storage.ErrNotFound, storage.ErrUnavailable, context.DeadlineExceeded} {
		s := &manifestReadStore{err: backend}
		plan, err := ResolveReadPlan(context.Background(), s, h.Object, h, nil)
		if !errors.Is(err, backend) || !reflect.DeepEqual(plan, ReadPlan{}) {
			t.Fatal(plan, err)
		}
		if backend == storage.ErrNotFound && !errors.Is(err, ErrIncompleteManifest) {
			t.Fatal(err)
		}
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	s = &manifestReadStore{pages: p}
	if _, err := ResolveReadPlan(cancelled, s, h.Object, h, nil); !errors.Is(err, context.Canceled) || s.calls != 0 {
		t.Fatal(err, s.calls)
	}
	after, cancelAfter := context.WithCancel(context.Background())
	defer cancelAfter()
	s = &manifestReadStore{pages: p, cancel: cancelAfter}
	if _, err := ResolveReadPlan(after, s, h.Object, h, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s = &manifestReadStore{pages: p}
	if _, err := ResolveReadPlan(context.Background(), s, h.Object, h, &storage.ByteRange{Offset: 10, Length: 1}); !errors.Is(err, ErrInvalidReadRange) || s.calls != 0 {
		t.Fatal(err, s.calls)
	}
	if _, err := ResolveReadPlan(context.Background(), nil, h.Object, h, nil); !errors.Is(err, storage.ErrInvalid) {
		t.Fatal(err)
	}
	h.Size = 0
	s = &manifestReadStore{err: storage.ErrNotFound}
	if _, err := ResolveReadPlan(context.Background(), s, h.Object, h, nil); err != nil || s.calls != 0 {
		t.Fatal(err, s.calls)
	}
}

// Use decoded logical refs from existing v1 goldens, not a new binary codec.
// This does not verify page envelope checksums or independent descriptor fields.
func TestPlanReadGoldenLogicalRefs(t *testing.T) {
	data, err := os.ReadFile("../../tests/fixtures/format/pages.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Manifests []struct {
			Name, Kind, Owner, Domain string
			Refs                      []struct {
				Hash string
				Raw  int64
			}
			Expect struct {
				Size       int64
				ChunkCount int64 `json:"chunk_count"`
			}
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range fixture.Manifests {
		if f.Kind != "object" || (f.Name != "empty object" && f.Name != "small object") {
			continue
		}
		checked++
		t.Run(f.Name, func(t *testing.T) {
			h, _ := readFixture()
			h.ManifestID = f.Owner
			h.Domain = f.Domain
			h.Size = f.Expect.Size
			var pages []storage.ManifestPage
			var cursor int64
			if len(f.Refs) > 0 {
				page := storage.ManifestPage{Version: 1, ManifestID: f.Owner, Domain: f.Domain}
				for _, r := range f.Refs {
					page.Chunks = append(page.Chunks, storage.ChunkRef{Key: storage.ChunkKey{Domain: f.Domain, Hash: r.Hash}, RawBytes: r.Raw, LogicalOffset: cursor})
					cursor += r.Raw
				}
				pages = append(pages, page)
			}
			plan, err := PlanRead(h.Object, h, pages, nil)
			if err != nil || plan.Validation.ObservedChunks != f.Expect.ChunkCount || plan.Range.Length != f.Expect.Size || plan.Validation.DescriptorVerified {
				t.Fatal(plan, err)
			}
		})
	}
	if checked != 2 {
		t.Fatal("expected empty/small fixture cases", checked)
	}
}

func TestPlanReadAllSlicesAgainstBodyOracle(t *testing.T) {
	h, pages := readFixture()
	// The first and last chunks share content. Expected body is independent of
	// planner intersection arithmetic; it is sliced directly as one byte string.
	bodies := map[string]string{strings.Repeat("a", 64): "abc", strings.Repeat("b", 64): "DEFG"}
	body := "abcDEFGabc"
	for start := 0; start < len(body); start++ {
		for end := start + 1; end <= len(body); end++ {
			span := storage.ByteRange{Offset: int64(start), Length: int64(end - start)}
			plan, err := PlanRead(h.Object, h, pages, &span)
			if err != nil {
				t.Fatal(err)
			}
			var got strings.Builder
			for _, part := range plan.Chunks {
				raw := bodies[part.Ref.Key.Hash]
				got.WriteString(raw[part.Slice.Offset : part.Slice.Offset+part.Slice.Length])
			}
			if got.String() != body[start:end] {
				t.Fatalf("[%d,%d): %q want %q", start, end, got.String(), body[start:end])
			}
		}
	}
}

func TestPlanReadDoesNotInventDescriptorProof(t *testing.T) {
	h, pages := readFixture()
	// A structurally coherent shortened head+manifest cannot be rejected using
	// independent counts/index absent from shared models. Keep that gap explicit.
	h.Size = 7
	plan, err := PlanRead(h.Object, h, pages[:1], nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Validation.DescriptorVerified || plan.Validation.ObservedPages != 1 || plan.Validation.ObservedChunks != 2 {
		t.Fatal(plan.Validation)
	}
}

func TestPlanReadRepeatedKeyRawBytesRegression(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		first, last           int64
		split, outside, valid bool
	}{
		{name: "valid repeated key same page", first: 3, last: 3, valid: true},
		{name: "valid repeated key across pages", first: 3, last: 3, split: true, valid: true},
		{name: "conflict same page", first: 3, last: 2},
		{name: "conflict across pages", first: 3, last: 2, split: true},
		{name: "reversed conflict same page", first: 2, last: 3},
		{name: "reversed conflict across pages", first: 2, last: 3, split: true},
		{name: "conflict entirely outside range", first: 3, last: 2, split: true, outside: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head, pages := readFixture()
			a := storage.ChunkKey{Domain: head.Domain, Hash: strings.Repeat("a", 64)}
			b := storage.ChunkKey{Domain: head.Domain, Hash: strings.Repeat("b", 64)}
			refs := []storage.ChunkRef{
				{Key: a, RawBytes: tc.first, LogicalOffset: 0},
				{Key: b, RawBytes: 4, LogicalOffset: tc.first},
				{Key: a, RawBytes: tc.last, LogicalOffset: tc.first + 4},
			}
			head.Size = tc.first + 4 + tc.last
			pages[0].Chunks = refs
			pages = pages[:1]
			if tc.split {
				pages[0].Chunks = refs[:2]
				pages = append(pages, storage.ManifestPage{Version: storage.ModelVersion, ManifestID: head.ManifestID, Domain: head.Domain, Number: 1, Chunks: refs[2:]})
			}
			// All individual fields, ordering, offsets and totals are valid. Only the
			// cross-occurrence identity/length invariant distinguishes conflict cases.
			for _, page := range pages {
				if err := page.Validate(); err != nil {
					t.Fatalf("bad regression fixture: %v", err)
				}
			}
			var requested *storage.ByteRange
			if tc.outside {
				requested = &storage.ByteRange{Offset: tc.first, Length: 1}
			}
			beforeHead, _ := json.Marshal(head)
			beforePages, _ := json.Marshal(pages)
			plan, err := PlanRead(head.Object, head, pages, requested)
			afterHead, _ := json.Marshal(head)
			afterPages, _ := json.Marshal(pages)
			if string(beforeHead) != string(afterHead) || string(beforePages) != string(afterPages) {
				t.Fatal("planner mutated head or manifest")
			}
			if !tc.valid {
				if !errors.Is(err, ErrInvalidManifest) || !errors.Is(err, storage.ErrInvalid) || !reflect.DeepEqual(plan, ReadPlan{}) {
					t.Fatalf("conflicting RawBytes accepted or partial plan returned: plan=%+v err=%v", plan, err)
				}
				return
			}
			if err != nil || len(plan.Chunks) != len(refs) {
				t.Fatalf("valid occurrences lost: %+v %v", plan, err)
			}
			for i, ref := range refs {
				want := PlannedChunk{Ref: ref, Slice: storage.ByteRange{Length: ref.RawBytes}}
				if plan.Chunks[i] != want {
					t.Fatalf("occurrence %d: %+v want %+v", i, plan.Chunks[i], want)
				}
			}
			if plan.Validation.DescriptorVerified {
				t.Fatal("invented descriptor verification")
			}
		})
	}
}
