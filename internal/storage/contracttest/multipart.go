package contracttest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	multipart "github.com/KinasTomes/miles/internal/multipart/core"
	s "github.com/KinasTomes/miles/internal/storage"
)

type MultipartHarness struct {
	SetStrict func(bool)
	Metadata  s.MetadataStore
	Store     s.MultipartStore
	SetTime   func(time.Time)
}
type MultipartFactory func(*testing.T, Fixture) MultipartHarness

func RunMultipart(t *testing.T, factory MultipartFactory) {
	for _, reopen := range []bool{false, true} {
		name := "complete-subset-and-receipt"
		if reopen {
			name = "freeze-cancel-reopen"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := NewFixture()
			f.Quota.Limit = 1 << 20
			f.Lease.ExpiresAt = f.Now.Add(72 * time.Hour)
			h := factory(t, f)
			m := h.Store
			base := f.Request("complete", 0)
			uid := strings.Repeat("1", 32)
			r := s.MultipartRequest{UploadID: uid, Credential: "writer-key", OpID: fixtureOpID("create"), Writer: f.Lease.Fence}
			u, err := m.CreateUpload(ctx, s.CreateUploadRequest{Request: r, Upload: s.MultipartUpload{ID: uid, Object: base.Head.Object, ReservationID: "upload-reservation", ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT", Metadata: map[string]string{"content-type": "application/octet-stream"}}})
			if err != nil {
				t.Fatal(err)
			}
			if u.Owner != "owner" || u.Initiator != "writer" || u.Revision != 1 {
				t.Fatal(u)
			}
			if _, _, err := h.Metadata.PutChunkIfAbsent(ctx, f.Chunk("default"), f.Lease.Fence); err != nil {
				t.Fatal(err)
			}
			var parts []s.UploadPart
			for n := 1; n <= 2; n++ {
				pid := fmt.Sprintf("%032x", n+1)
				manifestID := fmt.Sprintf("%032x", n+10)
				if err := h.Metadata.(s.PartPageStore).WritePartPages(ctx, []s.ManifestPage{{Version: 1, ManifestID: manifestID, Domain: "default", Chunks: []s.ChunkRef{{Key: f.Chunk("default").Key, RawBytes: 4}}}}, r.Writer); err != nil {
					t.Fatal(err)
				}
				pub := s.PartPublisher{ID: pid, UploadID: uid, OpID: fixtureOpID(fmt.Sprint("part-", n)), PartNumber: uint32(n), Bytes: 4, ManifestID: manifestID, Deadline: f.Now.Add(time.Minute)}
				if _, err := m.RegisterPartPublisher(ctx, r, pub); err != nil {
					t.Fatal(err)
				}
				p := s.UploadPart{Version: 1, Number: uint32(n), Size: 4, ManifestID: manifestID, Pages: 1, ETag: strings.Repeat("a", 32), Checksum: s.Checksum{Algorithm: "CRC32C", Type: "FULL_OBJECT", Value: "AAAAAA=="}}
				if reopen && n == 2 {
					if err := m.CancelPartPublisher(ctx, r, pid); err != nil {
						t.Fatal(err)
					}
					break
				}
				published, err := m.PublishPart(ctx, s.PartPublishRequest{Request: r, PublisherID: pid, Part: p})
				if err != nil {
					t.Fatal(err)
				}
				parts = append(parts, published)
			}
			u, err = m.GetUpload(ctx, uid)
			if err != nil {
				t.Fatal(err)
			}
			selected := parts[len(parts)-1]
			input := multipart.Input{V: 1, BucketID: u.Object.BucketID, Key: u.Object.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType, Parts: []multipart.Part{{PartNumber: int(selected.Number), ETag: selected.ETag}}}
			write := func(kind byte, owner string, body []byte) s.StreamDescriptor {
				t.Helper()
				d, err := manifest.WriteCompletionStream(kind, owner, bytes.NewReader(body), func(n uint32, b []byte) error { return m.WriteCompletionPage(ctx, kind, owner, n, b, r.Writer) })
				if err != nil {
					t.Fatal(err)
				}
				return d
			}
			in := write(3, strings.Repeat("a", 32), input.Canonical())
			r.OpID = base.Head.OpID
			r.ExpectedRevision = u.Revision
			u, err = m.FreezeCompletion(ctx, s.FreezeRequest{Request: r, Input: in})
			if err != nil {
				t.Fatal(err)
			}
			r.ExpectedRevision = u.Revision
			r.Fence = u.Fence
			if reopen {
				u, err = m.ReopenUpload(ctx, r)
				if err != nil {
					t.Fatal(err)
				}
				r.ExpectedRevision = u.Revision
				in = write(3, strings.Repeat("b", 32), input.Canonical())
				u, err = m.FreezeCompletion(ctx, s.FreezeRequest{Request: r, Input: in})
				if err != nil {
					t.Fatal("new attempt under same completion op failed", err)
				}
				r.ExpectedRevision = u.Revision
				r.Fence = u.Fence
			}
			snap, err := manifest.EncodeSnapshot([]manifest.SnapshotPart{{Number: selected.Number, Revision: selected.Revision, Size: uint64(selected.Size), Pages: selected.Pages, ETag: selected.ETag, ManifestID: selected.ManifestID, Checksum: selected.Checksum.Value}})
			if err != nil {
				t.Fatal(err)
			}
			d := write(4, strings.Repeat("c", 32), snap)
			u, err = m.PrepareCompletion(ctx, s.PrepareCompletionRequest{Request: r, Snapshot: d, Head: s.ObjectHead{Object: u.Object, ManifestID: "prepared-final"}})
			if err != nil {
				t.Fatal(err)
			}
			ledger, err := h.Metadata.GetReservation(ctx, u.ReservationID)
			if err != nil {
				t.Fatal(err)
			}
			base.Head = u.Prepared
			base.ReservationID = u.ReservationID
			base.ReservationRevision = ledger.Revision
			base.MultipartAttempt = u.Fence
			base.Completion = &s.Completion{UploadID: u.ID, BucketID: u.Object.BucketID, Key: u.Object.Key, Initiator: u.Initiator, Revision: u.Revision, Fence: u.Fence, State: "Completing"}
			base.Receipt = &s.CompletedReceipt{Version: 1, UploadID: u.ID, BucketID: u.Object.BucketID, Key: u.Object.Key, Initiator: u.Initiator, OpID: u.CompletionOp, InputDigest: u.Input.SHA256, ManifestID: u.Prepared.ManifestID, ETag: u.Prepared.ETag, Checksum: u.Prepared.Checksum, ExpiresAt: f.Now.Add(7 * 24 * time.Hour)}
			result, err := h.Metadata.CommitObject(ctx, base)
			if err != nil {
				t.Fatal(err)
			}
			if result.Head.Size != 4 {
				t.Fatal(result)
			}
			if _, err := h.Metadata.CommitObject(ctx, base); err != nil {
				t.Fatal("complete retry", err)
			}
			q, err := h.Metadata.GetQuota(ctx, "owner")
			if err != nil || q.Used != 4 || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
				t.Fatal(q, err)
			}
			r.Fence = 0
			receipt, err := m.GetMultipartReceipt(ctx, r, input.Digest())
			if err != nil || receipt.Head.Revision != 1 || receipt.SnapshotDigest != d.SHA256 {
				t.Fatal(receipt, err)
			}
			h.SetTime(f.Now.Add(48 * time.Hour))
			again, err := m.GetMultipartReceipt(ctx, r, input.Digest())
			if err != nil || again.ExpiresAt != receipt.ExpiresAt {
				t.Fatal("receipt incorrectly uses Active/writer expiry", again, err)
			}
			h.SetTime(receipt.ExpiresAt)
			if _, err = m.GetMultipartReceipt(ctx, r, input.Digest()); !errors.Is(err, s.ErrNotFound) {
				t.Fatal("receipt equality must expire", err)
			}
		})
	}
}
