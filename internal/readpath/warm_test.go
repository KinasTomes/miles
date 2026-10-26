package readpath

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http/httptest"
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
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// SDK seam only: metadata serialization, physical S3 adapter, writer, decoder,
// checksums, LRU and scheduler are production code. Backend bytes are borrowed
// from immutable fixture storage, not copied into an unaccounted backend buffer.
type warmS3 struct {
	packs3.API
	mu               sync.Mutex
	body             []byte
	gets             int
	class            types.StorageClass
	classes          map[string]types.StorageClass
	fault            string
	before           func(context.Context) error
	beforeRange      func(context.Context, *awss3.GetObjectInput) error
	entered, unblock chan struct{}
}

func (s *warmS3) HeadObject(ctx context.Context, in *awss3.HeadObjectInput, _ ...func(*awss3.Options)) (*awss3.HeadObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	class := s.class
	if v, ok := s.classes[aws.ToString(in.Key)]; ok {
		class = v
	}
	return &awss3.HeadObjectOutput{ContentLength: aws.Int64(int64(len(s.body))), StorageClass: class}, nil
}
func (s *warmS3) GetObject(ctx context.Context, in *awss3.GetObjectInput, _ ...func(*awss3.Options)) (*awss3.GetObjectOutput, error) {
	s.mu.Lock()
	s.gets++
	hook := s.before
	rangeHook := s.beforeRange
	entered, unblock := s.entered, s.unblock
	s.mu.Unlock()
	if rangeHook != nil {
		if err := rangeHook(ctx, in); err != nil {
			return nil, err
		}
	}
	if hook != nil {
		if err := hook(ctx); err != nil {
			return nil, err
		}
	}
	if entered != nil {
		entered <- struct{}{}
		<-unblock
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var lo, hi int64
	if _, err := fmt.Sscanf(aws.ToString(in.Range), "bytes=%d-%d", &lo, &hi); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if lo < 0 || hi < lo || hi >= int64(len(s.body)) {
		return nil, storage.ErrInvalid
	}
	b := s.body[lo : hi+1]
	n := int64(len(b))
	switch s.fault {
	case "short":
		b = b[:len(b)-1]
	case "long":
		b = append(bytes.Clone(b), 0)
	case "error":
		return nil, storage.ErrUnavailable
	}
	return &awss3.GetObjectOutput{ContentLength: aws.Int64(n), ContentRange: aws.String(packs3.RangeHeader(lo, n, int64(len(s.body)))), Body: io.NopCloser(bytes.NewReader(b))}, nil
}
func (s *warmS3) count() int { s.mu.Lock(); defer s.mu.Unlock(); return s.gets }

type warmFixture struct {
	reader   *WarmReader
	request  WarmRequest
	backend  *warmS3
	body     []byte
	first    []byte
	entries  []pack.Entry
	ref      storage.PackRef
	clock    *clock.Virtual
	metadata *badger.Store
}

func newWarmFixture(t *testing.T, w int64) *warmFixture {
	return newWarmMode(t, w, false)
}
func newWarmMode(t *testing.T, w int64, noFCS bool, rawSizes ...int) *warmFixture {
	t.Helper()
	ctx := context.Background()
	f := contracttest.NewFixture()
	c, err := clock.NewVirtual(f.Now, 100)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte{0xab, 0x9b, 0xe0, 0x9b} // valid nonempty CRC32C == 0
	compressed := bytes.Repeat([]byte("warm-zstd-body\n"), 300)
	if noFCS {
		compressed = bytes.Repeat([]byte{'x'}, 4096)
	}
	if len(rawSizes) > 0 {
		compressed = bytes.Repeat([]byte{'x'}, rawSizes[0])
	}
	cmp, err := engine.NewCompressor(engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)})
	if err != nil {
		t.Fatal(err)
	}
	defer cmp.Close()
	ref := storage.PackRef{ID: "pack-reader", Generation: "generation-1"}
	builder, err := pack.NewBuilder(ref)
	if err != nil {
		t.Fatal(err)
	}
	for i, b := range [][]byte{raw, compressed} {
		k, err := engine.HashChunk("default", b)
		if err != nil {
			t.Fatal(err)
		}
		v := pack.Blob{Key: k, RawLength: uint32(len(b)), Codec: "raw", Stored: b}
		if i == 1 {
			enc, err := cmp.Encode(engine.Chunk{Key: k, Bytes: b})
			if err != nil || enc.Codec != "zstd" {
				t.Fatal(err)
			}
			v.Codec = enc.Codec
			v.Stored = enc.Bytes
			if noFCS {
				// No FCS/dictionary, 4KiB window, one final RLE block of 4096 x.
				v.Stored = []byte{0x28, 0xb5, 0x2f, 0xfd, 0, 0x10, 0x03, 0x80, 0x00, 'x'}
			}
		}
		if err = builder.Add(v); err != nil {
			t.Fatal(err)
		}
	}
	data, _, entries, err := builder.Finish()
	if err != nil {
		t.Fatal(err)
	}
	var first, second pack.Entry
	for _, e := range entries {
		if e.Codec == "raw" {
			first = e
		} else {
			second = e
		}
	}
	if first.CRC32C != 0 {
		t.Fatal("zero CRC premise", first)
	}
	entries = []pack.Entry{first, second, first}
	f.Packs = []storage.PackInfo{{Version: 1, Ref: ref, Size: int64(len(data)), State: "Warm", Class: storage.Standard, StateRevision: 1}}
	f.Quota.Limit = 1 << 30 // Fixture-only logical quota, not a production default.
	db, err := badger.Open(badger.Options{Path: t.TempDir(), Now: func() time.Time { return f.Now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err = db.Bootstrap(ctx, badger.Bootstrap{Buckets: []storage.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []storage.Grant{f.Grant}, Quotas: []storage.LogicalQuota{f.Quota}, Packs: f.Packs, Writers: []storage.WriterLease{f.Lease}}); err != nil {
		t.Fatal("bootstrap", err)
	}
	for _, e := range entries[:2] {
		if _, _, err = db.PutChunkIfAbsent(ctx, storage.Chunk{Version: 1, Key: e.Key, RawBytes: int64(e.RawLength), Location: e.Location(ref), LocationRevision: 1}, f.Lease.Fence); err != nil {
			t.Fatal("chunk", err)
		}
	}
	req := f.Request("warm-read", 0)
	pages := []storage.ManifestPage{{Version: 1, ManifestID: req.Head.ManifestID, Domain: "default", Number: 0}}
	var offset int64
	for _, e := range entries {
		pages[0].Chunks = append(pages[0].Chunks, storage.ChunkRef{Key: e.Key, RawBytes: int64(e.RawLength), LogicalOffset: offset})
		offset += int64(e.RawLength)
	}
	if err = db.WriteManifestPages(ctx, pages, f.Lease.Fence); err != nil {
		t.Fatal("pages", err)
	}
	req.Head.Size = offset
	if err = db.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, OpID: req.Head.OpID, Revision: 1, State: storage.ReservationActive, Deadline: f.Now.Add(time.Hour), ID: req.ReservationID, OwnerID: f.Bucket.OwnerID, Object: req.Head.Object, Bytes: offset, StagedBytes: offset, Writer: f.Lease.Fence}); err != nil {
		t.Fatal("reserve", err)
	}
	if _, err = db.CommitObject(ctx, req); err != nil {
		t.Fatal("commit", err)
	}
	backend := &warmS3{body: data, class: types.StorageClassStandard}
	store, err := packs3.New(backend, "physical-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	lru, err := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 10, MaxCopyBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		t.Fatal(err)
	}
	global, err := v.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if err != nil {
		t.Fatal(err)
	}
	bp := config.DefaultCachePolicy()
	bp.ReadaheadChunks = w
	bucket, err := v.NewBucketSnapshot(f.Bucket.ID, 1, bp)
	if err != nil {
		t.Fatal(err)
	}
	g, err := coordination.NewWarmGate(c)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewWarmReader(WarmOptions{Metadata: db, Packs: store, Cache: lru, Admission: admission(t, 32<<20, 32<<20), Gate: g, Clock: c, Global: global, Metrics: &WarmMetrics{}, TransportBytes: 4096, MaxManifestPages: 4, MaxManifestChunks: 20000})
	if err != nil {
		t.Fatal(err)
	}
	d, err := c.Now().Instant.Add(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := append(append(bytes.Clone(raw), compressed...), raw...)
	return &warmFixture{reader: reader, request: WarmRequest{CredentialID: "owner-key", Bucket: "demo", Key: "file", Deadline: d, BucketPolicy: bucket, Budget: budget(32 << 20), MaxOutstanding: 3}, backend: backend, body: body, first: raw, entries: entries, ref: ref, clock: c, metadata: db}
}
func (f *warmFixture) read(ctx context.Context, q WarmRequest) (WarmResult, []byte, error) {
	var out []byte
	r, err := f.reader.Read(ctx, q, func(_ context.Context, b []byte) error { out = append(out, b...); return nil })
	return r, out, err
}

func TestWarmReaderRealAdaptersFullRangeCacheAndPurge(t *testing.T) {
	for _, w := range []int64{0, 2} {
		t.Run(fmt.Sprint(w), func(t *testing.T) {
			f := newWarmFixture(t, w)
			ctx := context.Background()
			for _, span := range []*storage.ByteRange{nil, {Offset: 2, Length: int64(len(f.body) - 4)}, {Offset: 4, Length: int64(len(f.body) - 8)}, {Offset: int64(len(f.body) - 1), Length: 1}} {
				q := f.request
				q.Range = span
				want := f.body
				if span != nil {
					want = want[span.Offset : span.Offset+span.Length]
				}
				for pass := 0; pass < 2; pass++ {
					before := f.backend.count()
					result, out, err := f.read(ctx, q)
					if err != nil || !bytes.Equal(out, want) || sha256.Sum256(out) != sha256.Sum256(want) {
						t.Fatalf("range=%v pass=%d result=%+v out=%x err=%v", span, pass, result, out, err)
					}
					if pass == 1 && f.backend.count() != before {
						t.Fatal("cache hit fetched")
					}
					drained(t, f.reader.options.Admission)
				}
			}
			if err := f.reader.options.Cache.Evict(ctx, f.ref); err != nil {
				t.Fatal(err)
			}
			before := f.backend.count()
			_, out, err := f.read(ctx, f.request)
			if err != nil || !bytes.Equal(out, f.body) || f.backend.count() == before {
				t.Fatal("purge affected origin", err)
			}
			if err = f.reader.options.Gate.Retire(f.ref); err != nil {
				t.Fatal("leaked generation pin", err)
			}
		})
	}
}
func TestWarmReaderDenialDeadlineAndColdBeforeCache(t *testing.T) {
	for _, mode := range []string{"denied", "missing credential", "bucket incarnation", "deadline equality", "cold backend"} {
		t.Run(mode, func(t *testing.T) {
			f := newWarmFixture(t, 0)
			ctx := context.Background()
			if _, _, err := f.read(ctx, f.request); err != nil {
				t.Fatal(err)
			}
			q := f.request
			switch mode {
			case "denied":
				q.CredentialID = "writer-key"
			case "missing credential":
				q.CredentialID = "missing"
			case "bucket incarnation":
				v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
				q.BucketPolicy, _ = v.NewBucketSnapshot("other-bucket", 1, config.DefaultCachePolicy())
			case "deadline equality":
				q.Deadline = f.clock.Now().Instant
			case "cold backend":
				f.backend.class = types.StorageClassGlacier
			}
			before := f.reader.options.Metrics.Snapshot()
			calls := f.backend.count()
			_, out, err := f.read(ctx, q)
			after := f.reader.options.Metrics.Snapshot()
			if err == nil || len(out) != 0 || after.CacheHits != before.CacheHits || f.backend.count() != calls {
				t.Fatal("denied operation reached cache/payload", err, after)
			}
			drained(t, f.reader.options.Admission)
		})
	}
}
func TestWarmReaderCorruptMiddleAbortsAndCounts(t *testing.T) {
	for _, source := range []string{"backend", "cache"} {
		t.Run(source, func(t *testing.T) {
			f := newWarmFixture(t, 0)
			ctx := context.Background()
			e := f.entries[1]
			if source == "backend" {
				f.backend.body[int(e.Offset)] ^= 1
			} else {
				stored := bytes.Clone(f.backend.body[e.Offset : e.Offset+uint64(e.StoredLen)])
				stored[0] ^= 1
				if err := f.reader.options.Cache.Put(ctx, storage.CacheKey{Pack: f.ref, Range: storage.ByteRange{Offset: int64(e.Offset), Length: int64(e.StoredLen)}}, stored); err != nil {
					t.Fatal(err)
				}
			}
			_, out, err := f.read(ctx, f.request)
			var format *pack.FormatError
			if !errors.As(err, &format) || !bytes.Equal(out, f.first) {
				t.Fatal("corrupt chunk emitted or stream not aborted", out, err)
			}
			s := f.reader.options.Metrics.Snapshot()
			if s.IntegrityFailures != 1 || s.Aborted != 1 || s.EmittedBytes != uint64(len(f.first)) {
				t.Fatal(s)
			}
			rec := httptest.NewRecorder()
			f.reader.options.Metrics.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
			if !strings.Contains(rec.Body.String(), "miles_warm_read_integrity_failures_total 1") {
				t.Fatal(rec.Body.String())
			}
			drained(t, f.reader.options.Admission)
			if err = f.reader.options.Gate.Retire(f.ref); err != nil {
				t.Fatal("leaked pin", err)
			}
		})
	}
}
func TestWarmReaderBackendFaultsAndNoCacheFill(t *testing.T) {
	for _, mode := range []string{"short", "long", "error"} {
		t.Run(mode, func(t *testing.T) {
			f := newWarmFixture(t, 0)
			f.backend.fault = mode
			_, out, err := f.read(context.Background(), f.request)
			if err == nil || len(out) != 0 {
				t.Fatal(out, err)
			}
			if f.reader.options.Metrics.Snapshot().CacheHits != 0 {
				t.Fatal("unexpected hit")
			}
			e := f.entries[0]
			_, hit, err := f.reader.options.Cache.Get(context.Background(), storage.CacheKey{Pack: f.ref, Range: storage.ByteRange{Offset: int64(e.Offset), Length: int64(e.StoredLen)}})
			if err != nil || hit {
				t.Fatal("unverified data cached", err)
			}
			drained(t, f.reader.options.Admission)
		})
	}
}
func TestWarmReaderCancellationQuiescesBeforeRelease(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.backend.entered = make(chan struct{}, 1)
	f.backend.unblock = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, out, err := f.read(ctx, f.request)
		if len(out) != 0 {
			err = fmt.Errorf("unexpected output: %x", out)
		}
		done <- err
	}()
	<-f.backend.entered
	cancel()
	if err := f.reader.options.Gate.Retire(f.ref); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("pin released before backend quiescence", err)
	}
	f.reader.options.Admission.mu.Lock()
	active := f.reader.options.Admission.active
	used := f.reader.options.Admission.used
	f.reader.options.Admission.mu.Unlock()
	if active != 1 || used.total[0] != 1 {
		t.Fatal("reservation released early", active, used)
	}
	close(f.backend.unblock)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	drained(t, f.reader.options.Admission)
	if err := f.reader.options.Gate.Retire(f.ref); err != nil {
		t.Fatal(err)
	}
}
func TestWarmReaderEmitterFailureRetainsPinAndCleansUp(t *testing.T) {
	f := newWarmFixture(t, 2)
	stop := errors.New("emitter failed")
	_, err := f.reader.Read(context.Background(), f.request, func(_ context.Context, _ []byte) error {
		if err := f.reader.options.Gate.Retire(f.ref); !errors.Is(err, storage.ErrConflict) {
			t.Fatal("pin released while emitter owns bytes", err)
		}
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	drained(t, f.reader.options.Admission)
	if err = f.reader.options.Gate.Retire(f.ref); err != nil {
		t.Fatal(err)
	}
}
func TestWarmReaderRejectsDecoderCostBeforePayload(t *testing.T) {
	f := newWarmFixture(t, 0)
	q := f.request
	q.Range = &storage.ByteRange{Offset: 4, Length: 1}
	q.Budget.Capacity.WorkspaceBytes = 1
	q.Budget.Prefetch.WorkspaceBytes = 0
	_, out, err := f.read(context.Background(), q)
	if !errors.Is(err, ErrExhausted) || f.backend.count() != 0 || len(out) != 0 {
		t.Fatal("decoder allocation/I/O before budget", err)
	}
	drained(t, f.reader.options.Admission)
}
