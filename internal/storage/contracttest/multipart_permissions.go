package contracttest

import (
	"context"
	"strings"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

// runPartPermissions pins API v2 2.2.3: ListParts is (read AND initiator) OR
// owner OR admin, while the part revision lookup of UploadPart (GetPart) is
// write AND initiator, so neither right stands in for the other.
func runPartPermissions(t *testing.T, factory MultipartFactory) {
	t.Run("list-parts-read-initiator-get-part-write-initiator", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		f.Quota.Limit = 1 << 20
		h := factory(t, f)
		m := h.Store
		r := s.MultipartRequest{UploadID: strings.Repeat("e", 32), Credential: "writer-key", OpID: fixtureOpID("perm-create"), Writer: f.Lease.Fence}
		u, err := m.CreateUpload(ctx, s.CreateUploadRequest{Request: r, Upload: s.MultipartUpload{ID: r.UploadID, Object: f.Request("perm", 0).Head.Object, ReservationID: "perm-ledger", ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT"}})
		if err != nil {
			t.Fatal(err)
		}
		c := f.Chunk("default")
		if _, _, err = h.Metadata.PutChunkIfAbsent(ctx, c, r.Writer); err != nil {
			t.Fatal(err)
		}
		page := s.ManifestPage{Version: 1, ManifestID: "perm-manifest", Domain: "default", Chunks: []s.ChunkRef{{Key: c.Key, RawBytes: 4}}}
		if err = h.Metadata.(s.PartPageStore).WritePartPages(ctx, []s.ManifestPage{page}, r.Writer); err != nil {
			t.Fatal(err)
		}
		pub := s.PartPublisher{ID: strings.Repeat("f", 32), UploadID: u.ID, OpID: fixtureOpID("perm-part"), ManifestID: page.ManifestID, PartNumber: 1, Bytes: 4, Deadline: f.Now.Add(time.Minute)}
		if _, err = m.RegisterPartPublisher(ctx, r, pub); err != nil {
			t.Fatal(err)
		}
		part := s.UploadPart{Version: 1, Number: 1, Size: 4, ManifestID: page.ManifestID, Pages: 1, ETag: strings.Repeat("a", 32), Checksum: s.Checksum{Algorithm: "CRC32C", Type: "FULL_OBJECT", Value: "AAAAAA=="}}
		if _, err = m.PublishPart(ctx, s.PartPublishRequest{Request: r, PublisherID: pub.ID, Part: part}); err != nil {
			t.Fatal(err)
		}
		list := func(credential string) error {
			_, err := m.ListParts(ctx, s.MultipartRequest{UploadID: u.ID, Credential: credential}, 0, 10)
			return err
		}
		lookup := func() error {
			got, err := m.GetPart(ctx, s.MultipartRequest{UploadID: u.ID, Credential: "writer-key"}, 1)
			if err == nil && got.Revision != 1 {
				t.Fatal("GetPart revision", got)
			}
			return err
		}
		// Write-only initiator (the fixture grant): UploadPart's lookup, no listing.
		requireError(t, list("writer-key"), s.ErrAccessDenied)
		if err := lookup(); err != nil {
			t.Fatal("write-only initiator part lookup:", err)
		}
		_, err = m.GetPart(ctx, s.MultipartRequest{UploadID: u.ID, Credential: "writer-key"}, 2)
		requireError(t, err, s.ErrNotFound)
		for _, who := range []string{"owner-key", "admin-key"} {
			if err := list(who); err != nil {
				t.Fatal(who, "ListParts:", err)
			}
		}
		// Read-only initiator: listing, no UploadPart lookup.
		h.SetGrant(s.Grant{BucketID: f.Bucket.ID, UserID: "writer", Read: true, Revision: 2})
		if err := list("writer-key"); err != nil {
			t.Fatal("read-only initiator ListParts:", err)
		}
		requireError(t, lookup(), s.ErrAccessDenied)
		requireError(t, func() error {
			_, err := m.GetPart(ctx, s.MultipartRequest{UploadID: u.ID, Credential: "owner-key"}, 1)
			return err
		}(), s.ErrAccessDenied)
	})
}
