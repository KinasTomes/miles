package object

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (f *serviceFixture) copy(ctx context.Context, src, dst string, metadata map[string]string) (CopyResult, error) {
	return f.service.CopyObject(ctx, CopyRequest{
		Source: f.bucket, SourceKey: src, Bucket: f.bucket, Key: dst, Metadata: metadata,
		UserID: "writer", CredentialID: "writer-key",
	})
}

func (f *serviceFixture) head(t *testing.T, key string) storage.ObjectHead {
	t.Helper()
	h, err := f.metadata.GetObject(context.Background(), storage.ObjectKey{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: key})
	if err != nil {
		t.Fatal(key, err)
	}
	return h
}

func (f *serviceFixture) refs(t *testing.T, h storage.ObjectHead) []storage.ChunkRef {
	t.Helper()
	pages, err := f.metadata.GetManifest(context.Background(), h.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	var out []storage.ChunkRef
	for _, p := range pages {
		out = append(out, p.Chunks...)
	}
	return out
}

// A same-domain copy by a grantee is a new manifest of the source refs: no
// pack is written, ETag/checksum/metadata are the source's, the bucket owner
// pays, and the copy keeps its bytes when the source is overwritten.
func TestCopySameDomainRepeatsRefsWithoutPayload(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	data := make([]byte, 100<<10)
	_, _ = rand.Read(data)
	f.put(t, ctx, "src", data)
	f.physical.mu.Lock()
	packsBefore := len(f.physical.objects)
	f.physical.mu.Unlock()

	out, err := f.copy(ctx, "src", "dst", nil)
	if err != nil {
		t.Fatal(err)
	}
	src, dst := f.head(t, "src"), f.head(t, "dst")
	if dst.ManifestID == src.ManifestID || dst.ETag != src.ETag || dst.ETagKind != src.ETagKind || dst.Checksum != src.Checksum || dst.Size != src.Size {
		t.Fatalf("copy head %+v, source %+v", dst, src)
	}
	if out.ETag != src.ETag || out.LastModified.IsZero() {
		t.Fatalf("result %+v", out)
	}
	if got := info(dst).Metadata; !reflect.DeepEqual(got, info(src).Metadata) {
		t.Fatalf("COPY metadata %v, source %v", got, info(src).Metadata)
	}
	if !reflect.DeepEqual(f.refs(t, dst), f.refs(t, src)) || len(f.refs(t, dst)) < 2 {
		t.Fatal("copied manifest does not repeat the source refs")
	}
	f.physical.mu.Lock()
	packsAfter := len(f.physical.objects)
	f.physical.mu.Unlock()
	if packsAfter != packsBefore {
		t.Fatalf("copy wrote %d pack objects", packsAfter-packsBefore)
	}
	q, err := f.metadata.GetQuota(ctx, f.bucket.OwnerID)
	if err != nil || q.Used != 2*int64(len(data)) || q.Reserved != 0 {
		t.Fatalf("owner quota %+v %v", q, err)
	}

	f.put(t, ctx, "src", []byte("overwritten"))
	if _, got := f.get(t, ctx, "dst", nil); !bytes.Equal(got, data) {
		t.Fatal("copy changed with its source")
	}
}

// Self-copy needs REPLACE: COPY changes nothing and is refused before
// admission; REPLACE publishes a new revision with the same content and ETag,
// new metadata and no quota delta.
func TestCopyToItself(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	data := bytes.Repeat([]byte("self"), 9000)
	f.put(t, ctx, "k", data)
	before := f.head(t, "k")
	quota, _ := f.metadata.GetQuota(ctx, f.bucket.OwnerID)

	if _, err := f.copy(ctx, "k", "k", nil); !errors.Is(err, ErrCopyToItself) {
		t.Fatal("self COPY", err)
	}
	if f.head(t, "k").Revision != before.Revision {
		t.Fatal("refused self-copy published")
	}
	replaced := map[string]string{"content-type": "text/csv", "content-language": "vi-VN", "x-amz-meta-tag": "v2"}
	if _, err := f.copy(ctx, "k", "k", replaced); err != nil {
		t.Fatal("self REPLACE", err)
	}
	after := f.head(t, "k")
	if after.Revision != before.Revision+1 || after.ManifestID == before.ManifestID || after.ETag != before.ETag || after.Checksum != before.Checksum {
		t.Fatalf("self REPLACE head %+v, before %+v", after, before)
	}
	if got := info(after).Metadata; !reflect.DeepEqual(got, replaced) {
		t.Fatalf("REPLACE metadata %v", got)
	}
	if q, _ := f.metadata.GetQuota(ctx, f.bucket.OwnerID); q.Used != quota.Used || q.Reserved != 0 {
		t.Fatalf("self REPLACE quota %+v, before %+v", q, quota)
	}
	if _, got := f.get(t, ctx, "k", nil); !bytes.Equal(got, data) {
		t.Fatal("self REPLACE changed content")
	}
}

// The destination owner's quota bounds the copy; a refused copy leaves the
// destination and the ledger as they were.
func TestCopyQuotaExceededKeepsDestination(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	big := bytes.Repeat([]byte("q"), 600<<10)
	f.put(t, ctx, "src", big)
	f.put(t, ctx, "dst", []byte("old"))
	before := f.head(t, "dst")
	if _, err := f.copy(ctx, "src", "dst", nil); !errors.Is(err, storage.ErrQuotaExceeded) {
		t.Fatal("copy over quota", err)
	}
	if after := f.head(t, "dst"); after.Revision != before.Revision || after.ManifestID != before.ManifestID {
		t.Fatal("refused copy replaced the destination")
	}
	if q, _ := f.metadata.GetQuota(ctx, f.bucket.OwnerID); q.Reserved != 0 || q.Used != int64(len(big))+3 {
		t.Fatalf("ledger after refused copy %+v", q)
	}
}

// headHook is the Warm reader's metadata. override replaces a head no test
// should materialize (a 5 GiB object); after runs once the real lookup
// returned, at the boundary where a source must already be protected.
type headHook struct {
	*badger.Store
	override func(storage.ObjectKey) (storage.ObjectHead, bool)
	after    func(storage.ObjectKey)
}

func (m *headHook) GetObject(ctx context.Context, k storage.ObjectKey) (storage.ObjectHead, error) {
	if m.override != nil {
		if h, ok := m.override(k); ok {
			return h, nil
		}
	}
	h, err := m.Store.GetObject(ctx, k)
	if m.after != nil {
		m.after(k)
	}
	return h, err
}

// Retiring a source pack right after the source head lookup must be refused
// for CopyObject exactly as for GET: the source view protects resolution
// before the destination writer pins anything.
func TestCopySourceHeadIsProtectedAgainstRetirement(t *testing.T) {
	for _, op := range []string{"GET", "CopyObject"} {
		t.Run(op, func(t *testing.T) {
			f := newServiceFixture(t)
			ctx := context.Background()
			data := bytes.Repeat([]byte("retire"), 5000)
			f.put(t, ctx, "src", data)
			pack := f.chunkPack(t, f.head(t, "src"))
			var retire error
			hooked := false
			f.reads.after = func(k storage.ObjectKey) {
				if k.Key == "src" && !hooked {
					hooked = true
					retire = f.gate.Retire(pack)
				}
			}
			var err error
			if op == "GET" {
				_, err = f.service.GetObject(ctx, GetRequest{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "src", CredentialID: "owner-key",
					Emit: func(context.Context, []byte) error { return nil }})
			} else {
				_, err = f.copy(ctx, "src", "dst", nil)
			}
			if !hooked || !errors.Is(retire, storage.ErrConflict) || err != nil {
				t.Fatalf("hooked=%v retire=%v %s=%v", hooked, retire, op, err)
			}
			if op == "CopyObject" {
				if _, got := f.get(t, ctx, "dst", nil); !bytes.Equal(got, data) {
					t.Fatal("copy bytes differ")
				}
			}
		})
	}
}

func (f *serviceFixture) chunkPack(t *testing.T, h storage.ObjectHead) storage.PackRef {
	t.Helper()
	key := f.refs(t, h)[0].Key
	rows, err := f.metadata.GetChunks(context.Background(), []storage.ChunkKey{key}, true)
	if err != nil {
		t.Fatal(err)
	}
	return rows[key].Location.Pack
}

func TestCopyRejectsBeforeAdmission(t *testing.T) {
	ctx := context.Background()
	f := newServiceFixture(t)
	f.put(t, ctx, "real", []byte("data"))
	real := f.head(t, "real")
	cases := []struct {
		name  string
		edit  func(*storage.ObjectHead, *CopyRequest)
		err   error
		admit bool
	}{
		{"source tombstone", func(h *storage.ObjectHead, _ *CopyRequest) { h.Deleted = true }, ErrNoSuchCopySource, false},
		{"5 GiB + 1", func(h *storage.ObjectHead, _ *CopyRequest) { h.Size = MaxCopyBytes + 1 }, ErrCopySourceTooLarge, false},
		{"exactly 5 GiB passes the cap; the owner quota decides", func(h *storage.ObjectHead, _ *CopyRequest) { h.Size = MaxCopyBytes }, storage.ErrQuotaExceeded, true},
		{"checksum algorithm change", func(_ *storage.ObjectHead, r *CopyRequest) { r.ChecksumAlgorithm = "SHA256" }, ErrCopyChecksumChange, false},
		{"same checksum algorithm", func(_ *storage.ObjectHead, r *CopyRequest) { r.ChecksumAlgorithm = "CRC64NVME" }, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head := real
			head.Object.Key = "src"
			r := CopyRequest{Source: f.bucket, SourceKey: "src", Bucket: f.bucket, Key: "dst", UserID: "owner", CredentialID: "owner-key"}
			tc.edit(&head, &r)
			f.reads.override = func(k storage.ObjectKey) (storage.ObjectHead, bool) { return head, k.Key == "src" }
			defer func() { f.reads.override = nil }()
			admitted := false
			o := f.options
			o.AdmitWriter = func(ctx context.Context, w WriterRequest) (AdmittedWriter, error) {
				admitted = true
				return f.options.AdmitWriter(ctx, w)
			}
			s, err := New(o)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.CopyObject(ctx, r)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error %v, want %v", err, tc.err)
			}
			if admitted != tc.admit {
				t.Fatalf("admitted %v, want %v (err %v)", admitted, tc.admit, err)
			}
		})
	}
	if _, err := f.copy(ctx, "missing", "dst", nil); !errors.Is(err, ErrNoSuchCopySource) {
		t.Fatal("missing source", err)
	}
}

func TestCopyCrossDomain(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	b2, created, err := f.metadata.CreateBucket(ctx, storage.BucketCreate{
		CredentialID: "writer-key", UserID: "writer", Name: "dst-bucket",
	})
	if err != nil || !created {
		t.Fatalf("create second bucket %v %v", b2, err)
	}

	data := bytes.Repeat([]byte("cross domain chunk data\n"), 4096)
	f.put(t, ctx, "src", data)
	srcHead := f.head(t, "src")

	f.physical.mu.Lock()
	packsBefore := len(f.physical.objects)
	f.physical.mu.Unlock()

	// 1. Missing chunks in destination domain: loads from source, publishes into other-domain
	out, err := f.service.CopyObject(ctx, CopyRequest{
		Source: f.bucket, SourceKey: "src", Bucket: b2, Key: "dst1",
		UserID: "writer", CredentialID: "writer-key",
	})
	if err != nil {
		t.Fatalf("cross-domain copy: %v", err)
	}
	if out.ETag != srcHead.ETag || out.LastModified.IsZero() {
		t.Fatalf("result %+v, src %+v", out, srcHead)
	}

	h1, err := f.metadata.GetObject(ctx, storage.ObjectKey{Bucket: b2.Name, BucketID: b2.ID, Key: "dst1"})
	if err != nil {
		t.Fatalf("get dst1 head: %v", err)
	}
	if h1.Domain != "other-domain" || h1.ManifestID == srcHead.ManifestID || h1.Size != srcHead.Size || h1.ETag != srcHead.ETag || h1.Checksum != srcHead.Checksum {
		t.Fatalf("h1 %+v, srcHead %+v", h1, srcHead)
	}

	// Verify no cross-domain references in destination manifest
	pages1, err := f.metadata.GetManifest(ctx, h1.ManifestID)
	if err != nil {
		t.Fatalf("get manifest: %v", err)
	}
	for _, p := range pages1 {
		if p.Domain != "other-domain" {
			t.Fatalf("page domain %q, want other-domain", p.Domain)
		}
		for _, c := range p.Chunks {
			if c.Key.Domain != "other-domain" {
				t.Fatalf("chunk ref domain %q, want other-domain", c.Key.Domain)
			}
		}
	}

	// Read back through GetObject on destination bucket
	var readBack []byte
	_, err = f.service.GetObject(ctx, GetRequest{
		Bucket: b2.Name, BucketID: b2.ID, Key: "dst1", CredentialID: "writer-key",
		Emit: func(_ context.Context, b []byte) error { readBack = append(readBack, b...); return nil },
	})
	if err != nil || !bytes.Equal(readBack, data) {
		t.Fatalf("read back cross-domain copy len=%d, err=%v", len(readBack), err)
	}

	f.physical.mu.Lock()
	packsAfterFirst := len(f.physical.objects)
	f.physical.mu.Unlock()
	if packsAfterFirst <= packsBefore {
		t.Fatalf("expected new packs in destination domain, got %d <= %d", packsAfterFirst, packsBefore)
	}

	// 2. Destination domain already has the chunks: reuse without writing new packs
	out2, err := f.service.CopyObject(ctx, CopyRequest{
		Source: f.bucket, SourceKey: "src", Bucket: b2, Key: "dst2",
		UserID: "writer", CredentialID: "writer-key",
	})
	if err != nil {
		t.Fatalf("second cross-domain copy: %v", err)
	}
	if out2.ETag != srcHead.ETag {
		t.Fatalf("out2 ETag %q != %q", out2.ETag, srcHead.ETag)
	}

	f.physical.mu.Lock()
	packsAfterSecond := len(f.physical.objects)
	f.physical.mu.Unlock()
	if packsAfterSecond != packsAfterFirst {
		t.Fatalf("expected 0 new packs for existing chunks in destination, wrote %d", packsAfterSecond-packsAfterFirst)
	}

	// 3. Metadata REPLACE
	replaced := map[string]string{"content-type": "application/json", "x-amz-meta-custom": "tested"}
	out3, err := f.service.CopyObject(ctx, CopyRequest{
		Source: f.bucket, SourceKey: "src", Bucket: b2, Key: "dst3", Metadata: replaced,
		UserID: "writer", CredentialID: "writer-key",
	})
	if err != nil {
		t.Fatalf("cross-domain REPLACE copy: %v", err)
	}
	if out3.ETag != srcHead.ETag {
		t.Fatalf("out3 ETag %q", out3.ETag)
	}
	h3, err := f.metadata.GetObject(ctx, storage.ObjectKey{Bucket: b2.Name, BucketID: b2.ID, Key: "dst3"})
	if err != nil {
		t.Fatal(err)
	}
	if got := info(h3).Metadata; !reflect.DeepEqual(got, replaced) {
		t.Fatalf("REPLACE metadata %v, want %v", got, replaced)
	}
}

func TestCopyColdSource(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()

	data := bytes.Repeat([]byte("cold pack data\n"), 2048)
	f.put(t, ctx, "cold-src", data)
	srcHead := f.head(t, "cold-src")
	packRef := f.chunkPack(t, srcHead)

	// Find pack key in physical backend
	var packKey string
	f.physical.mu.Lock()
	for k := range f.physical.objects {
		if strings.HasPrefix(k, "packs/") && strings.Contains(k, packRef.ID) {
			packKey = k
			break
		}
	}
	packSize := int64(len(f.physical.objects[packKey]))
	f.physical.classes[packKey] = types.StorageClassGlacier
	f.physical.mu.Unlock()

	// Mark the source pack as Cold Glacier in metadata
	pInfo := storage.PackInfo{
		Version: storage.ModelVersion, Ref: packRef, Size: packSize,
		Class: storage.Glacier, State: "Cold", StateRevision: 2,
	}
	if err := f.metadata.SetPackForTest(pInfo); err != nil {
		t.Fatalf("set pack cold: %v", err)
	}

	// 1. Same-domain copy of Cold source succeeds without reading payload (Architecture 5.3)
	sameResult, err := f.service.CopyObject(ctx, CopyRequest{
		Source: f.bucket, SourceKey: "cold-src", Bucket: f.bucket, Key: "cold-same-domain-dst",
		UserID: "owner", CredentialID: "owner-key",
	})
	if err != nil {
		t.Fatalf("same-domain Cold copy failed: %v", err)
	}
	if sameResult.ETag != srcHead.ETag {
		t.Fatalf("sameResult ETag %q != %q", sameResult.ETag, srcHead.ETag)
	}

	b2, created, err := f.metadata.CreateBucket(ctx, storage.BucketCreate{
		CredentialID: "writer-key", UserID: "writer", Name: "dst-bkt-cold",
	})
	if err != nil || !created {
		t.Fatalf("create second bucket %v %v", b2, err)
	}

	// 2. Cross-domain copy when destination is MISSING the chunks and source is Cold:
	// must fail with ErrCold and trigger auto-restore if configured
	restoredPacks := []storage.PackRef{}
	var restoreRev uint64
	f.options.Restore = func(ctx context.Context, view *coordination.WarmView, head storage.ObjectHead, ref storage.PackRef, revision uint64, required time.Time, explicit *storage.RestoreRequest) (bool, error) {
		restoreRev = revision
		restoredPacks = append(restoredPacks, ref)
		return true, nil
	}
	sWithRestore, err := New(f.options)
	if err != nil {
		t.Fatal(err)
	}

	_, err = sWithRestore.CopyObject(ctx, CopyRequest{
		Source: f.bucket, SourceKey: "cold-src", Bucket: b2, Key: "cold-cross-dst-fail",
		UserID: "writer", CredentialID: "writer-key",
	})
	if !errors.Is(err, storage.ErrCold) {
		t.Fatalf("expected ErrCold, got %v", err)
	}
	if len(restoredPacks) != 1 || restoredPacks[0] != packRef {
		t.Fatalf("expected restore triggered for %v, got %v", packRef, restoredPacks)
	}
	if restoreRev == 0 {
		t.Fatalf("expected nonzero revision passed to Restore, got 0")
	}

	// 3. Put same data into destination domain so destination already has all chunks
	_, err = sWithRestore.PutObject(ctx, PutRequest{
		Bucket: b2, Key: "existing", Body: body(data), ContentLength: int64(len(data)),
		UserID: "writer", CredentialID: "writer-key",
	})
	if err != nil {
		t.Fatalf("put existing: %v", err)
	}

	restoredPacks = nil

	// Cross-domain copy of cold-src into b2 succeeds without restoring because destination already has chunks!
	outSuccess, err := sWithRestore.CopyObject(ctx, CopyRequest{
		Source: f.bucket, SourceKey: "cold-src", Bucket: b2, Key: "cold-cross-dst-ok",
		UserID: "writer", CredentialID: "writer-key",
	})
	if err != nil {
		t.Fatalf("cross-domain copy of cold source with existing chunks failed: %v", err)
	}
	if outSuccess.ETag != srcHead.ETag {
		t.Fatalf("outSuccess ETag %q != %q", outSuccess.ETag, srcHead.ETag)
	}
	if len(restoredPacks) != 0 {
		t.Fatalf("expected 0 restores triggered, got %d", len(restoredPacks))
	}
}
