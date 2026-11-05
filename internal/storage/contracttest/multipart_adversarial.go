package contracttest

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	multipart "github.com/KinasTomes/miles/internal/multipart/core"
	s "github.com/KinasTomes/miles/internal/storage"
)

// runMultipartAdversarial pins the accounting of API v2 2.6 and 4.3 under
// a revoked right, a full staging allowance and Abort/Complete in either
// order: a refused write changes no counter and a terminal state releases
// the upload once.
func runMultipartAdversarial(t *testing.T, factory MultipartFactory) {
	create := func(t *testing.T, h MultipartHarness, f Fixture, label string) (s.MultipartRequest, s.MultipartUpload) {
		t.Helper()
		ctx := context.Background()
		r := s.MultipartRequest{UploadID: fixtureOpID(label + "-upload"), Credential: "writer-key", OpID: fixtureOpID(label + "-create"), Writer: f.Lease.Fence}
		u, err := h.Store.CreateUpload(ctx, s.CreateUploadRequest{Request: r, Upload: s.MultipartUpload{ID: r.UploadID, Object: f.Request(label, 0).Head.Object, ReservationID: label + "-ledger", ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT"}})
		if err != nil {
			t.Fatal(err)
		}
		r.ExpectedRevision = u.Revision
		return r, u
	}
	quota := func(t *testing.T, h MultipartHarness) s.LogicalQuota {
		t.Helper()
		q, err := h.Metadata.GetQuota(context.Background(), "owner")
		if err != nil {
			t.Fatal(err)
		}
		return q
	}
	t.Run("revoked-initiator-before-publish", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		f.Quota.Limit = 1 << 20
		h := factory(t, f)
		r, u := create(t, h, f, "revoked")
		c := f.Chunk("default")
		if _, _, err := h.Metadata.PutChunkIfAbsent(ctx, c, r.Writer); err != nil {
			t.Fatal(err)
		}
		page := s.ManifestPage{Version: 1, ManifestID: "revoked-manifest", Domain: "default", Chunks: []s.ChunkRef{{Key: c.Key, RawBytes: 4}}}
		if err := h.Metadata.(s.PartPageStore).WritePartPages(ctx, []s.ManifestPage{page}, r.Writer); err != nil {
			t.Fatal(err)
		}
		pub := s.PartPublisher{ID: fixtureOpID("revoked-publisher"), UploadID: u.ID, OpID: fixtureOpID("revoked-part"), ManifestID: page.ManifestID, PartNumber: 1, Bytes: 4, Deadline: f.Now.Add(time.Minute)}
		if _, err := h.Store.RegisterPartPublisher(ctx, r, pub); err != nil {
			t.Fatal(err)
		}
		staged := quota(t, h)
		if staged.Staged != 4 {
			t.Fatal("provisional bytes not charged at registration", staged)
		}
		// The bucket owner takes the initiator's write right away between
		// the body and its publication.
		h.SetGrant(s.Grant{BucketID: f.Bucket.ID, UserID: "writer", Read: true, Revision: 2})
		part := s.UploadPart{Version: 1, Number: 1, Size: 4, ManifestID: page.ManifestID, Pages: 1, ETag: strings.Repeat("a", 32), Checksum: s.Checksum{Algorithm: "CRC32C", Type: "FULL_OBJECT", Value: "AAAAAA=="}}
		_, err := h.Store.PublishPart(ctx, s.PartPublishRequest{Request: r, PublisherID: pub.ID, Part: part})
		requireError(t, err, s.ErrAccessDenied)
		other := pub
		other.ID, other.OpID, other.PartNumber = fixtureOpID("revoked-publisher-2"), fixtureOpID("revoked-part-2"), 2
		_, err = h.Store.RegisterPartPublisher(ctx, r, other)
		requireError(t, err, s.ErrAccessDenied)
		if q := quota(t, h); q != staged {
			t.Fatal("a refused write changed accounting", q, staged)
		}
		// The request that owns the publisher closes it; nothing stays charged.
		if err = h.Store.CancelPartPublisher(ctx, r, pub.ID); err != nil {
			t.Fatal(err)
		}
		if q := quota(t, h); q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 1 {
			t.Fatal("cancelled publisher kept its bytes", q)
		}
		got, err := h.Store.GetUpload(ctx, u.ID)
		if err != nil || got.State != "Active" || got.Publishers != 0 || got.ProvisionalBytes != 0 || got.PublishedBytes != 0 {
			t.Fatal(got, err)
		}
		h.SetGrant(s.Grant{BucketID: f.Bucket.ID, UserID: "writer", Write: true, Revision: 3})
		_, err = h.Store.GetPart(ctx, s.MultipartRequest{UploadID: u.ID, Credential: "writer-key"}, 1)
		requireError(t, err, s.ErrNotFound)
	})
	t.Run("staging-cap", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		f.Quota.Limit = 1 << 20
		// Other uploads of the owner already stage up to 4 bytes below the
		// logical limit plus 100 GiB of headroom.
		f.Quota.Staged = f.Quota.Limit + s.MaxObjectBytes - 4
		h := factory(t, f)
		r, u := create(t, h, f, "staging")
		before := quota(t, h)
		pub := s.PartPublisher{ID: fixtureOpID("staging-publisher"), UploadID: u.ID, OpID: fixtureOpID("staging-part"), ManifestID: "staging-manifest", PartNumber: 1, Bytes: 5, Deadline: f.Now.Add(time.Minute)}
		_, err := h.Store.RegisterPartPublisher(ctx, r, pub)
		requireError(t, err, s.ErrQuotaExceeded)
		if q := quota(t, h); q != before {
			t.Fatal("refused registration changed accounting", q, before)
		}
		if got, err := h.Store.GetUpload(ctx, u.ID); err != nil || got.Revision != u.Revision || got.Publishers != 0 || got.ProvisionalBytes != 0 {
			t.Fatal(got, err)
		}
		pub.Bytes = 4
		if _, err = h.Store.RegisterPartPublisher(ctx, r, pub); err != nil {
			t.Fatal("registration up to the staging cap", err)
		}
		if q := quota(t, h); q.Staged != f.Quota.Limit+s.MaxObjectBytes || q.Reserved != 4 {
			t.Fatal(q)
		}
	})
	t.Run("abort-and-complete-in-either-order", func(t *testing.T) {
		ctx := context.Background()
		f := NewFixture()
		f.Quota.Limit = 1 << 20
		h := factory(t, f)
		freeze := func(r s.MultipartRequest, u s.MultipartUpload, owner string) (s.MultipartUpload, error) {
			in := multipart.Input{V: 1, BucketID: u.Object.BucketID, Key: u.Object.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType, Parts: []multipart.Part{{PartNumber: 1, ETag: strings.Repeat("a", 32)}}}
			d, err := manifest.WriteCompletionStream(manifest.CompletionInput, owner, bytes.NewReader(in.Canonical()), func(n uint32, b []byte) error {
				return h.Store.WriteCompletionPage(ctx, manifest.CompletionInput, owner, n, b, r.Writer)
			})
			if err != nil {
				t.Fatal(err)
			}
			r.OpID = fixtureOpID(owner + "-complete")
			return h.Store.FreezeCompletion(ctx, s.FreezeRequest{Request: r, Input: d})
		}

		// Complete froze first: Abort conflicts and releases nothing.
		r, u := create(t, h, f, "complete-first")
		frozen, err := freeze(r, u, strings.Repeat("c", 32))
		if err != nil {
			t.Fatal(err)
		}
		held := quota(t, h)
		r.ExpectedRevision = frozen.Revision
		_, err = h.Store.AbortUpload(ctx, r)
		requireError(t, err, s.ErrConflict)
		got, err := h.Store.GetUpload(ctx, u.ID)
		if err != nil || got.State != "Completing" || got.Revision != frozen.Revision || quota(t, h) != held {
			t.Fatal("abort interrupted Complete", got, err)
		}
		if ledger, err := h.Metadata.GetReservation(ctx, u.ReservationID); err != nil || ledger.State != s.ReservationActive {
			t.Fatal("abort released the completing reservation", ledger, err)
		}

		// Abort landed first: a Complete read before it cannot freeze, and
		// the slot is released once.
		r, u = create(t, h, f, "abort-first")
		if got, err = h.Store.AbortUpload(ctx, r); err != nil || got.State != "Aborted" {
			t.Fatal(got, err)
		}
		released := quota(t, h)
		if released.ActiveUploads != 1 {
			t.Fatal("abort did not release its slot", released)
		}
		if _, err = freeze(r, u, strings.Repeat("d", 32)); !errors.Is(err, s.ErrConflict) {
			t.Fatal("Complete on the record read before Abort", err)
		}
		r.ExpectedRevision = got.Revision
		if _, err = freeze(r, u, strings.Repeat("e", 32)); !errors.Is(err, s.ErrFenced) {
			t.Fatal("Complete on an Aborted upload", err)
		}
		if again, err := h.Store.AbortUpload(ctx, r); err != nil || again.State != "Aborted" || quota(t, h) != released {
			t.Fatal("repeated abort released twice", again, err, quota(t, h))
		}
	})
}
