package object

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"reflect"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
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
	replaced := map[string]string{"content-type": "text/csv", "x-amz-meta-tag": "v2"}
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

// sourceHead overrides the source head the service reads, for limits no test
// should materialize (a 5 GiB object) or states the fixture cannot reach.
type sourceHead struct {
	Metadata
	head storage.ObjectHead
}

func (m sourceHead) GetObject(ctx context.Context, k storage.ObjectKey) (storage.ObjectHead, error) {
	if k.Key == "src" {
		return m.head, nil
	}
	return m.Metadata.GetObject(ctx, k)
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
		{"other dedup domain", func(h *storage.ObjectHead, _ *CopyRequest) { h.Domain = "tenant-b" }, ErrCrossDomainCopy, false},
		{"checksum algorithm change", func(_ *storage.ObjectHead, r *CopyRequest) { r.ChecksumAlgorithm = "SHA256" }, ErrCopyChecksumChange, false},
		{"same checksum algorithm", func(_ *storage.ObjectHead, r *CopyRequest) { r.ChecksumAlgorithm = "CRC64NVME" }, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			head := real
			r := CopyRequest{Source: f.bucket, SourceKey: "src", Bucket: f.bucket, Key: "dst", UserID: "owner", CredentialID: "owner-key"}
			tc.edit(&head, &r)
			admitted := false
			o := f.options
			o.Metadata = sourceHead{f.options.Metadata, head}
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
