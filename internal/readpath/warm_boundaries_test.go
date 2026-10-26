package readpath

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/manifest"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/klauspost/compress/zstd"
)

func TestWarmReaderNoFCSAndExactWorkspaceBoundary(t *testing.T) {
	for _, delta := range []int64{-1, 0, 1} {
		t.Run(fmt.Sprint(delta), func(t *testing.T) {
			f := newWarmMode(t, 0, true)
			e := f.entries[1]
			var header zstd.Header
			if err := header.Decode(f.backend.body[e.Offset : e.Offset+uint64(e.StoredLen)]); err != nil || header.HasFCS {
				t.Fatal("no-FCS premise", header, err)
			}
			m, err := pack.EstimateDecodeMemory(e)
			if err != nil {
				t.Fatal(err)
			}
			q := f.request
			q.Range = &storage.ByteRange{Offset: 4, Length: int64(e.RawLength)}
			q.Budget.Capacity.WorkspaceBytes = m.WorkspaceBytes + delta
			q.Budget.Prefetch.WorkspaceBytes = 0
			_, out, err := f.read(context.Background(), q)
			if delta < 0 {
				if !errors.Is(err, ErrExhausted) || f.backend.count() != 0 || len(out) != 0 {
					t.Fatal("admission bypass", err)
				}
			} else if err != nil || !bytes.Equal(out, bytes.Repeat([]byte{'x'}, 4096)) {
				t.Fatal("wrong/no decoded output", err)
			}
			drained(t, f.reader.options.Admission)
		})
	}
}
func TestWarmReaderConcurrentAdmissionAndReadAhead(t *testing.T) {
	f := newWarmFixture(t, 2)
	q := f.request
	q.MaxOutstanding = 2
	f.reader.options.Admission.limits.Bucket.ActiveRequests = 1
	firstEntered, secondDone, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	firstRange := fmt.Sprintf("bytes=%d-%d", f.entries[0].Offset, f.entries[0].Offset+uint64(f.entries[0].StoredLen)-1)
	f.backend.beforeRange = func(ctx context.Context, in *awss3.GetObjectInput) error {
		if aws.ToString(in.Range) == firstRange {
			once.Do(func() { close(firstEntered); <-release })
		} else {
			close(secondDone)
		}
		return ctx.Err()
	}
	done := make(chan error, 1)
	var out []byte
	go func() {
		_, err := f.reader.Read(context.Background(), q, func(_ context.Context, b []byte) error { out = append(out, b...); return nil })
		done <- err
	}()
	<-firstEntered
	<-secondDone
	_, other, err := f.read(context.Background(), q)
	if !errors.Is(err, ErrExhausted) || len(other) != 0 {
		t.Error("concurrent request bypassed bucket admission", err)
	}
	// No emitter can run while the first demanded chunk is still blocked.
	f.reader.options.Admission.mu.Lock()
	slots := f.reader.options.Admission.used.total[0]
	f.reader.options.Admission.mu.Unlock()
	if slots != 2 {
		t.Error("K accounting", slots)
	}
	select {
	case err := <-done:
		t.Error("emitted out of order/returned early", err)
	default:
	}
	close(release)
	if err = <-done; err != nil || !bytes.Equal(out, f.body) {
		t.Fatal(out, err)
	}
	drained(t, f.reader.options.Admission)
}
func TestWarmReaderManifestBudgetsAndReady(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	f.reader.options.MaxManifestChunks = 2
	_, out, err := f.read(ctx, f.request)
	if !errors.Is(err, ErrExhausted) || len(out) != 0 || f.backend.count() != 0 {
		t.Fatal("manifest limit bypass", err)
	}
	f.reader.options.MaxManifestChunks = 20000
	q := f.request
	stop := errors.New("ready rejected")
	q.Ready = func(r WarmResult) error {
		if r.Head.PageCount != 1 || r.Range.Length != int64(len(f.body)) {
			t.Fatal(r)
		}
		return stop
	}
	_, out, err = f.read(ctx, q)
	if !errors.Is(err, stop) || len(out) != 0 || f.backend.count() != 0 {
		t.Fatal(err)
	}
	drained(t, f.reader.options.Admission)
}
func TestWarmReaderRangeAcrossGreedyManifestPages(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	seed := contracttest.NewFixture()
	r := seed.Request("two-pages", 1)
	cap, err := manifest.RefCapacity(r.Head.ManifestID, "default")
	if err != nil {
		t.Fatal(err)
	}
	e := f.entries[0]
	count := cap + 1
	pages := []storage.ManifestPage{{Version: 1, ManifestID: r.Head.ManifestID, Domain: "default", Number: 0}, {Version: 1, ManifestID: r.Head.ManifestID, Domain: "default", Number: 1}}
	for i := 0; i < count; i++ {
		page := i / cap
		pages[page].Chunks = append(pages[page].Chunks, storage.ChunkRef{Key: e.Key, RawBytes: int64(e.RawLength), LogicalOffset: int64(i) * int64(e.RawLength)})
	}
	if err = f.metadata.WriteManifestPages(ctx, pages, seed.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	old, err := f.metadata.GetObject(ctx, r.Head.Object)
	if err != nil {
		t.Fatal(err)
	}
	r.Head.Size = int64(count) * int64(e.RawLength)
	res := storage.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: seed.Bucket.OwnerID, Object: r.Head.Object, BaseRevision: 1, BaseSize: old.Size, Revision: 1, State: storage.ReservationActive, Deadline: seed.Now.Add(30 * time.Minute), Writer: r.Writer, Bytes: r.Head.Size - old.Size, StagedBytes: r.Head.Size}
	if err = f.metadata.ReserveLogicalQuota(ctx, res); err != nil {
		t.Fatal(err)
	}
	if _, err = f.metadata.CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
	q := f.request
	q.Range = &storage.ByteRange{Offset: int64(cap*4 - 2), Length: 4}
	result, out, err := f.read(ctx, q)
	want := append(bytes.Clone(f.first[2:]), f.first[:2]...)
	if err != nil || !bytes.Equal(out, want) || result.Head.PageCount != 2 {
		t.Fatal("cross-page range", out, err)
	}
	drained(t, f.reader.options.Admission)
}
func TestWarmReaderActualSDKTransport(t *testing.T) {
	f := newWarmFixture(t, 0)
	data := f.backend.body
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/physical-bucket/packs/pa/pack-reader/generation-1" {
			http.Error(w, "wrong exact generation", 404)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method == "HEAD" {
			return
		}
		var lo, hi int
		if _, err := fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &lo, &hi); err != nil || lo < 0 || hi >= len(data) || hi < lo {
			http.Error(w, "range", 416)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(hi-lo+1))
		w.Header().Set("Content-Range", packs3.RangeHeader(int64(lo), int64(hi-lo+1), int64(len(data))))
		w.WriteHeader(206)
		_, _ = w.Write(data[lo : hi+1])
	}))
	defer server.Close()
	client := awss3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: aws.AnonymousCredentials{}, HTTPClient: server.Client()}, func(o *awss3.Options) { o.BaseEndpoint = aws.String(server.URL); o.UsePathStyle = true })
	physical, err := packs3.New(client, "physical-bucket", "")
	if err != nil {
		t.Fatal(err)
	}
	f.reader.options.Packs = physical
	for pass := 0; pass < 2; pass++ {
		_, out, err := f.read(context.Background(), f.request)
		if err != nil || !bytes.Equal(out, f.body) {
			t.Fatal("SDK read", err)
		}
		drained(t, f.reader.options.Admission)
	}
}

func TestWarmReaderGenerationIsolationAndExpiryDuringFetch(t *testing.T) {
	t.Run("other generation cache cannot satisfy canonical", func(t *testing.T) {
		f := newWarmFixture(t, 0)
		e := f.entries[0]
		key := storage.CacheKey{Pack: storage.PackRef{ID: f.ref.ID, Generation: "other-generation"}, Range: storage.ByteRange{Offset: int64(e.Offset), Length: int64(e.StoredLen)}}
		if err := f.reader.options.Cache.Put(context.Background(), key, []byte("fake")); err != nil {
			t.Fatal(err)
		}
		_, out, err := f.read(context.Background(), f.request)
		if err != nil || !bytes.Equal(out, f.body) || f.backend.count() == 0 {
			t.Fatal("wrong generation cache accepted", err)
		}
		drained(t, f.reader.options.Admission)
	})
	t.Run("expiry during synchronous fetch", func(t *testing.T) {
		f := newWarmFixture(t, 0)
		f.backend.before = func(context.Context) error { return f.clock.Advance(time.Minute) }
		_, out, err := f.read(context.Background(), f.request)
		if !errors.Is(err, storage.ErrFenced) || len(out) != 0 {
			t.Fatal("emitted after deadline", err)
		}
		drained(t, f.reader.options.Admission)
		if err = f.reader.options.Gate.Retire(f.ref); err != nil {
			t.Fatal("expired pin leaked", err)
		}
	})
}

func TestWarmReaderMaximumChunkUsesAuditedEstimator(t *testing.T) {
	f := newWarmMode(t, 0, false, engine.MaxChunkBytes)
	f.reader.options.Admission = admission(t, 256<<20, 256<<20)
	q := f.request
	q.Budget = budget(256 << 20)
	q.MaxOutstanding = 1
	q.Range = &storage.ByteRange{Offset: 4, Length: 1}
	m, err := pack.EstimateDecodeMemory(f.entries[1])
	if err != nil {
		t.Fatal(err)
	}
	if m.OutputBytes != engine.MaxChunkBytes || m.WorkspaceBytes <= 5*int64(engine.MaxChunkBytes) {
		t.Fatal("invalid max estimator premise", m)
	}
	_, out, err := f.read(context.Background(), q)
	if err != nil || !bytes.Equal(out, []byte{'x'}) {
		t.Fatal(out, err)
	}
	drained(t, f.reader.options.Admission)
}

func TestWarmReaderEmptyMissingAndInvalidRange(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	q := f.request
	q.Key = "absent"
	if _, out, err := f.read(ctx, q); !errors.Is(err, storage.ErrNotFound) || len(out) != 0 {
		t.Fatal(out, err)
	}
	q = f.request
	q.Range = &storage.ByteRange{Offset: 0, Length: 0}
	if _, out, err := f.read(ctx, q); err == nil || len(out) != 0 {
		t.Fatal(out, err)
	}
	seed := contracttest.NewFixture()
	r := seed.Request("empty", 1)
	r.Head.Size = 0
	old, err := f.metadata.GetObject(ctx, r.Head.Object)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.metadata.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, Object: r.Head.Object, OwnerID: seed.Bucket.OwnerID, BaseRevision: 1, BaseSize: old.Size, Revision: 1, State: storage.ReservationActive, Deadline: seed.Now.Add(time.Minute), Writer: r.Writer}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.metadata.CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
	ready := 0
	q = f.request
	q.Ready = func(result WarmResult) error {
		ready++
		if result.Head.Size != 0 || result.Range.Length != 0 {
			t.Fatal(result)
		}
		return nil
	}
	_, out, err := f.read(ctx, q)
	if err != nil || len(out) != 0 || ready != 1 || f.backend.count() != 0 {
		t.Fatal(out, ready, err)
	}
	drained(t, f.reader.options.Admission)
	if err = f.reader.options.Gate.Retire(f.ref); err != nil {
		t.Fatal(err)
	}
}

func TestWarmReaderWholeRangeArchivePreflight(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	seed := contracttest.NewFixture()
	ref := storage.PackRef{ID: "pack-second", Generation: "generation-2"}
	info := storage.PackInfo{Version: 1, Ref: ref, State: "Uploading", StateRevision: 1, Class: storage.Standard, Size: int64(len(f.backend.body)), Writer: seed.Lease.Fence}
	info.ProtectedUntil = seed.Now.Add(time.Hour)
	if err := f.metadata.RegisterUploadingPack(ctx, info, seed.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	if _, err := f.metadata.MarkPackWarm(ctx, ref, 1, seed.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	loc := f.entries[1].Location(ref)
	if _, err := f.metadata.CompareAndSwapChunkLocation(ctx, f.entries[1].Key, 1, loc, seed.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	f.backend.classes = map[string]types.StorageClass{"packs/pa/pack-second/generation-2": types.StorageClassGlacier}
	_, out, err := f.read(ctx, f.request)
	if !errors.Is(err, storage.ErrCold) || len(out) != 0 || f.backend.count() != 0 {
		t.Fatal("later archived pack discovered after cache/emit", out, err)
	}
	if s := f.reader.options.Metrics.Snapshot(); s.CacheHits != 0 || s.CacheMisses != 0 {
		t.Fatal("cache before full preflight", s)
	}
	drained(t, f.reader.options.Admission)
}

func TestWarmReaderSingleTicketAcrossReadyAndStreaming(t *testing.T) {
	f := newWarmFixture(t, 0)
	f.reader.options.Admission.limits.Bucket.ActiveRequests = 1
	q := f.request
	ready := false
	q.Ready = func(WarmResult) error {
		ready = true
		_, out, err := f.read(context.Background(), f.request)
		if !errors.Is(err, ErrExhausted) || len(out) != 0 {
			t.Fatal("ticket was dropped between preflight and streaming", err)
		}
		return nil
	}
	_, out, err := f.read(context.Background(), q)
	if err != nil || !ready || !bytes.Equal(out, f.body) {
		t.Fatal(out, err)
	}
	drained(t, f.reader.options.Admission)
}

func TestWarmReaderCanonicalHashMismatchWithValidStoredCRC(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	seed := contracttest.NewFixture()
	rows, err := f.metadata.GetChunks(ctx, []storage.ChunkKey{f.entries[0].Key}, true)
	if err != nil {
		t.Fatal(err)
	}
	row := rows[f.entries[0].Key]
	row.Key.Hash = strings.Repeat("f", 64)
	if _, _, err = f.metadata.PutChunkIfAbsent(ctx, row, seed.Lease.Fence); err != nil {
		t.Fatal(err)
	}
	r := seed.Request("wrong-hash", 1)
	r.Head.Size = row.RawBytes
	if err = f.metadata.WriteManifestPages(ctx, []storage.ManifestPage{{Version: 1, ManifestID: r.Head.ManifestID, Domain: "default", Number: 0, Chunks: []storage.ChunkRef{{Key: row.Key, RawBytes: row.RawBytes}}}}, r.Writer); err != nil {
		t.Fatal(err)
	}
	old, err := f.metadata.GetObject(ctx, r.Head.Object)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.metadata.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, Object: r.Head.Object, OwnerID: seed.Bucket.OwnerID, BaseRevision: 1, BaseSize: old.Size, Revision: 1, State: storage.ReservationActive, StagedBytes: r.Head.Size, Deadline: seed.Now.Add(time.Minute), Writer: r.Writer}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.metadata.CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
	_, out, err := f.read(ctx, f.request)
	var format *pack.FormatError
	if !errors.As(err, &format) || format.Stage != "content" || len(out) != 0 {
		t.Fatal("SHA verification bypassed", out, err)
	}
	if s := f.reader.options.Metrics.Snapshot(); s.IntegrityFailures != 1 || s.EmittedBytes != 0 {
		t.Fatal(s)
	}
	drained(t, f.reader.options.Admission)
}

func TestWarmReaderTombstoneIsNotFoundBeforeCache(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	if _, _, err := f.read(ctx, f.request); err != nil {
		t.Fatal(err)
	}
	seed := contracttest.NewFixture()
	r := seed.Request("deleted", 1)
	r.Head.Size = 0
	r.Head.Deleted = true
	r.Head.ManifestID = ""
	r.Head.ETag = ""
	r.Head.ETagKind = ""
	r.Head.Checksum = storage.Checksum{}
	r.Head.Metadata = nil
	old, err := f.metadata.GetObject(ctx, r.Head.Object)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.metadata.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, Object: r.Head.Object, OwnerID: seed.Bucket.OwnerID, BaseRevision: 1, BaseSize: old.Size, Revision: 1, State: storage.ReservationActive, Deadline: seed.Now.Add(time.Minute), Writer: r.Writer}); err != nil {
		t.Fatal(err)
	}
	if _, err = f.metadata.DeleteObject(ctx, r); err != nil {
		t.Fatal(err)
	}
	before := f.reader.options.Metrics.Snapshot()
	calls := f.backend.count()
	_, out, err := f.read(ctx, f.request)
	if !errors.Is(err, storage.ErrNotFound) || len(out) != 0 || f.backend.count() != calls || f.reader.options.Metrics.Snapshot().CacheHits != before.CacheHits {
		t.Fatal("tombstone reached cache", out, err)
	}
	drained(t, f.reader.options.Admission)
}
