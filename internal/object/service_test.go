package object

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc64"
	"io"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// memS3 is an SDK seam: the production pack adapter, metadata, decoder and
// read path run unchanged; only the S3 service is in memory.
type memS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemS3() *memS3 { return &memS3{objects: map[string][]byte{}} }

func (m *memS3) PutObject(ctx context.Context, in *awss3.PutObjectInput, _ ...func(*awss3.Options)) (*awss3.PutObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	key := aws.ToString(in.Key)
	if in.IfNoneMatch != nil && m.objects[key] != nil {
		return nil, &smithy.GenericAPIError{Code: "PreconditionFailed", Message: "exists"}
	}
	m.objects[key] = data
	sum := md5.Sum(data)
	return &awss3.PutObjectOutput{ETag: aws.String(`"` + hex.EncodeToString(sum[:]) + `"`)}, nil
}

func (m *memS3) HeadObject(ctx context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.objects[aws.ToString(in.Key)]
	if !ok {
		return nil, &types.NotFound{Message: aws.String("missing")}
	}
	return &awss3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(data))), StorageClass: types.StorageClassStandard}, nil
}

func (m *memS3) GetObject(ctx context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	data, ok := m.objects[aws.ToString(in.Key)]
	m.mu.Unlock()
	if !ok {
		return nil, &types.NoSuchKey{Message: aws.String("missing")}
	}
	if in.Range == nil {
		return &awss3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(data)), ContentLength: aws.Int64(int64(len(data)))}, nil
	}
	var lo, hi int64
	if _, err := fmt.Sscanf(aws.ToString(in.Range), "bytes=%d-%d", &lo, &hi); err != nil || lo < 0 || hi < lo || hi >= int64(len(data)) {
		return nil, &smithy.GenericAPIError{Code: "InvalidRange", Message: "bad range"}
	}
	body := data[lo : hi+1]
	return &awss3.GetObjectOutput{
		Body: io.NopCloser(bytes.NewReader(body)), ContentLength: aws.Int64(int64(len(body))),
		ContentRange: aws.String(packs3.RangeHeader(lo, int64(len(body)), int64(len(data)))),
	}, nil
}

func (m *memS3) CopyObject(context.Context, *awss3.CopyObjectInput, ...func(*awss3.Options)) (*awss3.CopyObjectOutput, error) {
	return nil, errors.New("unused")
}
func (m *memS3) RestoreObject(context.Context, *awss3.RestoreObjectInput, ...func(*awss3.Options)) (*awss3.RestoreObjectOutput, error) {
	return nil, errors.New("unused")
}
func (m *memS3) DeleteObject(context.Context, *awss3.DeleteObjectInput, ...func(*awss3.Options)) (*awss3.DeleteObjectOutput, error) {
	return nil, errors.New("unused")
}

// testBody is the verified logical body the API layer supplies.
type testBody struct {
	r   io.Reader
	sum []byte
	crc []byte
	err error
	alg string
}

func body(data []byte) *testBody {
	sum := md5.Sum(data)
	table := crc64.MakeTable(0x9a6c9329ac4bc9b5)
	var crc [8]byte
	binary.BigEndian.PutUint64(crc[:], crc64.Checksum(data, table))
	return &testBody{r: bytes.NewReader(data), sum: sum[:], crc: crc[:], alg: "CRC64NVME"}
}
func (b *testBody) Read(p []byte) (int, error) { return b.r.Read(p) }
func (b *testBody) MD5() []byte                { return b.sum }
func (b *testBody) Checksum() (string, []byte) {
	return b.alg, b.crc
}

type failingBody struct {
	*testBody
	err error
}

func (b *failingBody) Read(p []byte) (int, error) {
	n, err := b.testBody.Read(p)
	if errors.Is(err, io.EOF) {
		return n, b.err
	}
	return n, err
}

type fixtureEvents struct{}

func (fixtureEvents) ObjectCommit(_ context.Context, e CommitEvent) (storage.OutboxEvent, error) {
	history := strings.Repeat("a", 32)
	id, err := storage.EventID(history, e.OpID, "Objects", "object_commit")
	if err != nil {
		return storage.OutboxEvent{}, err
	}
	return storage.OutboxEvent{
		Version: storage.ModelVersion, ID: id, HistoryID: history, Epoch: 0,
		Entity: "Objects", Action: "object_commit", Object: e.Object, Revision: e.Revision,
		OpID: e.OpID, PayloadPointer: "journal/input/" + e.OpID, PayloadSHA256: strings.Repeat("b", 64),
	}, nil
}

type serviceFixture struct {
	service  *Service
	metadata *badger.Store
	bucket   storage.Bucket
	options  Options
	fences   []storage.WriterFence
	next     int
}

// fence returns a fresh backend writer fence per logical operation, like a
// runtime lease owner that registers one session per request.
func (f *serviceFixture) fence() storage.WriterFence {
	fence := f.fences[f.next%len(f.fences)]
	f.next++
	return fence
}

func (f *serviceFixture) withFence(fence storage.WriterFence) *Service {
	options := f.options
	options.Fence = fence
	service, err := New(options)
	if err != nil {
		panic(err)
	}
	return service
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	ctx := context.Background()
	f := contracttest.NewFixture()
	f.Quota.Limit = 1 << 20
	c, err := clock.NewVirtual(f.Now, 100)
	if err != nil {
		t.Fatal(err)
	}
	db, err := badger.Open(badger.Options{Path: t.TempDir(), Now: func() time.Time { return f.Now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	fences := []storage.WriterFence{
		{LeaseID: "writer-lease", ServiceIncarnation: f.Lease.Fence.ServiceIncarnation, Epoch: 1},
		{LeaseID: "writer-lease-2", ServiceIncarnation: f.Lease.Fence.ServiceIncarnation, Epoch: 1},
		{LeaseID: "writer-lease-3", ServiceIncarnation: f.Lease.Fence.ServiceIncarnation, Epoch: 1},
		{LeaseID: "writer-lease-4", ServiceIncarnation: f.Lease.Fence.ServiceIncarnation, Epoch: 1},
	}
	leases := make([]storage.WriterLease, len(fences))
	for i, fence := range fences {
		leases[i] = storage.WriterLease{Fence: fence, ExpiresAt: f.Now.Add(time.Minute)}
	}
	if err := db.Bootstrap(ctx, badger.Bootstrap{
		Buckets: []storage.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials,
		Grants: []storage.Grant{f.Grant}, Quotas: []storage.LogicalQuota{f.Quota},
		Writers: leases,
	}); err != nil {
		t.Fatal(err)
	}
	physical := newMemS3()
	store, err := packs3.New(physical, "physical-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 16, MaxCopyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	limits := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
	gate, err := coordination.NewR1Gate(c, f.Lease.Fence.ServiceIncarnation, limits)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := coordination.NewWriterRegistry(gate, limits, coordination.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	admission, err := readpath.NewAdmission(readpath.Limits{
		Node:   readpath.ScopeLimits{ActiveRequests: 100, Budget: testBudget(64 << 20)},
		Bucket: readpath.ScopeLimits{ActiveRequests: 100, Budget: testBudget(64 << 20)},
	})
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
	bucketPolicy, err := validator.NewBucketSnapshot(f.Bucket.ID, 1, config.DefaultCachePolicy())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := readpath.NewWarmReader(readpath.WarmOptions{
		Metadata: db, Packs: store, Cache: lru, Admission: admission, Gate: gate, Clock: c,
		Global: global, Metrics: &readpath.WarmMetrics{}, TransportBytes: 4096,
		MaxManifestPages: 4, MaxManifestChunks: 20000,
	})
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Metadata: db, Packs: store, Registry: registry, Clock: c, Fence: fences[0],
		ProtectedUntil: f.Now.Add(time.Hour),
		Chunking:       engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10},
		Compression:    engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)},
		Events:         fixtureEvents{},
		Reader:         reader,
		BucketPolicy:   func(string) (config.BucketSnapshot, error) { return bucketPolicy, nil },
		Budget:         testBudget(64 << 20),
		MaxOutstanding: 3,
		RequestTimeout: 30 * time.Second,
	}
	service, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return &serviceFixture{service: service, metadata: db, bucket: f.Bucket, options: options, fences: fences}
}

func testBudget(n int64) readpath.Budget {
	c := readpath.Cost{ChunkSlots: n, StoredBytes: n, IOSlots: n, IOBytes: n, RawBytes: n, WorkspaceBytes: n, CopyBytes: n}
	prefetch := readpath.Cost{ChunkSlots: n / 2, StoredBytes: n / 2, IOSlots: n / 2, IOBytes: n / 2, RawBytes: n / 2, WorkspaceBytes: n / 2, CopyBytes: n / 2}
	return readpath.Budget{Capacity: c, Prefetch: prefetch}
}

func (f *serviceFixture) put(t *testing.T, ctx context.Context, key string, data []byte) PutResult {
	t.Helper()
	service := f.withFence(f.fence())
	out, err := service.PutObject(ctx, PutRequest{
		Bucket: f.bucket, Key: key, Body: body(data), ContentLength: int64(len(data)),
		Metadata: map[string]string{"content-type": "text/plain"}, UserID: "owner", CredentialID: "owner-key",
	})
	if err != nil {
		t.Fatalf("put %q (%d bytes): %v", key, len(data), err)
	}
	return out
}

func (f *serviceFixture) get(t *testing.T, ctx context.Context, key string, span *storage.ByteRange) (GetResult, []byte) {
	t.Helper()
	var out []byte
	result, err := f.service.GetObject(ctx, GetRequest{
		Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: key, CredentialID: "owner-key", Range: span,
		Emit: func(_ context.Context, b []byte) error { out = append(out, b...); return nil },
	})
	if err != nil {
		t.Fatalf("get %q range=%v: %v", key, span, err)
	}
	return result, out
}

func TestPutThenReadHeadAndRange(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	data := []byte("hello warm object\n")
	put := f.put(t, ctx, "file", data)
	sum := md5.Sum(data)
	if put.ETag != `"`+hex.EncodeToString(sum[:])+`"` {
		t.Fatalf("etag %s", put.ETag)
	}
	if put.ChecksumAlgorithm != "CRC64NVME" || put.ChecksumValue == "" {
		t.Fatalf("checksum %+v", put)
	}

	stat, err := f.service.Stat(ctx, GetRequest{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "file", CredentialID: "owner-key"})
	if err != nil {
		t.Fatal(err)
	}
	if stat.Info.Size != int64(len(data)) || stat.Info.ETag != put.ETag || stat.Info.LastModified.IsZero() {
		t.Fatalf("stat %+v", stat.Info)
	}
	if _, ok := stat.Info.Metadata["x-dsg-commit-time"]; ok {
		t.Fatal("reserved metadata leaked")
	}

	result, got := f.get(t, ctx, "file", nil)
	if result.Info.Size != int64(len(data)) || !bytes.Equal(got, data) || result.Range != (storage.ByteRange{Offset: 0, Length: int64(len(data))}) {
		t.Fatalf("get %+v %q", result, got)
	}
	want := data[2:9]
	result, got = f.get(t, ctx, "file", &storage.ByteRange{Offset: 2, Length: 7})
	if !bytes.Equal(got, want) || result.Range != (storage.ByteRange{Offset: 2, Length: 7}) {
		t.Fatalf("range %+v %q", result, got)
	}
}

func TestPutEmptyObject(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	f.put(t, ctx, "empty", nil)
	stat, err := f.service.Stat(ctx, GetRequest{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "empty", CredentialID: "owner-key"})
	if err != nil || stat.Info.Size != 0 {
		t.Fatalf("stat %+v err %v", stat, err)
	}
	result, got := f.get(t, ctx, "empty", nil)
	if len(got) != 0 || result.Info.Size != 0 {
		t.Fatalf("empty get %+v %q", result, got)
	}
}

func TestFailedOverwriteKeepsOldObjectAndReleasesReservation(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	old := []byte("old object")
	f.put(t, ctx, "file", old)

	broken := &failingBody{testBody: body([]byte("new object that never completes")), err: errors.New("payload failed")}
	service := f.withFence(f.fence())
	_, err := service.PutObject(ctx, PutRequest{
		Bucket: f.bucket, Key: "file", Body: broken, ContentLength: 31,
		Metadata: map[string]string{"content-type": "text/plain"}, UserID: "owner", CredentialID: "owner-key",
	})
	if err == nil {
		t.Fatal("failed body committed")
	}
	_, got := f.get(t, ctx, "file", nil)
	if !bytes.Equal(got, old) {
		t.Fatalf("overwrite failure changed the object: %q", got)
	}
	quota, err := f.metadata.GetQuota(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	if quota.Reserved != 0 || quota.Used != int64(len(old)) {
		t.Fatalf("quota after failure: %+v", quota)
	}
}

func TestPutOverwriteAndChecksumMetadata(t *testing.T) {
	f := newServiceFixture(t)
	ctx := context.Background()
	f.put(t, ctx, "file", []byte(strings.Repeat("a", 100)))
	second := f.put(t, ctx, "file", []byte(strings.Repeat("b", 50)))
	_, got := f.get(t, ctx, "file", nil)
	if string(got) != strings.Repeat("b", 50) {
		t.Fatalf("overwrite not visible: %d bytes", len(got))
	}
	decoded, err := base64.StdEncoding.DecodeString(second.ChecksumValue)
	if err != nil || len(decoded) != 8 {
		t.Fatalf("checksum value %q", second.ChecksumValue)
	}
}
