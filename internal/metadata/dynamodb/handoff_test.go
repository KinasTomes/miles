package dynamodb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"io"
	"math/big"
	"sync"
	"testing"
	"time"
)

// Only physical S3 service I/O is simulated. The writer, pack adapter, native
// DynamoDB metadata (Local), manifest decoder and Warm reader run real code.
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

func TestLocalWriteCommitWarmReadHandoff(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	f.Quota.Limit = 1 << 20
	m, _ := fixtureStore(t, f)
	physical, err := packs3.New(&handoffS3{objects: map[string][]byte{}}, "dev-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	pub := &write.StorePublisher{Metadata: m, Packs: physical, Guard: handoffGuard{}, Writer: f.Lease.Fence, ProtectedUntil: f.Now.Add(time.Hour)}
	pipeline := write.Pipeline{Lookup: m, Publisher: pub, Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10}, Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	body := bytes.Repeat([]byte("ddb-handoff\n"), 4096)
	id, err := write.NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := write.PrepareObject(ctx, pipeline, m, f.Lease.Fence, bytes.NewReader(body), "default", id)
	if err != nil {
		t.Fatal(err)
	}
	r := f.Request("ddb-writer-reader", 0)
	r.Head, err = prepared.BindHead(r.Head)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: f.Bucket.OwnerID, Object: r.Head.Object, Bytes: prepared.Size, StagedBytes: prepared.Size, Revision: 1, State: storage.ReservationActive, Deadline: f.Now.Add(time.Hour), Writer: r.Writer}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err = m.CommitObject(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	c, err := clock.NewVirtual(f.Now, 100)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		t.Fatal(err)
	}
	global, err := validator.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	policy := config.DefaultCachePolicy()
	policy.ReadaheadChunks = 0
	bucket, err := validator.NewBucketSnapshot(f.Bucket.ID, 1, policy)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := coordination.NewWarmGate(c)
	if err != nil {
		t.Fatal(err)
	}
	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 10, MaxCopyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	cost := func(n int64) readpath.Cost {
		return readpath.Cost{ChunkSlots: n, StoredBytes: n, IOSlots: n, IOBytes: n, RawBytes: n, WorkspaceBytes: n, CopyBytes: n}
	}
	budget := readpath.Budget{Capacity: cost(32 << 20), Prefetch: cost(16 << 20)}
	scope := readpath.ScopeLimits{ActiveRequests: 100, Budget: budget}
	admission, err := readpath.NewAdmission(readpath.Limits{Node: scope, Bucket: scope})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := readpath.NewWarmReader(readpath.WarmOptions{Metadata: m, Packs: physical, Cache: lru, Admission: admission, Gate: gate, Clock: c, Global: global, Metrics: &readpath.WarmMetrics{}, TransportBytes: 4096, MaxManifestPages: 4, MaxManifestChunks: 20000})
	if err != nil {
		t.Fatal(err)
	}
	deadline, err := c.Now().Instant.Add(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	q := readpath.WarmRequest{CredentialID: "owner-key", Bucket: "demo", Key: "file", Deadline: deadline, BucketPolicy: bucket, Budget: budget, MaxOutstanding: 3}
	for _, span := range []*storage.ByteRange{nil, {Offset: 16380, Length: 33}, nil} {
		q.Range = span
		var got []byte
		result, err := reader.Read(ctx, q, func(_ context.Context, b []byte) error { got = append(got, b...); return nil })
		want := body
		if span != nil {
			want = body[span.Offset : span.Offset+span.Length]
		}
		if err != nil || result.Head.Revision != 1 || !bytes.Equal(got, want) {
			t.Fatalf("read after commit: %+v %v bytes=%d", result, err, len(got))
		}
	}
	quota, err := m.GetQuota(ctx, f.Bucket.OwnerID)
	if err != nil || quota.Used != int64(len(body)) || quota.Reserved != 0 || quota.Staged != 0 {
		t.Fatal(quota, err)
	}
	del := f.Request("ddb-handoff-delete", 1)
	del.Head.Deleted, del.Head.Size, del.Head.ManifestID = true, 0, ""
	del.Head.ETag, del.Head.ETagKind, del.Head.Metadata = "", "", nil
	if err = m.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, ID: del.ReservationID, OpID: del.Head.OpID, OwnerID: f.Bucket.OwnerID, Object: del.Head.Object, BaseRevision: 1, BaseSize: prepared.Size, Revision: 1, State: storage.ReservationActive, Deadline: f.Now.Add(time.Hour), Writer: del.Writer}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.DeleteObject(ctx, del); err != nil {
		t.Fatal(err)
	}
	q.Range = nil
	if _, err = reader.Read(ctx, q, func(context.Context, []byte) error { return nil }); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("cached deleted object exposed", err)
	}
}
