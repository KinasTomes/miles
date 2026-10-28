package dynamodb

import (
	"bytes"
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/manifest"
	mp "github.com/KinasTomes/miles/internal/multipart/core"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"strings"
	"testing"
	"time"
)

func TestLocalMultipartClosureLostACK(t *testing.T) {
	for _, after := range []bool{false, true} {
		name := "before-send"
		if after {
			name = "after-commit"
		}
		t.Run(name, func(t *testing.T) {
			f := contracttest.NewFixture()
			f.Lease.ExpiresAt = f.Now.Add(time.Hour)
			m, _ := fixtureStore(t, f)
			ctx := context.Background()
			r := s.MultipartRequest{UploadID: strings.Repeat("a", 32), Credential: "writer-key", OpID: strings.Repeat("b", 32), Writer: f.Lease.Fence}
			u, err := m.CreateUpload(ctx, s.CreateUploadRequest{Request: r, Upload: s.MultipartUpload{ID: r.UploadID, Object: f.Request("close-multipart", 0).Head.Object, ReservationID: "multipart-close", ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT"}})
			if err != nil {
				t.Fatal(err)
			}
			in := mp.Input{V: 1, BucketID: u.Object.BucketID, Key: u.Object.Key, ChecksumAlgorithm: u.ChecksumAlgorithm, ChecksumType: u.ChecksumType, Parts: []mp.Part{{PartNumber: 1, ETag: strings.Repeat("a", 32)}}}
			owner := strings.Repeat("d", 32)
			d, err := manifest.WriteCompletionStream(3, owner, bytes.NewReader(in.Canonical()), func(n uint32, b []byte) error { return m.WriteCompletionPage(ctx, 3, owner, n, b, r.Writer) })
			if err != nil {
				t.Fatal(err)
			}
			r.OpID = strings.Repeat("c", 32)
			r.ExpectedRevision = u.Revision
			u, err = m.FreezeCompletion(ctx, s.FreezeRequest{Request: r, Input: d})
			if err != nil {
				t.Fatal(err)
			}
			r.ExpectedRevision, r.Fence = u.Revision, u.Fence
			fault := &closeFault{API: m.api, before: !after, after: after}
			m.api = fault
			result, err := m.ResolveCompletion(ctx, r)
			if !errors.Is(err, s.ErrUnavailable) || result.Disposition != s.RecoveryUnknown {
				t.Fatal("unacknowledged close became proof", result, err)
			}
			observed, err := m.GetUpload(ctx, u.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after && observed.State != "Aborted" || !after && observed.State != "Completing" {
				t.Fatal("non-atomic closure", observed)
			}
			fault.before = false
			result, err = m.ResolveCompletion(ctx, r)
			if err != nil || result.Disposition != s.RecoveryClosed || result.Upload.State != "Aborted" {
				t.Fatal(result, err)
			}
			q, err := m.GetQuota(ctx, "owner")
			if err != nil || q.ActiveUploads != 0 || q.Reserved != 0 || q.Staged != 0 {
				t.Fatal(q, err)
			}
		})
	}
}
