package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
)

type markFixture struct {
	pages      map[string]s.ObservationPage
	manifests  map[string]s.ManifestPage
	parts      []s.UploadPart
	publishers []s.PartPublisher
}

func (f markFixture) ObservePage(_ context.Context, r s.ObservationRequest) (s.ObservationPage, error) {
	return f.pages[r.Kind+"/"+r.Cursor], nil
}
func (f markFixture) GetManifestPage(_ context.Context, id string, _ uint32) (s.ManifestPage, error) {
	p, ok := f.manifests[id]
	if !ok {
		return p, s.ErrNotFound
	}
	return p, nil
}
func (f markFixture) GetPartPage(ctx context.Context, id string, n uint32) (s.ManifestPage, error) {
	return f.GetManifestPage(ctx, id, n)
}
func (f markFixture) ObserveUploadParts(context.Context, string, uint32, int) (s.PartListPage, error) {
	return s.PartListPage{Parts: f.parts}, nil
}
func (f markFixture) ListPublishers(context.Context, string, string, int) (s.PublisherPage, error) {
	return s.PublisherPage{Publishers: f.publishers}, nil
}

func TestMarkerProtectsEachRootAndCountsOnlyCommittedOccurrences(t *testing.T) {
	h := strings.Repeat("a", 64)
	live := s.PackRef{ID: "pack", Generation: "live"}
	part := s.PackRef{ID: "pack", Generation: "part"}
	pin := s.PackRef{ID: "pack", Generation: "old"}
	recovery := s.PackRef{ID: "pack", Generation: "cut"}
	pending := s.PackRef{ID: "pack", Generation: "pending"}
	a, b := s.ChunkKey{Domain: "a", Hash: h}, s.ChunkKey{Domain: "b", Hash: h}
	row := func(k s.ChunkKey, p s.PackRef) s.Chunk {
		return s.Chunk{Version: 1, Key: k, RawBytes: 4, LocationRevision: 1, Location: s.ChunkLocation{Pack: p, Range: s.ByteRange{Length: 4}, Codec: "raw"}}
	}
	obj := s.ObjectKey{Bucket: "bucket", BucketID: "bucket-id", Key: "key"}
	uploadObj := s.ObjectKey{Bucket: "other", BucketID: "other-id", Key: "key"}
	head := s.ObjectHead{Version: 1, Revision: 1, Object: obj, Domain: "a", ManifestID: "live", Size: 8, PageCount: 1, ChunkCount: 2, PageIndex: []uint64{0}}
	f := markFixture{
		pages: map[string]s.ObservationPage{
			"buckets/":   {Buckets: []s.Bucket{{Name: obj.Bucket, ID: obj.BucketID, Domain: "a", Active: true}}},
			"packs/":     {Packs: []s.PackInfo{{Ref: live, Size: 4}, {Ref: part, Size: 4}, {Ref: pin, Size: 4}}},
			"chunks/":    {Chunks: []s.Chunk{row(a, live), row(b, part)}},
			"heads/":     {Next: "next"},
			"heads/next": {Heads: []s.ObjectHead{head}},
			"uploads/":   {Uploads: []s.MultipartUpload{{ID: "upload", Object: uploadObj, Domain: "b", State: "Completing", PublishedBytes: 4, Publishers: 1}}},
		},
		manifests: map[string]s.ManifestPage{
			"live": {Version: 1, ManifestID: "live", Domain: "a", Chunks: []s.ChunkRef{{Key: a, RawBytes: 4}, {Key: a, RawBytes: 4, LogicalOffset: 4}}},
			"part": {Version: 1, ManifestID: "part", Domain: "b", Chunks: []s.ChunkRef{{Key: b, RawBytes: 4}}},
		},
		parts:      []s.UploadPart{{UploadID: "upload", Number: 1, ManifestID: "part", Pages: 1, Size: 4}},
		publishers: []s.PartPublisher{{UploadID: "upload", State: "Open", Packs: []s.PackRef{pending}}},
	}
	m := Marker{Store: f, MaxRecords: 100, MaxReferences: 100,
		Pinned:        func(_ context.Context, p s.PackRef) (bool, error) { return p == pin, nil },
		RecoveryRoots: func(context.Context) ([]s.PackRef, error) { return []s.PackRef{recovery}, nil },
	}
	got, e := m.Collect(t.Context())
	if e != nil {
		t.Fatal(e)
	}
	for p, want := range map[s.PackRef]RootReason{live: LiveHead, part: UnfinishedUpload, pin: ReadOrWriterPin, recovery: RecoveryCut, pending: UnfinishedUpload} {
		if got.Roots[p] != want {
			t.Errorf("root %v = %v, want %v", p, got.Roots[p], want)
		}
	}
	if got.Refcounts[a] != 2 || got.Refcounts[b] != 0 || got.LiveBytes[live] != 4 || got.LiveBytes[part] != 0 {
		t.Fatal(got.Refcounts, got.LiveBytes)
	}
	m.RecoveryRoots = func(context.Context) ([]s.PackRef, error) { return nil, s.ErrUnavailable }
	if report, e := m.Collect(t.Context()); report != nil || !errors.Is(e, s.ErrUnavailable) {
		t.Fatalf("incomplete proof: %v, %v", report, e)
	}
}
