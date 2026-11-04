package object

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

func (f *serviceFixture) missing(t *testing.T, ctx context.Context, key string) {
	t.Helper()
	_, err := f.service.GetObject(ctx, GetRequest{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: key, CredentialID: "owner-key",
		Emit: func(context.Context, []byte) error { return nil }})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("GET %q: %v, want not found", key, err)
	}
}

func killAt(prefix string, err error) func(string) error {
	return func(op string) error {
		if strings.HasPrefix(op, prefix) {
			return err
		}
		return nil
	}
}

// A receipt answers a lost Complete response without touching the key: an
// object deleted after the commit stays deleted through the 7-day receipt,
// past the 24-hour Active expiry too, and only the same input gets it.
func TestCompleteReceiptKeepsDeletedObjectDeleted(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 1<<10)}
	u, res := f.stage(t, ctx, "gone", "CRC64NVME", "FULL_OBJECT", data)
	in := input(u, res, false, 1, 2)
	head, err := f.complete(ctx, u.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.DeleteObject(ctx, DeleteRequest{Bucket: f.bucket, Key: "gone", UserID: "owner", CredentialID: "owner-key"}); err != nil {
		t.Fatal(err)
	}
	deleted := f.quota(t, ctx)
	if deleted.Used != 0 || deleted.Reserved != 0 || deleted.Staged != 0 || deleted.ActiveUploads != 0 {
		t.Fatalf("quota after delete %+v", deleted)
	}
	check := func(when string) {
		t.Helper()
		again, err := f.complete(ctx, u.ID, in)
		if err != nil || again.Revision != head.Revision || again.ETag != head.ETag {
			t.Fatal(when, "receipt", again, err)
		}
		f.missing(t, ctx, "gone")
		if q := f.quota(t, ctx); q != deleted {
			t.Fatalf("%s: receipt changed quota %+v, want %+v", when, q, deleted)
		}
		if _, err := f.complete(ctx, u.ID, input(u, res, false, 1)); !errors.Is(err, storage.ErrConflict) {
			t.Fatal(when, "different input got the receipt", err)
		}
	}
	check("after delete")
	f.advance(48 * time.Hour)
	check("after Active expiry")
	f.advance(5 * 24 * time.Hour)
	if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("receipt after 7 days", err)
	}
	f.missing(t, ctx, "gone")
}

// A takeover resumes only the frozen input: another input past expiry is
// refused and leaves the Completing upload and its reservation as they were.
func TestTakeoverRejectsDifferentInput(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixtureQuota(t, nil, 64*mib)
	data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
	u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
	in := input(u, res, false, 1, 2)
	f.refuse = killAt("prepare/", storage.ErrUnavailable)
	if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal("killed attempt", err)
	}
	f.refuse = nil
	f.advance(25 * time.Hour)
	frozen, held := f.state(t, ctx, u.ID), f.quota(t, ctx)
	if _, err := f.complete(ctx, u.ID, input(u, res, false, 1)); !errors.Is(err, storage.ErrInvalid) {
		t.Fatal("takeover with another input", err)
	}
	if s := f.state(t, ctx, u.ID); s.State != "Completing" || s.Fence != frozen.Fence || s.Revision != frozen.Revision || f.quota(t, ctx) != held {
		t.Fatal("refused takeover changed the upload", s.State, f.quota(t, ctx))
	}
	head, err := f.complete(ctx, u.ID, in)
	if err != nil || head.OpID != frozen.CompletionOp {
		t.Fatal("takeover", head, err)
	}
	if q := f.quota(t, ctx); q.Used != head.Size || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("quota %+v", q)
	}
}

// Abort cannot interrupt a frozen Complete, and a Complete that lost to
// Abort cannot publish: each order releases the upload exactly once.
func TestAbortAndCompleteEitherOrder(t *testing.T) {
	ctx := context.Background()
	abort := func(f *serviceFixture, id string) error {
		u, err := f.service.GetUpload(ctx, id)
		if err != nil {
			return err
		}
		return f.service.AbortUpload(ctx, AbortRequest{Upload: u, CredentialID: "owner-key"})
	}
	t.Run("complete first", func(t *testing.T) {
		f := newServiceFixtureQuota(t, nil, 64*mib)
		data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
		u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
		in := input(u, res, false, 1, 2)
		f.refuse = killAt("prepare/", storage.ErrUnavailable)
		if _, err := f.complete(ctx, u.ID, in); !errors.Is(err, storage.ErrUnavailable) {
			t.Fatal("killed attempt", err)
		}
		f.refuse = nil
		held := f.quota(t, ctx)
		if err := abort(f, u.ID); !errors.Is(err, storage.ErrConflict) {
			t.Fatal("abort of a Completing upload", err)
		}
		if s := f.state(t, ctx, u.ID); s.State != "Completing" || f.quota(t, ctx) != held {
			t.Fatal("abort released a Completing upload", s.State, f.quota(t, ctx))
		}
		f.advance(grace)
		head, err := f.complete(ctx, u.ID, in)
		if err != nil {
			t.Fatal("takeover after the refused abort", err)
		}
		if err := abort(f, u.ID); !errors.Is(err, storage.ErrConflict) {
			t.Fatal("abort of a Completed upload", err)
		}
		if q := f.quota(t, ctx); q.Used != head.Size || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
			t.Fatalf("quota %+v", q)
		}
	})
	t.Run("abort first", func(t *testing.T) {
		f := newServiceFixtureQuota(t, nil, 64*mib)
		data := map[uint32][]byte{1: random(t, 5*mib), 2: random(t, 3<<10)}
		u, res := f.stage(t, ctx, "k", "CRC64NVME", "FULL_OBJECT", data)
		read, err := f.service.GetUpload(ctx, u.ID) // the record Complete checked
		if err != nil {
			t.Fatal(err)
		}
		if err := abort(f, u.ID); err != nil {
			t.Fatal(err)
		}
		released := f.quota(t, ctx)
		if released.Reserved != 0 || released.Staged != 0 || released.ActiveUploads != 0 {
			t.Fatalf("quota after abort %+v", released)
		}
		_, err = f.service.CompleteUpload(ctx, CompleteRequest{Upload: read, Input: input(u, res, false, 1, 2), UserID: "writer", CredentialID: "writer-key"})
		if !errors.Is(err, storage.ErrConflict) {
			t.Fatal("Complete after Abort", err)
		}
		if s := f.state(t, ctx, u.ID); s.State != "Aborted" || f.quota(t, ctx) != released {
			t.Fatal("Complete changed an Aborted upload", s.State, f.quota(t, ctx))
		}
		f.missing(t, ctx, "k")
	})
}

type publishDenied struct{ Uploads }

func (publishDenied) PublishPart(context.Context, storage.PartPublishRequest) (storage.UploadPart, error) {
	return storage.UploadPart{}, storage.ErrAccessDenied
}

// The initiator's right is checked again at publication (the contract
// suite revokes it for real): a denied part returns its provisional bytes.
func TestUploadPartDeniedAtPublishReleasesStaging(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	u := f.createUpload(t, ctx, "denied")
	o := f.options
	o.Uploads = publishDenied{o.Uploads}
	service, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	data := random(t, 32<<10)
	b := body(data)
	if _, err = service.UploadPart(ctx, PartRequest{Upload: u, Number: 1, Body: b, ContentLength: int64(len(data)), CredentialID: "writer-key"}); !errors.Is(err, storage.ErrAccessDenied) {
		t.Fatal(err)
	}
	s := f.state(t, ctx, u.ID)
	q := f.quota(t, ctx)
	if s.State != "Active" || s.Publishers != 0 || s.ProvisionalBytes != 0 || s.PublishedBytes != 0 || q.Reserved != 0 || q.Staged != 0 {
		t.Fatalf("denied publish kept staging: upload %+v quota %+v", s, q)
	}
	page, err := f.service.ListParts(ctx, ListPartsRequest{UploadID: u.ID, CredentialID: "owner-key", Limit: 10})
	if err != nil || len(page.Parts) != 0 {
		t.Fatal(page, err)
	}
}
