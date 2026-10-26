package readpath

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Only the SDK service is simulated. Writer, S3 adapter, Badger, manifest,
// decoder and Warm reader are production code. No maintenance runs in this test.
type handoffS3 struct {
	packs3.API
	mu      sync.Mutex
	objects map[string][]byte
}

func (s *handoffS3) PutObject(ctx context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	b, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.objects[aws.ToString(in.Key)]; ok {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed"}
	}
	if aws.ToString(in.IfNoneMatch) != "*" || int64(len(b)) != aws.ToInt64(in.ContentLength) {
		return nil, storage.ErrInvalid
	}
	s.objects[aws.ToString(in.Key)] = b
	return &awss3.PutObjectOutput{}, nil
}
func (s *handoffS3) HeadObject(ctx context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NotFound"}
	}
	return &awss3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(b)))}, nil
}
func (s *handoffS3) GetObject(ctx context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "NoSuchKey"}
	}
	out := &awss3.GetObjectOutput{}
	if in.Range != nil {
		var first, last int64
		if _, err := fmt.Sscanf(*in.Range, "bytes=%d-%d", &first, &last); err != nil || first < 0 || last < first || last >= int64(len(b)) {
			return nil, storage.ErrInvalid
		}
		out.ContentRange = aws.String(packs3.RangeHeader(first, last-first+1, int64(len(b))))
		b = b[first : last+1]
	}
	out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(b)), aws.Int64(int64(len(b)))
	return out, nil
}

type handoffGuard struct{}

func (handoffGuard) Pin(context.Context, storage.Chunk) (bool, error) { return true, nil }

func TestWriteCommitWarmReadHandoff(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	seed := contracttest.NewFixture()
	physical, err := packs3.New(&handoffS3{objects: map[string][]byte{}}, "dev-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	f.reader.options.Packs = physical
	pub := &write.StorePublisher{Metadata: f.metadata, Packs: physical, Guard: handoffGuard{}, Writer: seed.Lease.Fence, ProtectedUntil: seed.Now.Add(time.Hour)}
	pipeline := write.Pipeline{Lookup: f.metadata, Publisher: pub,
		Chunking:    engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	body := bytes.Repeat([]byte("storage-handoff\n"), 4096)
	id, err := write.NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := write.PrepareObject(ctx, pipeline, f.metadata, seed.Lease.Fence, bytes.NewReader(body), "default", id)
	if err != nil {
		t.Fatal(err)
	}
	r := seed.Request("handoff-overwrite", 1)
	old, err := f.metadata.GetObject(ctx, r.Head.Object)
	if err != nil || old.Revision != 1 {
		t.Fatal(old, err)
	}
	r.Head.ManifestID, r.Head.Size = prepared.ManifestID, prepared.Size
	r.Head.PageCount, r.Head.ChunkCount, r.Head.PageIndex = prepared.PageCount, prepared.ChunkCount, prepared.PageIndex
	if err := f.metadata.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: seed.Bucket.OwnerID,
		Object: r.Head.Object, BaseRevision: old.Revision, BaseSize: old.Size, Bytes: max(0, prepared.Size-old.Size), StagedBytes: prepared.Size,
		Revision: 1, State: storage.ReservationActive, Deadline: seed.Now.Add(time.Hour), Writer: r.Writer}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := f.metadata.CommitObject(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	for _, span := range []*storage.ByteRange{nil, {Offset: 16380, Length: 33}} {
		q := f.request
		q.Range = span
		want := body
		if span != nil {
			want = body[span.Offset : span.Offset+span.Length]
		}
		result, got, err := f.read(ctx, q)
		if err != nil || result.Head.Revision != 2 || !bytes.Equal(got, want) {
			t.Fatalf("read after write: %+v %v", result, err)
		}
	}
	quota, _ := f.metadata.GetQuota(ctx, seed.Bucket.OwnerID)
	if quota.Used != int64(len(body)) || quota.Reserved != 0 || quota.Staged != 0 {
		t.Fatal(quota)
	}
	del := seed.Request("handoff-delete", 2)
	del.Head.Deleted, del.Head.Size, del.Head.ManifestID = true, 0, ""
	del.Head.ETag, del.Head.ETagKind, del.Head.Metadata = "", "", nil
	if err := f.metadata.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, ID: del.ReservationID, OpID: del.Head.OpID, OwnerID: seed.Bucket.OwnerID,
		Object: del.Head.Object, BaseRevision: 2, BaseSize: prepared.Size, Revision: 1, State: storage.ReservationActive, Deadline: seed.Now.Add(time.Hour), Writer: del.Writer}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.metadata.DeleteObject(ctx, del); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.read(ctx, f.request); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("cached deleted object was exposed", err)
	}
}
