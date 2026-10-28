package write

import (
	"bytes"
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

type fenceRecorder struct {
	CoordinationStore
	seen map[string]storage.WriterFence
}

func (s *fenceRecorder) RegisterUploadingPack(ctx context.Context, p storage.PackInfo, f storage.WriterFence) error {
	s.seen["register_pack"] = f
	if p.Writer != f {
		return storage.ErrFenced
	}
	return s.CoordinationStore.RegisterUploadingPack(ctx, p, f)
}
func (s *fenceRecorder) MarkPackWarm(ctx context.Context, p storage.PackRef, r uint64, f storage.WriterFence) (storage.PackInfo, error) {
	s.seen["warm"] = f
	return s.CoordinationStore.MarkPackWarm(ctx, p, r, f)
}
func (s *fenceRecorder) PutChunkIfAbsent(ctx context.Context, c storage.Chunk, f storage.WriterFence) (storage.Chunk, bool, error) {
	s.seen["chunk"] = f
	return s.CoordinationStore.PutChunkIfAbsent(ctx, c, f)
}
func (s *fenceRecorder) WriteManifestPages(ctx context.Context, p []storage.ManifestPage, f storage.WriterFence) error {
	s.seen["manifest"] = f
	return s.CoordinationStore.WriteManifestPages(ctx, p, f)
}
func (s *fenceRecorder) ReserveLogicalQuota(ctx context.Context, r storage.Reservation) error {
	s.seen["reserve"] = r.Writer
	return s.CoordinationStore.ReserveLogicalQuota(ctx, r)
}
func (s *fenceRecorder) CommitObject(ctx context.Context, r storage.CommitRequest) (storage.CommitResult, error) {
	s.seen["commit"] = r.Writer
	return s.CoordinationStore.CommitObject(ctx, r)
}

type coordinatedFixture struct {
	c        *clock.Virtual
	gate     *coordination.WarmGate
	registry *coordination.WriterRegistry
	w        *coordination.Writer
	meta     *CoordinatedMetadata
	recorder *fenceRecorder
	packs    storage.PackStore
	physical storage.PackStore
	backend  storage.MetadataStore
	f        contracttest.Fixture
}

func coordinatedFixtureFor(t *testing.T, kind string, hooks coordination.Hooks) *coordinatedFixture {
	t.Helper()
	ctx := context.Background()
	f := contracttest.NewFixture()
	f.Quota.Limit = 1 << 20
	f.Lease.ExpiresAt = f.Now.Add(time.Hour)
	var store CoordinationStore
	if kind == "badger" {
		m, err := badger.Open(badger.Options{Path: t.TempDir(), Now: func() time.Time { return f.Now }})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := m.Close(); err != nil {
				t.Error(err)
			}
		})
		if err := m.Bootstrap(ctx, badger.Bootstrap{Buckets: []storage.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []storage.Grant{f.Grant}, Quotas: []storage.LogicalQuota{f.Quota}, Writers: []storage.WriterLease{f.Lease}}); err != nil {
			t.Fatal(err)
		}
		store = m
	} else {
		store = fake.NewMetadata(fake.Seed{Now: func() time.Time { return f.Now }, Buckets: []storage.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []storage.Grant{f.Grant}, Quotas: []storage.LogicalQuota{f.Quota}, Writers: []storage.WriterLease{f.Lease}})
	}
	c, err := clock.NewVirtual(f.Now, 100)
	if err != nil {
		t.Fatal(err)
	}
	l := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
	g, err := coordination.NewR1Gate(c, f.Lease.Fence.ServiceIncarnation, l)
	if err != nil {
		t.Fatal(err)
	}
	r, err := coordination.NewWriterRegistry(g, l, hooks)
	if err != nil {
		t.Fatal(err)
	}
	d, _ := c.Now().Instant.Add(5 * time.Minute)
	w, err := r.Register(ctx, f.Lease.Fence, d)
	if err != nil {
		t.Fatal(err)
	}
	recorder := &fenceRecorder{CoordinationStore: store, seen: map[string]storage.WriterFence{}}
	physical := fake.NewPacks(nil)
	m, p, err := BindCoordination(recorder, physical, w)
	if err != nil {
		t.Fatal(err)
	}
	return &coordinatedFixture{c: c, gate: g, registry: r, w: w, meta: m, recorder: recorder, packs: p, physical: physical, backend: store.(storage.MetadataStore), f: f}
}
func (f *coordinatedFixture) prepare(t *testing.T) (storage.CommitRequest, error) {
	t.Helper()
	ctx := context.Background()
	body := bytes.Repeat([]byte("R1-stream-body"), 2500)
	pub := &StorePublisher{Metadata: f.meta, Packs: f.packs, Guard: f.meta, Writer: f.w.Fence(), ProtectedUntil: f.f.Now.Add(time.Hour)}
	pipeline := Pipeline{Lookup: f.meta, Publisher: pub, Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10}, Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
	id, err := NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareObject(ctx, pipeline, f.meta, f.w.Fence(), bytes.NewReader(body), "default", id)
	if err != nil {
		return storage.CommitRequest{}, err
	}
	q := f.f.Request("r1-integration", 0)
	q.Head, err = prepared.BindHead(q.Head)
	if err != nil {
		t.Fatal(err)
	}
	err = f.meta.ReserveLogicalQuota(ctx, storage.Reservation{Version: 1, Revision: 1, State: storage.ReservationActive, OpID: q.Head.OpID, ID: q.ReservationID, OwnerID: f.f.Bucket.OwnerID, Object: q.Head.Object, Bytes: prepared.Size, StagedBytes: prepared.Size, Deadline: f.f.Now.Add(time.Hour), Writer: q.Writer})
	return q, err
}
func TestCoordinatedPreparePublishPropagatesExactFence(t *testing.T) {
	for _, kind := range []string{"fake", "badger"} {
		t.Run(kind, func(t *testing.T) {
			f := coordinatedFixtureFor(t, kind, coordination.Hooks{})
			q, err := f.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.backend.GetObject(context.Background(), q.Head.Object); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("prepare published head", err)
			}
			result, err := f.meta.CommitObject(context.Background(), q)
			if err != nil || result.Head.Revision != 1 {
				t.Fatal(result, err)
			}
			for _, stage := range []string{"register_pack", "warm", "chunk", "manifest", "reserve", "commit"} {
				if f.recorder.seen[stage] != f.w.Fence() {
					t.Fatal("fence/incarnation missing", stage, f.recorder.seen)
				}
			}
			// A later PUT can reuse an exact canonical generation via the opaque writer.
			pages, err := f.backend.GetManifest(context.Background(), q.Head.ManifestID)
			if err != nil {
				t.Fatal(err)
			}
			rows, err := f.backend.GetChunks(context.Background(), []storage.ChunkKey{pages[0].Chunks[0].Key}, true)
			if err != nil {
				t.Fatal(err)
			}
			row := rows[pages[0].Chunks[0].Key]
			if ok, err := f.meta.Pin(context.Background(), row); err != nil || !ok {
				t.Fatal(ok, err)
			}
			if !errors.Is(f.gate.Retire(row.Location.Pack), storage.ErrConflict) {
				t.Fatal("reuse lost generation pin")
			}
			bad := q
			bad.Writer.ServiceIncarnation = "old-service"
			if _, err := f.meta.CommitObject(context.Background(), bad); !errors.Is(err, storage.ErrFenced) {
				t.Fatal(err)
			}
			if _, err := f.meta.DeleteObject(context.Background(), bad); !errors.Is(err, storage.ErrFenced) {
				t.Fatal("delete bypassed fence", err)
			}
			stale := row
			stale.LocationRevision++
			if ok, err := f.meta.Pin(context.Background(), stale); ok || !errors.Is(err, storage.ErrConflict) {
				t.Fatal("stale location accepted", ok, err)
			}
			if err := f.w.Release(); err != nil {
				t.Fatal(err)
			}
			if err := f.gate.Retire(row.Location.Pack); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(f.packs.DeletePack(context.Background(), row.Location.Pack), coordination.ErrMaintenanceDisabled) {
				t.Fatal("local retire enabled physical delete")
			}
		})
	}
}
func TestCoordinatedPrepareLosesFenceBeforeNextMutation(t *testing.T) {
	for _, stop := range []string{"pack/", "sidecar/", "manifest"} {
		t.Run(stop, func(t *testing.T) {
			var f *coordinatedFixture
			h := coordination.Hooks{Before: func(_ context.Context, stage string, _ storage.WriterFence) error {
				if strings.HasPrefix(stage, stop) {
					f.w.CloseFence()
				}
				return nil
			}}
			f = coordinatedFixtureFor(t, "fake", h)
			if _, err := f.prepare(t); !errors.Is(err, storage.ErrFenced) {
				t.Fatal("stale prepare continued", err)
			}
			q := f.f.Request("r1-integration", 0)
			contracttest.NoPublication(t, f.backend, q)
			if err := f.w.Release(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCoordinatedUnknownCommitReconcilesOriginalOutcome(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "fail before commit", true: "lost committed ACK"}[committed], func(t *testing.T) {
			h := coordination.Hooks{}
			if committed {
				h.After = func(_ context.Context, stage string, _ storage.WriterFence) error {
					if strings.HasPrefix(stage, "commit/") {
						return errors.New("lost ACK")
					}
					return nil
				}
			}
			f := coordinatedFixtureFor(t, "fake", h)
			q, err := f.prepare(t)
			if err != nil {
				t.Fatal(err)
			}
			if !committed {
				f.backend.(*fake.Metadata).FailCommit(true, false)
			}
			if _, err := f.meta.CommitObject(context.Background(), q); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatal(err)
			}
			f.w.CloseFence()
			if !errors.Is(f.w.Release(), coordination.ErrReconcilePending) {
				t.Fatal("unknown outcome released")
			}
			got, err := f.w.ReconcileCommit(context.Background(), f.meta, q.Head.OpID)
			if committed {
				if err != nil || got.Head.Revision != 1 || got.Head.OpID != q.Head.OpID {
					t.Fatal(got, err)
				}
				if _, err := f.backend.GetOutbox(context.Background(), q.Event.ID); err != nil {
					t.Fatal(err)
				}
				if err := f.w.Release(); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, storage.ErrNotFound) {
					t.Fatal(err)
				}
				if !errors.Is(f.w.Release(), coordination.ErrReconcilePending) {
					t.Fatal("absence became no-commit proof")
				}
				contracttest.NoPublication(t, f.backend, q)
			}
		})
	}
}

func TestCoordinatedBackendFenceLossStopsPrepare(t *testing.T) {
	var f *coordinatedFixture
	h := coordination.Hooks{Before: func(_ context.Context, stage string, _ storage.WriterFence) error {
		if strings.HasPrefix(stage, "warm/") {
			// An authoritative fixture change, not a production lease API.
			next := f.f.Lease
			next.Fence.Epoch++
			f.backend.(*fake.Metadata).SetWriter(next)
		}
		return nil
	}}
	f = coordinatedFixtureFor(t, "fake", h)
	if _, err := f.prepare(t); !errors.Is(err, storage.ErrFenced) {
		t.Fatal("backend fence loss was ignored", err)
	}
	if !errors.Is(f.w.Heartbeat(context.Background()), storage.ErrFenced) {
		t.Fatal("backend fence loss was not sticky")
	}
	if err := f.w.Do(context.Background(), "new physical write", func(storage.WriterFence) error { t.Error("lost worker dispatched again"); return nil }); !errors.Is(err, storage.ErrFenced) {
		t.Fatal(err)
	}
	contracttest.NoPublication(t, f.backend, f.f.Request("r1-integration", 0))
	if err := f.w.Release(); err != nil {
		t.Fatal(err)
	}
}
