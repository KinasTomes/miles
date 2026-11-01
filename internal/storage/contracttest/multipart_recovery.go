package contracttest

import (
	"bytes"
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/manifest"
	mp "github.com/KinasTomes/miles/internal/multipart/core"
	s "github.com/KinasTomes/miles/internal/storage"
	"strings"
	"testing"
	"time"
)

func RunMultipartRecovery(t *testing.T, factory MultipartFactory) {
	for _, scenario := range []string{"abort-barrier", "completion-grace", "expired-completion"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			f := NewFixture()
			f.Lease.ExpiresAt = f.Now.Add(72 * time.Hour)
			h := factory(t, f)
			m := h.Store
			recovery := m.(s.MultipartRecovery)
			roots := m.(s.PublisherProtection)
			r := s.MultipartRequest{UploadID: strings.Repeat("e", 32), Credential: "writer-key", OpID: fixtureOpID("create-recovery"), Writer: f.Lease.Fence}
			u, err := m.CreateUpload(ctx, s.CreateUploadRequest{Request: r, Upload: s.MultipartUpload{ID: r.UploadID, Object: f.Request("recover", 0).Head.Object, ReservationID: "recover-ledger", ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT"}})
			if err != nil {
				t.Fatal(err)
			}
			p := s.PartPublisher{ID: strings.Repeat("f", 32), UploadID: u.ID, OpID: fixtureOpID("pending"), ManifestID: "pending-manifest", PartNumber: 1, Bytes: 4, Deadline: f.Now.Add(time.Minute)}
			if _, err = m.RegisterPartPublisher(ctx, r, p); err != nil {
				t.Fatal(err)
			}
			pack := f.Chunk("default").Location.Pack
			if err = roots.RetainPublisherPacks(ctx, r, p.ID, []s.PackRef{pack}); err != nil {
				t.Fatal(err)
			}
			if retry, err := m.RegisterPartPublisher(ctx, r, p); err != nil || len(retry.Packs) != 1 {
				t.Fatal("register retry lost retained roots", retry, err)
			}
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			unknown, err := recovery.ResolvePartPublisher(cancelled, r, p.ID)
			if err == nil || unknown.Disposition != s.RecoveryUnknown {
				t.Fatal("cancellation became proof", unknown, err)
			}
			members, err := roots.ListPublishers(ctx, u.ID, "", 10)
			if err != nil || len(members.Publishers) != 1 || len(members.Publishers[0].Packs) != 1 || members.Publishers[0].State != "Open" {
				t.Fatal("unknown lost membership", members, err)
			}
			u, err = m.GetUpload(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			r.ExpectedRevision = u.Revision
			if scenario == "abort-barrier" {
				u, err = m.AbortUpload(ctx, r)
				if err != nil || u.State != "Aborting" || u.Publishers != 1 {
					t.Fatal(u, err)
				}
				q, err := h.Metadata.GetQuota(ctx, "owner")
				if err != nil || q.ActiveUploads != 1 || q.Staged != 4 {
					t.Fatal("released pending quota", q, err)
				}
			} else {
				in := mp.Input{V: 1, BucketID: u.Object.BucketID, Key: u.Object.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType, Parts: []mp.Part{{PartNumber: 1, ETag: strings.Repeat("a", 32)}}}
				owner := strings.Repeat("a", 32)
				d, err := manifest.WriteCompletionStream(3, owner, bytes.NewReader(in.Canonical()), func(n uint32, b []byte) error { return m.WriteCompletionPage(ctx, 3, owner, n, b, r.Writer) })
				if err != nil {
					t.Fatal(err)
				}
				r.OpID = fixtureOpID("complete-recovery")
				u, err = m.FreezeCompletion(ctx, s.FreezeRequest{Request: r, Input: d})
				if err != nil {
					t.Fatal(err)
				}
				r.ExpectedRevision, r.Fence = u.Revision, u.Fence
				if _, err = m.AbortUpload(ctx, r); !errors.Is(err, s.ErrConflict) {
					t.Fatal("public Abort interrupted Complete", err)
				}
				h.SetTime(u.LeaseUntil.Add(5*time.Second - time.Nanosecond))
				if result, err := recovery.ResolveCompletion(ctx, r); !errors.Is(err, s.ErrConflict) || result.Disposition != s.RecoveryUnknown {
					t.Fatal("grace became proof", result, err)
				}
				// Takeover starts after lease+grace and is not stopped by
				// business expiry: the frozen attempt resumes past expires_at.
				at := u.LeaseUntil.Add(5 * time.Second)
				if scenario == "expired-completion" {
					at = u.ExpiresAt
				}
				h.SetTime(at)
				old := r
				previous := u
				u, err = m.ClaimCompletion(ctx, r)
				if err != nil || u.Fence != previous.Fence+1 || u.Input != previous.Input || u.CompletionOp != previous.CompletionOp {
					t.Fatal("takeover changed frozen identity", u, err)
				}
				if _, err = m.ReopenUpload(ctx, old); !errors.Is(err, s.ErrConflict) {
					t.Fatal("stale worker changed new attempt", err)
				}
				r.ExpectedRevision, r.Fence = u.Revision, u.Fence
				u, err = m.RenewCompletion(ctx, r)
				if err != nil {
					t.Fatal("takeover renewal", err)
				}
				r.ExpectedRevision = u.Revision
				result, err := recovery.ResolveCompletion(ctx, r)
				if err != nil || result.Disposition != s.RecoveryClosed || result.Upload.State != "Aborting" || result.Upload.ClosedFence != u.Fence || result.Upload.ClosedWriter != r.Writer {
					t.Fatal(result, err)
				}
				u = result.Upload
			}
			p.ID = strings.Repeat("d", 32)
			p.OpID = fixtureOpID("new-blocked")
			if _, err = m.RegisterPartPublisher(ctx, r, p); err == nil {
				t.Fatal("barrier admitted publisher")
			}
			p.ID = strings.Repeat("f", 32)
			resolved, err := recovery.ResolvePartPublisher(ctx, r, p.ID)
			if err != nil || resolved.Disposition != s.RecoveryClosed || len(resolved.Publisher.Packs) != 1 {
				t.Fatal(resolved, err)
			}
			if retry, err := recovery.ResolvePartPublisher(ctx, r, p.ID); err != nil || retry.Disposition != s.RecoveryClosed {
				t.Fatal(retry, err)
			}
			u, err = m.GetUpload(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			r.ExpectedRevision, r.Fence = u.Revision, u.Fence
			result, err := recovery.ResolveCompletion(ctx, r)
			if err != nil || result.Disposition != s.RecoveryClosed || result.Upload.State != "Aborted" {
				t.Fatal(result, err)
			}
			if _, err = recovery.ResolveCompletion(ctx, r); err != nil {
				t.Fatal("abort retry", err)
			}
			q, err := h.Metadata.GetQuota(ctx, "owner")
			if err != nil || q.ActiveUploads != 0 || q.Staged != 0 || q.Reserved != 0 {
				t.Fatal("abort double counted", q, err)
			}
		})
	}
}
