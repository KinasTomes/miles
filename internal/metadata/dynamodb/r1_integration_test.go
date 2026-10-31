package dynamodb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cache"
	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/engine/write"
	"github.com/KinasTomes/miles/internal/readpath"
	s "github.com/KinasTomes/miles/internal/storage"
	packs3 "github.com/KinasTomes/miles/internal/storage/s3"
)

func TestLocalR1ManagedPutPeerGetAndCopySourcePins(t *testing.T) {
	ctx := context.Background()
	m, peer, f, utc := sharedFixture(t)
	f.Quota.Limit = 1 << 20
	if err := m.update(ctx, func(tx *transaction) error { return put(tx, key("quotas", f.Quota.OwnerID), f.Quota) }); err != nil {
		t.Fatal(err)
	}
	localA, _ := clock.NewVirtual(f.Now, 1000)
	localB, _ := clock.NewVirtual(f.Now, 1000)
	limits := coordination.LeaseLimits{Lifetime: 20 * time.Second, Heartbeat: 5 * time.Second}
	gateA, err := coordination.NewSharedR1Gate(localA, "shared-service", "worker-a", limits, m, utc)
	if err != nil {
		t.Fatal(err)
	}
	gateB, err := coordination.NewSharedR1Gate(localB, "shared-service", "worker-b", limits, peer, utc)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := coordination.NewWriterRegistry(gateA, limits, coordination.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	physical, err := packs3.New(&handoffS3{objects: map[string][]byte{}}, "dev-packs", "")
	if err != nil {
		t.Fatal(err)
	}
	r := f.Request("shared-managed-put", 0)
	r.Writer = s.WriterFence{LeaseID: r.Head.OpID, ServiceIncarnation: "shared-service", Epoch: 1}
	producer := s.MutationContext{HistoryID: strings.Repeat("1", 32), LogicalOpID: r.Head.OpID, MutationID: r.Head.OpID, Object: r.Head.Object, Writer: r.Writer}
	ctx = s.WithMutation(ctx, producer)
	r.Producer = &producer
	m.requireProducers = true
	peer.requireProducers = true
	if err = m.update(ctx, func(tx *transaction) error {
		return put(tx, key("bucket-namespace", f.Bucket.Name), s.BucketNamespace{Version: 1})
	}); err != nil {
		t.Fatal(err)
	}
	deadline, _ := localA.Now().Instant.Add(time.Minute)
	session, err := write.BeginWriteSession(ctx, write.SessionOptions{Registry: registry, Metadata: m, Packs: physical, Fence: r.Writer, Deadline: deadline, Object: r.Head.Object, OpID: r.Head.OpID, CredentialID: r.CredentialID, ProtectedUntil: f.Now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("shared-r1-managed-body"), 1600)
	r.ReservationID = "shared-r1-reservation"
	if err = session.Reserve(s.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: f.Bucket.OwnerID, Object: r.Head.Object, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(time.Minute), Bytes: int64(len(body)), StagedBytes: int64(len(body)), Writer: r.Writer}); err != nil {
		t.Fatal(err)
	}
	id, err := write.NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	pipeline := write.Pipeline{Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10}, Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	prepared, err := session.Prepare(pipeline, bytes.NewReader(body), id)
	if err != nil {
		t.Fatal(err)
	}
	r.Head.Size = prepared.Size
	r.Head, err = prepared.BindHead(r.Head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = session.Commit(r); err != nil {
		t.Fatal(err)
	}
	if err = session.Close(); err != nil {
		t.Fatal("shared writer pins leaked", err)
	}
	v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
	global, _ := v.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	bp := config.DefaultCachePolicy()
	bp.ReadaheadChunks = 0
	bucket, _ := v.NewBucketSnapshot(f.Bucket.ID, 1, bp)
	budget := warmBudget(32 << 20)
	scope := readpath.ScopeLimits{ActiveRequests: 16, Budget: budget}
	admission, _ := readpath.NewAdmission(readpath.Limits{Node: scope, Bucket: scope})
	lru, _ := cache.NewLRU(cache.Limits{CapacityBytes: 1 << 20, MaxEntries: 32, MaxCopyBytes: 1 << 20})
	reader, err := readpath.NewWarmReader(readpath.WarmOptions{Metadata: peer, Packs: physical, Cache: lru, Admission: admission, Gate: gateB, Clock: localB, Global: global, Metrics: &readpath.WarmMetrics{}, TransportBytes: 4096, MaxManifestPages: 4, MaxManifestChunks: 20000})
	if err != nil {
		t.Fatal(err)
	}
	readDeadline, _ := localB.Now().Instant.Add(time.Minute)
	q := readpath.WarmRequest{CredentialID: "owner-key", Bucket: f.Bucket.Name, Key: r.Head.Object.Key, Deadline: readDeadline, BucketPolicy: bucket, Budget: budget, MaxOutstanding: 1}
	for _, span := range []*s.ByteRange{nil, {Offset: 3, Length: 17}} {
		q.Range = span
		var out []byte
		_, err = reader.Read(ctx, q, func(_ context.Context, b []byte) error { out = append(out, b...); return nil })
		want := body
		if span != nil {
			want = body[span.Offset : span.Offset+span.Length]
		}
		if err != nil || !bytes.Equal(out, want) {
			t.Fatal("peer GET/Range", len(out), err)
		}
	}
	// Copy orchestration: admit a separate target writer, stream the authorized
	// source reader into it, close the global admission gate mid-stream, and let
	// both admitted operations finish. No CopyObject HTTP handler is invented.
	copyRequest := f.Request("shared-copy", 0)
	copyRequest.Head.Object.Key = "copied"
	copyRequest.Event.Object = copyRequest.Head.Object
	copyRequest.Writer = s.WriterFence{LeaseID: copyRequest.Head.OpID, Epoch: 1, ServiceIncarnation: "shared-service"}
	copyProducer := s.MutationContext{HistoryID: strings.Repeat("1", 32), LogicalOpID: copyRequest.Head.OpID, MutationID: copyRequest.Head.OpID, Object: copyRequest.Head.Object, Writer: copyRequest.Writer}
	copyRequest.Producer = &copyProducer
	copyContext := s.WithMutation(context.Background(), copyProducer)
	copySession, err := write.BeginWriteSession(copyContext, write.SessionOptions{Registry: registry, Metadata: m, Packs: physical, Fence: copyRequest.Writer, Deadline: deadline, Object: copyRequest.Head.Object, OpID: copyRequest.Head.OpID, CredentialID: copyRequest.CredentialID, ProtectedUntil: f.Now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if err = copySession.Reserve(s.Reservation{Version: 1, ID: copyRequest.ReservationID, OpID: copyRequest.Head.OpID, OwnerID: f.Bucket.OwnerID, Object: copyRequest.Head.Object, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(time.Minute), Bytes: int64(len(body)), StagedBytes: int64(len(body)), Writer: copyRequest.Writer}); err != nil {
		t.Fatal(err)
	}
	pipeIn, pipeOut := io.Pipe()
	sourceResult := make(chan error, 1)
	q.Range = nil
	go func() {
		closed := false
		_, err := reader.Read(ctx, q, func(_ context.Context, b []byte) error {
			if !closed {
				g, e := peer.GetR1Gate(ctx)
				if e != nil {
					return e
				}
				if e = peer.SetR1Gate(ctx, g.Revision, "Closing"); e != nil {
					return e
				}
				closed = true
			}
			_, e := pipeOut.Write(b)
			return e
		})
		_ = pipeOut.CloseWithError(err)
		sourceResult <- err
	}()
	copyManifest, err := write.NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	copied, err := copySession.Prepare(pipeline, pipeIn, copyManifest)
	_ = pipeIn.CloseWithError(err)
	if err != nil {
		t.Fatal("copy target prepare", err)
	}
	if err = <-sourceResult; err != nil {
		t.Fatal("copy source", err)
	}
	copyRequest.Head.Size = copied.Size
	copyRequest.Head, err = copied.BindHead(copyRequest.Head)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = copySession.Commit(copyRequest); err != nil {
		t.Fatal("admitted Copy publish during Closing", err)
	}
	if err = copySession.Close(); err != nil {
		t.Fatal(err)
	}
	gateAfterCopy, err := m.GetR1Gate(ctx)
	if err != nil || gateAfterCopy.Active != 0 || gateAfterCopy.State != "Closing" {
		t.Fatal("copy leaked shared roots", gateAfterCopy, err)
	}
	if err = m.SetR1Gate(ctx, gateAfterCopy.Revision, "Open"); err != nil {
		t.Fatal(err)
	}
	q.Key = "copied"
	var copyBytes []byte
	if _, err = reader.Read(ctx, q, func(_ context.Context, b []byte) error { copyBytes = append(copyBytes, b...); return nil }); err != nil || !bytes.Equal(copyBytes, body) {
		t.Fatal("copied object body", err)
	}
	q.Key = r.Head.Object.Key
	// Copy source uses the same authorized read view/root/pin protocol. It is
	// independent of the target writer; this does not implement CopyObject HTTP.
	head, err := peer.GetObject(ctx, r.Head.Object)
	if err != nil {
		t.Fatal(err)
	}
	view, err := gateB.Acquire(ctx, r.Head.Object, readDeadline)
	if err != nil {
		t.Fatal(err)
	}
	work, err := view.BeginWork(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = view.Bind(ctx, head); err != nil {
		t.Fatal(err)
	}
	pages, err := peer.GetManifest(ctx, head.ManifestID)
	if err != nil {
		t.Fatal(err)
	}
	chunks, err := peer.GetChunks(ctx, []s.ChunkKey{pages[0].Chunks[0].Key}, true)
	if err != nil {
		t.Fatal(err)
	}
	pack := chunks[pages[0].Chunks[0].Key].Location.Pack
	if err = view.Pin(ctx, []s.PackRef{pack}); err != nil {
		t.Fatal(err)
	}
	if err = view.Resolved(ctx); err != nil {
		t.Fatal(err)
	}
	if err = gateA.Retire(pack); !errors.Is(err, coordination.ErrMaintenanceDisabled) {
		t.Fatal("local retirement bypassed shared maintenance", err)
	}
	g, _ := m.GetR1Gate(ctx)
	if g.Active != 1 || g.Resolving != 0 {
		t.Fatal("read root not shared", g)
	}
	if err = m.SetR1Gate(ctx, g.Revision, "Closing"); err != nil {
		t.Fatal(err)
	}
	if _, err = gateA.Acquire(ctx, r.Head.Object, deadline); !errors.Is(err, s.ErrR1Busy) {
		t.Fatal("reader admitted during gate closure", err)
	}
	view.Release()
	g, _ = m.GetR1Gate(ctx)
	if g.Active != 1 {
		t.Fatal("released nonquiescent source", g)
	}
	work.Finish()
	g, _ = m.GetR1Gate(ctx)
	if g.Active != 0 || g.Resolving != 0 {
		t.Fatal("source pin cleanup", g)
	}
	if _, err = peer.CheckR1(ctx, r.Writer); !errors.Is(err, s.ErrFenced) {
		t.Fatal("released writer granted authority", err)
	}
}

func TestLocalR1MultipartPreparationAndStalePublisher(t *testing.T) {
	ctx := context.Background()
	m, peer, f, _ := sharedFixture(t)
	r := r1Request(f, "multipart", 1)
	r.OpID = f.Request("create-upload", 0).Head.OpID
	if _, err := m.AcquireR1(ctx, r); err != nil {
		t.Fatal(err)
	}
	uid := strings.Repeat("2", 32)
	if err := m.BindR1(ctx, r.Fence, s.ObjectHead{Object: r.Object, ManifestID: uid}); err != nil {
		t.Fatal(err)
	}
	c := s.MutationContext{HistoryID: strings.Repeat("1", 32), LogicalOpID: r.OpID, MutationID: r.OpID, Object: r.Object, Writer: r.Fence}
	ctx = s.WithMutation(ctx, c)
	m.requireProducers = true
	peer.requireProducers = true
	request := s.MultipartRequest{UploadID: uid, Credential: "writer-key", OpID: r.OpID, Writer: r.Fence}
	u, err := m.CreateUpload(ctx, s.CreateUploadRequest{Request: request, Upload: s.MultipartUpload{ID: uid, Object: r.Object, ReservationID: "multipart-reservation", ChecksumAlgorithm: "CRC32C", ChecksumType: "FULL_OBJECT"}})
	if err != nil {
		t.Fatal(err)
	}
	request.ExpectedRevision = u.Revision
	partID := strings.Repeat("3", 32)
	if err = m.BindR1(ctx, r.Fence, s.ObjectHead{Object: r.Object, ManifestID: partID}); err != nil {
		t.Fatal(err)
	}
	part := s.PartPublisher{ID: strings.Repeat("4", 32), UploadID: uid, OpID: r.OpID, ManifestID: partID, PartNumber: 1, Bytes: 4, Writer: r.Fence, Deadline: f.Now.Add(time.Minute)}
	if _, err = m.RegisterPartPublisher(ctx, request, part); err != nil {
		t.Fatal("multipart prepare", err)
	}
	if _, _, err = m.PutChunkIfAbsent(ctx, f.Chunk("default"), r.Fence); err != nil {
		t.Fatal(err)
	}
	if err = m.WritePartPages(ctx, []s.ManifestPage{{Version: 1, ManifestID: partID, Domain: "default", Chunks: []s.ChunkRef{{Key: f.Chunk("default").Key, RawBytes: 4}}}}, r.Fence); err != nil {
		t.Fatal(err)
	}
	publish := s.PartPublishRequest{Request: request, PublisherID: part.ID, Part: s.UploadPart{Version: 1, Number: 1, Size: 4, ManifestID: partID, Pages: 1, ETag: strings.Repeat("a", 32), Checksum: s.Checksum{Algorithm: "CRC32C", Type: "FULL_OBJECT", Value: "AAAAAA=="}}}
	if _, err = m.PublishPart(ctx, publish); err != nil {
		t.Fatal("multipart publish", err)
	}
	if err = peer.CloseR1(ctx, r.Fence); err != nil {
		t.Fatal(err)
	}
	if _, err = peer.PublishPart(ctx, publish); !errors.Is(err, s.ErrFenced) {
		t.Fatal("stale multipart publication accepted", err)
	}
	part.ID = strings.Repeat("5", 32)
	if _, err = peer.RegisterPartPublisher(ctx, request, part); !errors.Is(err, s.ErrFenced) {
		t.Fatal("stale multipart worker admitted", err)
	}
	if _, err = peer.GetUpload(ctx, uid); err != nil {
		t.Fatal("closure removed upload", err)
	}
}
