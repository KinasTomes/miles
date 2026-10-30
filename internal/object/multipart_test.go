package object

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

func (f *serviceFixture) createUpload(t *testing.T, ctx context.Context, key string) storage.MultipartUpload {
	t.Helper()
	u, err := f.service.CreateUpload(ctx, CreateUploadRequest{Bucket: f.bucket, Key: key, Metadata: map[string]string{"content-type": "text/plain"},
		ChecksumAlgorithm: "CRC64NVME", ChecksumType: "FULL_OBJECT", CredentialID: "writer-key"})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func (f *serviceFixture) uploadPart(ctx context.Context, u storage.MultipartUpload, n uint32, data []byte) (PartResult, error) {
	return f.service.UploadPart(ctx, PartRequest{Upload: u, Number: n, Body: body(data), ContentLength: int64(len(data)), CredentialID: "writer-key"})
}

func (f *serviceFixture) parts(t *testing.T, ctx context.Context, id string) []storage.UploadPart {
	t.Helper()
	page, err := f.service.ListParts(ctx, ListPartsRequest{UploadID: id, CredentialID: "writer-key", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return page.Parts
}

// openPublishers counts publishers still holding provisional bytes.
func (f *serviceFixture) openPublishers(t *testing.T, ctx context.Context, id string) int {
	t.Helper()
	page, err := f.metadata.ListPublishers(ctx, id, "", 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range page.Publishers {
		if p.State == "Open" {
			n++
		}
	}
	return n
}

func random(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func md5Hex(b []byte) string {
	sum := md5.Sum(b)
	return hex.EncodeToString(sum[:])
}

func TestUploadPartStagesVerifiedPartWithoutCompleting(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	u := f.createUpload(t, ctx, "big")
	data := random(t, 40<<10)
	res, err := f.uploadPart(ctx, u, 1, data)
	if err != nil {
		t.Fatal(err)
	}
	_, want := body(data).Checksum()
	if res.ETag != md5Hex(data) || res.ChecksumAlgorithm != "CRC64NVME" || res.ChecksumValue == "" {
		t.Fatalf("result %+v", res)
	}
	parts := f.parts(t, ctx, u.ID)
	if len(parts) != 1 || parts[0].Size != int64(len(data)) || parts[0].ETag != res.ETag || parts[0].Revision != 1 || parts[0].Checksum.Type != "FULL_OBJECT" {
		t.Fatalf("parts %+v", parts)
	}
	if got := parts[0].Checksum.Value; got != res.ChecksumValue || len(want) != 8 {
		t.Fatalf("checksum %q, want the part's CRC64NVME", got)
	}
	page, err := f.metadata.GetPartPage(ctx, parts[0].ManifestID, 0)
	if err != nil || len(page.Chunks) == 0 {
		t.Fatalf("part pages: %+v %v", page, err)
	}
	pubs, err := f.metadata.ListPublishers(ctx, u.ID, "", 10)
	if err != nil || len(pubs.Publishers) != 1 || pubs.Publishers[0].State != "Published" || len(pubs.Publishers[0].Packs) == 0 {
		t.Fatalf("publisher must be published with its retained packs: %+v %v", pubs, err)
	}
	if _, err := f.metadata.GetObject(ctx, u.Object); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("a staged part must not publish an object", err)
	}
	q, err := f.metadata.GetQuota(ctx, "owner")
	if err != nil || q.Used != 0 || q.Reserved != int64(len(data)) || q.Staged != int64(len(data)) || q.ActiveUploads != 1 {
		t.Fatalf("quota %+v %v", q, err)
	}
}

func TestUploadPartParallelAndReplacement(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	u := f.createUpload(t, ctx, "parallel")
	data := make([][]byte, 5)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for n := 1; n <= 4; n++ {
		data[n] = random(t, 20<<10)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := f.uploadPart(ctx, u, uint32(n), data[n])
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	parts := f.parts(t, ctx, u.ID)
	if len(parts) != 4 {
		t.Fatalf("parallel parts %+v", parts)
	}
	for i, p := range parts {
		if p.Number != uint32(i+1) || p.ETag != md5Hex(data[i+1]) {
			t.Fatalf("part %d = %+v", i+1, p)
		}
	}

	// Sequential replacement: the new head replaces the old one and only its
	// size stays staged.
	smaller := random(t, 4<<10)
	if _, err := f.uploadPart(ctx, u, 2, smaller); err != nil {
		t.Fatal(err)
	}
	// A slower replacement of the same part number registered first but
	// publishes after another one: it publishes again on top, the last wins.
	slow, fast := random(t, 8<<10), random(t, 12<<10)
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		b := &gatedBody{testBody: body(slow), started: started, release: release}
		_, err := f.service.UploadPart(ctx, PartRequest{Upload: u, Number: 3, Body: b, ContentLength: int64(len(slow)), CredentialID: "writer-key"})
		done <- err
	}()
	<-started
	if _, err := f.uploadPart(ctx, u, 3, fast); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	parts = f.parts(t, ctx, u.ID)
	if parts[1].ETag != md5Hex(smaller) || parts[1].Revision != 2 {
		t.Fatalf("replaced part 2 = %+v", parts[1])
	}
	if parts[2].Revision != 3 || parts[2].ETag != md5Hex(slow) {
		t.Fatalf("part 3 must be the last publisher's: %+v", parts[2])
	}
	var staged int64
	for _, p := range parts {
		staged += p.Size
	}
	q, err := f.metadata.GetQuota(ctx, "owner")
	if err != nil || q.Staged != staged || q.Reserved != staged || f.openPublishers(t, ctx, u.ID) != 0 {
		t.Fatalf("quota %+v staged parts %d: %v", q, staged, err)
	}
}

// gatedBody signals its first read and blocks it until release.
type gatedBody struct {
	*testBody
	started, release chan struct{}
	once             sync.Once
}

func (b *gatedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started); <-b.release })
	return b.testBody.Read(p)
}

// unread fails the test if the service reads a body it should have rejected.
type unread struct {
	*testBody
	t *testing.T
}

func (b unread) Read([]byte) (int, error) {
	b.t.Error("body read after the part was rejected")
	return 0, errors.New("unexpected read")
}

func TestUploadPartQuotaFailsBeforeBody(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	u := f.createUpload(t, ctx, "full")
	size := int64(1<<20) + 1 // above the owner's logical limit
	_, err := f.service.UploadPart(ctx, PartRequest{Upload: u, Number: 1, Body: unread{body(nil), t}, ContentLength: size, CredentialID: "writer-key"})
	if !errors.Is(err, storage.ErrQuotaExceeded) {
		t.Fatal("staging past the quota must fail before the body:", err)
	}
	_, err = f.service.UploadPart(ctx, PartRequest{Upload: u, Number: 1, Body: unread{body(nil), t}, ContentLength: MaxPartBytes + 1, CredentialID: "writer-key"})
	if !errors.Is(err, ErrPartTooLarge) {
		t.Fatal("a part above 5 GiB must fail before admission:", err)
	}
	if len(f.physical.objects) != 0 {
		t.Fatal("rejected parts wrote packs")
	}
	q, err := f.metadata.GetQuota(ctx, "owner")
	if err != nil || q.Reserved != 0 || q.Staged != 0 || f.openPublishers(t, ctx, u.ID) != 0 {
		t.Fatalf("rejected part leaked accounting: %+v %v", q, err)
	}
}

func TestUploadPartBadBodyCancelsPublisher(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	u := f.createUpload(t, ctx, "bad")
	data := random(t, 20<<10)
	bad := &failingBody{testBody: body(data), err: errors.New("bad digest")}
	if _, err := f.service.UploadPart(ctx, PartRequest{Upload: u, Number: 1, Body: bad, ContentLength: int64(len(data)), CredentialID: "writer-key"}); err == nil {
		t.Fatal("a body failing its checks must not publish")
	}
	if _, err := f.uploadPart(ctx, u, 2, data[:100]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.UploadPart(ctx, PartRequest{Upload: u, Number: 3, Body: body(data), ContentLength: int64(len(data)) + 1, CredentialID: "writer-key"}); !errors.Is(err, ErrLengthMismatch) {
		t.Fatal("short body:", err)
	}
	parts := f.parts(t, ctx, u.ID)
	q, err := f.metadata.GetQuota(ctx, "owner")
	if len(parts) != 1 || parts[0].Number != 2 || err != nil || q.Staged != 100 || f.openPublishers(t, ctx, u.ID) != 0 {
		t.Fatalf("parts %+v quota %+v %v", parts, q, err)
	}
}

func TestAbortReleasesUploadAndKeepsSharedData(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	shared := random(t, 48<<10)
	f.put(t, ctx, "shared", shared)
	u := f.createUpload(t, ctx, "copy")
	if _, err := f.uploadPart(ctx, u, 1, shared); err != nil { // dedups onto the object's chunks
		t.Fatal(err)
	}
	u, err := f.metadata.GetUpload(ctx, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.AbortUpload(ctx, AbortRequest{Upload: u, CredentialID: "owner-key"}); err != nil {
		t.Fatal("owner abort:", err)
	}
	if u, err = f.metadata.GetUpload(ctx, u.ID); err != nil || u.State != "Aborted" {
		t.Fatal(u.State, err)
	}
	q, err := f.metadata.GetQuota(ctx, "owner")
	if err != nil || q.Used != int64(len(shared)) || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("abort must release the upload once: %+v %v", q, err)
	}
	if _, got := f.get(t, ctx, "shared", nil); !bytes.Equal(got, shared) {
		t.Fatal("abort deleted data the object still uses")
	}
	if _, err := f.uploadPart(ctx, u, 2, shared[:10]); err == nil {
		t.Fatal("aborted upload accepted a part")
	}
	if _, err := f.service.ListParts(ctx, ListPartsRequest{UploadID: u.ID, CredentialID: "writer-key", Limit: 10}); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("aborted upload listed parts:", err)
	}
	if err := f.service.AbortUpload(ctx, AbortRequest{Upload: u, CredentialID: "owner-key"}); err != nil {
		t.Fatal("repeated abort of an Aborted upload:", err)
	}
	if q2, _ := f.metadata.GetQuota(ctx, "owner"); q2 != q {
		t.Fatalf("repeated abort changed accounting: %+v", q2)
	}
}

func TestAbortClosesOpenPublisher(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	u := f.createUpload(t, ctx, "crashed")
	// A part request that registered and then died: its publisher stays Open
	// with provisional bytes until Abort resolves it.
	admitted, err := f.options.AdmitWriter(ctx, WriterRequest{})
	if err != nil {
		t.Fatal(err)
	}
	req := storage.MultipartRequest{UploadID: u.ID, Credential: "writer-key", OpID: "0123456789abcdef0123456789abcdef", Writer: admitted.Fence}
	if _, err := f.metadata.RegisterPartPublisher(ctx, req, storage.PartPublisher{ID: "fedcba9876543210fedcba9876543210", UploadID: u.ID, OpID: req.OpID,
		ManifestID: "0011223344556677889900aabbccddee", PartNumber: 1, Bytes: 1000, Deadline: u.ExpiresAt.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if u, err = f.metadata.GetUpload(ctx, u.ID); err != nil || u.Publishers != 1 {
		t.Fatal(u, err)
	}
	if err := f.service.AbortUpload(ctx, AbortRequest{Upload: u, CredentialID: "writer-key"}); err != nil {
		t.Fatal(err)
	}
	u, err = f.metadata.GetUpload(ctx, u.ID)
	q, qerr := f.metadata.GetQuota(ctx, "owner")
	if err != nil || qerr != nil || u.State != "Aborted" || u.Publishers != 0 || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("upload %+v quota %+v: %v %v", u, q, err, qerr)
	}
}
