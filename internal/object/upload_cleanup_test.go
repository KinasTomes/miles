package object

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

func expireFixtureUpload(t *testing.T, f *serviceFixture, u storage.MultipartUpload) {
	t.Helper()
	f.advance(25 * time.Hour)
	if e := f.options.Clock.(*clock.Virtual).SetWall(u.ExpiresAt.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
}
func TestPurgeExpiredUploadReleasesStagingOnceAndKeepsSharedPacks(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	shared := random(t, 24<<10)
	f.put(t, ctx, "shared", shared)
	u := f.createUpload(t, ctx, "expired")
	if _, e := f.uploadPart(ctx, u, 1, shared); e != nil {
		t.Fatal(e)
	}
	before := f.quota(t, ctx)
	if _, e := f.service.PurgeExpiredUpload(ctx, u.ID); !errors.Is(e, storage.ErrConflict) || f.quota(t, ctx) != before {
		t.Fatal("unexpired upload purged", e)
	}
	expireFixtureUpload(t, f, u)
	for range 2 {
		out, e := f.service.PurgeExpiredUpload(ctx, u.ID)
		if e != nil || out.State != "Aborted" {
			t.Fatal(out, e)
		}
		q := f.quota(t, ctx)
		if q.Used != int64(len(shared)) || q.Staged != 0 || q.Reserved != 0 || q.ActiveUploads != 0 {
			t.Fatal(q)
		}
	}
	if _, got := f.get(t, ctx, "shared", nil); !bytes.Equal(got, shared) {
		t.Fatal("cleanup removed shared committed data")
	}
}
func TestPurgeExpiredUploadClosesOpenPublisher(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	u := f.createUpload(t, ctx, "crashed-part")
	admission, e := f.options.AdmitWriter(ctx, WriterRequest{})
	if e != nil {
		t.Fatal(e)
	}
	req := storage.MultipartRequest{UploadID: u.ID, Credential: "writer-key", OpID: "0123456789abcdef0123456789abcdef", Writer: admission.Fence}
	p := storage.PartPublisher{ID: "fedcba9876543210fedcba9876543210", UploadID: u.ID, OpID: req.OpID, ManifestID: "0011223344556677889900aabbccddee", PartNumber: 1, Bytes: 4096, Deadline: u.ExpiresAt.Add(-time.Hour)}
	if _, e = f.metadata.RegisterPartPublisher(ctx, req, p); e != nil {
		t.Fatal(e)
	}
	expireFixtureUpload(t, f, u)
	out, e := f.service.PurgeExpiredUpload(ctx, u.ID)
	if e != nil || out.State != "Aborted" {
		t.Fatal(out, e)
	}
	if f.openPublishers(t, ctx, u.ID) != 0 || f.quota(t, ctx).Staged != 0 {
		t.Fatal("publisher staging remained live")
	}
}
func TestPurgeDoesNotAbortExpiredCompletingUpload(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 16*mib)
	u, parts := f.stage(t, ctx, "completing", "CRC64NVME", "FULL_OBJECT", map[uint32][]byte{1: random(t, 128<<10)})
	f.refuse = func(op string) error {
		if strings.HasPrefix(op, "prepare/") {
			return storage.ErrUnavailable
		}
		return nil
	}
	if _, e := f.complete(ctx, u.ID, input(u, parts, false, 1)); e == nil {
		t.Fatal("fault did not stop completion")
	}
	f.refuse = nil
	expireFixtureUpload(t, f, u)
	q := f.quota(t, ctx)
	if _, e := f.service.PurgeExpiredUpload(ctx, u.ID); !errors.Is(e, storage.ErrConflict) {
		t.Fatal(e)
	}
	if f.state(t, ctx, u.ID).State != "Completing" || f.quota(t, ctx) != q {
		t.Fatal("expiry discarded a completing upload")
	}
	if _, e := f.service.RecoverUpload(ctx, u.ID); e != nil {
		t.Fatal(e)
	}
	if f.state(t, ctx, u.ID).State != "Completed" {
		t.Fatal("frozen completion was not recoverable")
	}
}
