package contracttest

import (
	"context"
	"errors"
	"fmt"
	s "github.com/KinasTomes/miles/internal/storage"
	"strings"
	"testing"
	"time"
)

func RunMultipartRaces(t *testing.T, factory MultipartFactory) {
	t.Run("sixteen-upload-owner-bound", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		h := factory(t, f)
		for n := 1; n <= 17; n++ {
			id := fmt.Sprintf("%032x", n)
			r := s.MultipartRequest{UploadID: id, Credential: "writer-key", OpID: fixtureOpID(id), Writer: f.Lease.Fence}
			_, err := h.Store.CreateUpload(ctx, s.CreateUploadRequest{Request: r, Upload: s.MultipartUpload{ID: id, Object: f.Request(id, 0).Head.Object, ReservationID: "upload-" + id, ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT"}})
			if n <= 16 && err != nil || n == 17 && !errors.Is(err, s.ErrQuotaExceeded) {
				t.Fatal(n, err)
			}
		}
		q, err := h.Metadata.GetQuota(ctx, "owner")
		if err != nil || q.ActiveUploads != 16 {
			t.Fatal(q, err)
		}
	})
	t.Run("abort-races-publication", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		h := factory(t, f)
		m := h.Store
		r := s.MultipartRequest{UploadID: strings.Repeat("a", 32), Credential: "writer-key", OpID: fixtureOpID("race-create"), Writer: f.Lease.Fence}
		u, err := m.CreateUpload(ctx, s.CreateUploadRequest{Request: r, Upload: s.MultipartUpload{ID: r.UploadID, Object: f.Request("race", 0).Head.Object, ReservationID: "race-ledger", ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT"}})
		if err != nil {
			t.Fatal(err)
		}
		c := f.Chunk("default")
		if _, _, err = h.Metadata.PutChunkIfAbsent(ctx, c, r.Writer); err != nil {
			t.Fatal(err)
		}
		page := s.ManifestPage{Version: 1, ManifestID: "race-manifest", Domain: "default", Chunks: []s.ChunkRef{{Key: c.Key, RawBytes: 4}}}
		if err = h.Metadata.(s.PartPageStore).WritePartPages(ctx, []s.ManifestPage{page}, r.Writer); err != nil {
			t.Fatal(err)
		}
		p := s.PartPublisher{ID: strings.Repeat("b", 32), UploadID: u.ID, OpID: fixtureOpID("race-part"), ManifestID: page.ManifestID, PartNumber: 1, Bytes: 4, Deadline: f.Now.Add(time.Minute)}
		if _, err = m.RegisterPartPublisher(ctx, r, p); err != nil {
			t.Fatal(err)
		}
		u, err = m.GetUpload(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		r.ExpectedRevision = u.Revision
		start := make(chan struct{})
		published := make(chan error, 1)
		aborted := make(chan error, 1)
		part := s.UploadPart{Version: 1, Number: 1, Size: 4, ManifestID: page.ManifestID, Pages: 1, ETag: strings.Repeat("a", 32), Checksum: s.Checksum{Algorithm: "CRC32C", Type: "FULL_OBJECT", Value: "AAAAAA=="}}
		go func() {
			<-start
			_, e := m.PublishPart(ctx, s.PartPublishRequest{Request: r, PublisherID: p.ID, Part: part})
			published <- e
		}()
		go func() { <-start; _, e := m.AbortUpload(ctx, r); aborted <- e }()
		close(start)
		pubErr, abortErr := <-published, <-aborted
		if pubErr != nil && !errors.Is(pubErr, s.ErrFenced) && !errors.Is(pubErr, s.ErrConflict) {
			t.Fatal(pubErr)
		}
		if abortErr != nil && !errors.Is(abortErr, s.ErrConflict) {
			t.Fatal(abortErr)
		}
		u, err = m.GetUpload(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		r.ExpectedRevision = u.Revision
		if u.State == "Active" {
			u, err = m.AbortUpload(ctx, r)
			if err != nil {
				t.Fatal(err)
			}
		}
		if u.State == "Aborting" {
			if _, err = m.(s.MultipartRecovery).ResolvePartPublisher(ctx, r, p.ID); err != nil {
				t.Fatal(err)
			}
			u, err = m.GetUpload(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			r.ExpectedRevision = u.Revision
			result, e := m.(s.MultipartRecovery).ResolveCompletion(ctx, r)
			if e != nil {
				t.Fatal(e)
			}
			u = result.Upload
		}
		q, err := h.Metadata.GetQuota(ctx, "owner")
		if err != nil || u.State != "Aborted" || q.ActiveUploads != 0 || q.Reserved != 0 || q.Staged != 0 {
			t.Fatal("race leaked or double-released accounting", u, q, err)
		}
	})
}
