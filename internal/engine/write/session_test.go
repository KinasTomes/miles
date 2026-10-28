package write

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/engine"
	"github.com/KinasTomes/miles/internal/pack"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

func sessionOptions(f *coordinatedFixture, label string) SessionOptions {
	q := f.f.Request(label, 0)
	d, _ := f.c.Now().Instant.Add(5 * time.Minute)
	return SessionOptions{Registry: f.registry, Metadata: f.recorder, Packs: f.physical, Fence: f.f.Lease.Fence, Deadline: d, Object: q.Head.Object, OpID: q.Head.OpID, CredentialID: q.CredentialID, ProtectedUntil: f.f.Now.Add(time.Hour)}
}
func openSession(t *testing.T, f *coordinatedFixture, label string) *WriteSession {
	t.Helper()
	s, err := BeginWriteSession(context.Background(), sessionOptions(f, label))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func sessionPipeline() Pipeline {
	return Pipeline{Chunking: engine.ChunkingParams{Min: 16 << 10, Avg: 16 << 10, Max: 16 << 10}, Compression: engine.CompressionPolicy{Enabled: true, Codec: "zstd", Level: 3, MinSavingRatio: big.NewRat(1, 10)}}
}
func prepareSession(t *testing.T, f *coordinatedFixture, s *WriteSession, label string, body []byte) storage.CommitRequest {
	t.Helper()
	id, err := NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Prepare(sessionPipeline(), bytes.NewReader(body), id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Size != int64(len(body)) {
		t.Fatal("wrong descriptor", p)
	}
	// Returned descriptors cannot mutate the session's prepared binding.
	if len(p.PageIndex) != 0 {
		p.PageIndex[0]++
	}
	q := f.f.Request(label, 0)
	q.Writer = s.writer.Fence()
	if err := s.Reserve(storage.Reservation{Version: 1, Revision: 1, State: storage.ReservationActive, ID: q.ReservationID, OpID: q.Head.OpID, Object: q.Head.Object, OwnerID: f.f.Bucket.OwnerID, Bytes: int64(len(body)), StagedBytes: int64(len(body)), Deadline: f.f.Now.Add(time.Hour), Writer: s.writer.Fence()}); err != nil {
		t.Fatal(err)
	}
	return q
}
func TestWriteSessionOwnsPreparePublishAndCleanup(t *testing.T) {
	for _, kind := range []string{"fake", "badger"} {
		t.Run(kind, func(t *testing.T) {
			f := coordinatedFixtureFor(t, kind, coordination.Hooks{})
			s := openSession(t, f, "managed")
			body := bytes.Repeat([]byte("managed-body"), 3500)
			q := prepareSession(t, f, s, "managed", body)
			bad := q
			bad.Head.Object.Key = "another-object"
			if _, err := s.Commit(bad); !errors.Is(err, storage.ErrInvalid) {
				t.Fatal("object binding bypass", err)
			}
			bad = q
			bad.Writer.Epoch++
			if _, err := s.Commit(bad); !errors.Is(err, storage.ErrInvalid) {
				t.Fatal("writer binding bypass", err)
			}
			result, err := s.Commit(q)
			if err != nil || result.Head.Revision != 1 || result.Head.Size != int64(len(body)) {
				t.Fatal(result, err)
			}
			event, err := f.backend.GetOutbox(context.Background(), q.Event.ID)
			if err != nil || event.HistoryID != q.Event.HistoryID || event.Epoch != q.Event.Epoch {
				t.Fatal("R1 fence replaced R2 event identity", event, err)
			}
			if err := result.Head.ValidateManifestDescriptor(); err != nil {
				t.Fatal("descriptor alias", err)
			}
			pages, err := f.backend.GetManifest(context.Background(), result.Head.ManifestID)
			if err != nil {
				t.Fatal(err)
			}
			var raw []byte
			for _, page := range pages {
				for _, ref := range page.Chunks {
					rows, err := f.backend.GetChunks(context.Background(), []storage.ChunkKey{ref.Key}, true)
					if err != nil {
						t.Fatal(err)
					}
					b, err := pack.ReadCanonicalChunk(context.Background(), f.physical, rows[ref.Key])
					if err != nil {
						t.Fatal(err)
					}
					raw = append(raw, b...)
				}
			}
			if !bytes.Equal(raw, body) {
				t.Fatal("published body mismatch")
			}
			for _, stage := range []string{"register_pack", "warm", "chunk", "manifest", "reserve", "commit"} {
				if f.recorder.seen[stage] != s.writer.Fence() {
					t.Fatal(stage, f.recorder.seen)
				}
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := s.Close(); err != nil {
				t.Fatal("double close", err)
			}
			if f.c.Pending() != 0 {
				t.Fatal("heartbeat not joined")
			}
			for _, page := range pages {
				for _, ref := range page.Chunks {
					rows, _ := f.backend.GetChunks(context.Background(), []storage.ChunkKey{ref.Key}, true)
					if err := f.gate.Retire(rows[ref.Key].Location.Pack); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestWriteSessionReauthorizesAtPublication(t *testing.T) {
	f := coordinatedFixtureFor(t, "fake", coordination.Hooks{})
	s := openSession(t, f, "revoke-write")
	q := prepareSession(t, f, s, "revoke-write", bytes.Repeat([]byte("body"), 5000))
	grant := f.f.Grant
	grant.Write = false
	f.backend.(*fake.Metadata).SetGrant(grant)
	if _, err := s.Commit(q); !errors.Is(err, storage.ErrAccessDenied) {
		t.Fatal("write used stale admission permission", err)
	}
	if _, err := f.backend.GetObject(context.Background(), q.Head.Object); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("denied publication changed head", err)
	}
	if _, err := f.backend.GetOutbox(context.Background(), q.Event.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("denied publication created event", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteSessionRecoveryIncarnationStopsPrepare(t *testing.T) {
	var f *coordinatedFixture
	h := coordination.Hooks{Before: func(_ context.Context, stage string, _ storage.WriterFence) error {
		if strings.HasPrefix(stage, "warm/") {
			next := f.f.Lease
			next.Fence.ServiceIncarnation = "service-after-recovery"
			f.backend.(*fake.Metadata).SetWriter(next)
		}
		return nil
	}}
	f = coordinatedFixtureFor(t, "fake", h)
	s := openSession(t, f, "new-incarnation")
	id, err := NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(sessionPipeline(), bytes.NewReader(bytes.Repeat([]byte("body"), 5000)), id); !errors.Is(err, storage.ErrFenced) {
		t.Fatal("old incarnation prepared after recovery", err)
	}
	if !errors.Is(s.writer.Heartbeat(context.Background()), storage.ErrFenced) {
		t.Fatal("old incarnation resurrected")
	}
	q := f.f.Request("new-incarnation", 0)
	if _, err := f.backend.GetObject(context.Background(), q.Head.Object); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("stale worker published head", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestWriteSessionAuthorizationAndDuplicateOwner(t *testing.T) {
	var registrations atomic.Int32
	f := coordinatedFixtureFor(t, "fake", coordination.Hooks{Before: func(_ context.Context, stage string, _ storage.WriterFence) error {
		if stage == "register" {
			registrations.Add(1)
		}
		return nil
	}})
	before := registrations.Load()
	o := sessionOptions(f, "authorization")
	o.CredentialID = "missing"
	if _, err := BeginWriteSession(context.Background(), o); !errors.Is(err, storage.ErrAccessDenied) {
		t.Fatal(err)
	}
	if registrations.Load() != before {
		t.Fatal("registration before authorization")
	}
	o = sessionOptions(f, "authorization")
	o.Object.BucketID = "other-incarnation"
	if _, err := BeginWriteSession(context.Background(), o); !errors.Is(err, storage.ErrConflict) {
		t.Fatal(err)
	}
	if registrations.Load() != before {
		t.Fatal("stale incarnation admitted")
	}
	s := openSession(t, f, "authorization")
	if _, err := BeginWriteSession(context.Background(), sessionOptions(f, "another-op")); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("duplicate executing owner", err)
	}
	q := prepareSession(t, f, s, "authorization", bytes.Repeat([]byte("data"), 5000))
	if _, err := s.Commit(q); err != nil {
		t.Fatal("duplicate attempt closed owner", err)
	}
}

type sessionClock struct {
	*clock.Virtual
	registered chan struct{}
}

func (c sessionClock) Sleep(ctx context.Context, d time.Duration) error {
	wake := make(chan struct{})
	event, err := c.AfterFunc(d, func() { close(wake) })
	if err != nil {
		return err
	}
	defer event.Cancel()
	select {
	case c.registered <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-wake:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type blockedBody struct {
	io.Reader
	entered, unblock chan struct{}
	once             bool
}

func (b *blockedBody) Read(p []byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.entered)
		<-b.unblock
	}
	return b.Reader.Read(p)
}

func TestWriteSessionHeartbeatAndCopiedCloseWaitForBodyQuiescence(t *testing.T) {
	f := coordinatedFixtureFor(t, "fake", coordination.Hooks{})
	c := sessionClock{Virtual: f.c, registered: make(chan struct{}, 1)}
	limits := coordination.LeaseLimits{Lifetime: time.Minute, Heartbeat: 15 * time.Second}
	g, err := coordination.NewR1Gate(c, f.f.Lease.Fence.ServiceIncarnation, limits)
	if err != nil {
		t.Fatal(err)
	}
	r, err := coordination.NewWriterRegistry(g, limits, coordination.Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	f.gate, f.registry = g, r
	s := openSession(t, f, "blocked-body")
	body := &blockedBody{Reader: bytes.NewReader(bytes.Repeat([]byte("body"), 5000)), entered: make(chan struct{}), unblock: make(chan struct{})}
	var releaseOnce sync.Once
	releaseBody := func() { releaseOnce.Do(func() { close(body.unblock) }) }
	defer releaseBody()
	id, err := NewManifestID()
	if err != nil {
		t.Fatal(err)
	}
	prepared := make(chan error, 1)
	go func() { _, err := s.Prepare(sessionPipeline(), body, id); prepared <- err }()
	<-body.entered
	waitTimer := func() {
		select {
		case <-c.registered:
		case <-s.ctx.Done():
			t.Fatal("heartbeat stopped before expected renewal")
		}
	}
	for i := 0; i < 5; i++ {
		waitTimer()
		if err := f.c.Advance(15 * time.Second); err != nil {
			t.Fatal(err)
		}
	}
	waitTimer() // renewal completed, now beyond the initial 60s lease
	if err := s.writer.Check(context.Background()); err != nil {
		t.Fatal("managed heartbeat failed", err)
	}
	copy := *s
	closed := make(chan error, 1)
	go func() { closed <- copy.Close() }()
	<-s.ctx.Done() // cancellation happened; body still owns the work token
	probe := storage.PackRef{ID: "probe", Generation: "1"}
	if !errors.Is(g.Retire(probe), storage.ErrConflict) {
		t.Fatal("cancellation released CPU/body protection")
	}
	releaseBody()
	if err := <-prepared; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	if f.c.Pending() != 0 {
		t.Fatal("heartbeat timer leaked")
	}
	if err := g.Retire(probe); err != nil {
		t.Fatal(err)
	}
}

func TestWriteSessionTwoWorkerFenceClosureBeforePublication(t *testing.T) {
	entered, proceed := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(proceed) }) }
	defer release()
	var firstOp string
	h := coordination.Hooks{Before: func(_ context.Context, stage string, _ storage.WriterFence) error {
		if stage == "commit/"+firstOp {
			close(entered)
			<-proceed
		}
		return nil
	}}
	f := coordinatedFixtureFor(t, "fake", h)
	a := openSession(t, f, "fenced-A")
	firstOp = a.opID
	qA := prepareSession(t, f, a, "fenced-A", bytes.Repeat([]byte("first"), 4000))
	next := f.f.Lease
	next.Fence.LeaseID = "worker-B"
	f.backend.(*fake.Metadata).SetWriter(next)
	o := sessionOptions(f, "winning-B")
	o.Fence = next.Fence
	b, err := BeginWriteSession(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	qB := prepareSession(t, f, b, "winning-B", bytes.Repeat([]byte("second"), 4000))
	done := make(chan error, 1)
	go func() { _, err := a.Commit(qA); done <- err }()
	<-entered
	a.writer.CloseFence()
	if _, err := b.Commit(qB); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; !errors.Is(err, storage.ErrFenced) {
		t.Fatal("old worker published after closure", err)
	}
	head, err := f.backend.GetObject(context.Background(), qA.Head.Object)
	if err != nil || head.OpID != qB.Head.OpID {
		t.Fatal(head, err)
	}
	if _, err := f.backend.GetOutbox(context.Background(), qA.Event.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("fenced worker published event", err)
	}
}

func TestWriteSessionUnknownOutcomeAndIndependentReconciliation(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown before commit", true: "lost committed ACK"}[committed], func(t *testing.T) {
			h := coordination.Hooks{After: func(_ context.Context, stage string, _ storage.WriterFence) error {
				if committed && strings.HasPrefix(stage, "commit/") {
					return errors.New("lost ACK")
				}
				return nil
			}}
			f := coordinatedFixtureFor(t, "fake", h)
			s := openSession(t, f, "managed-unknown")
			q := prepareSession(t, f, s, "managed-unknown", bytes.Repeat([]byte("body"), 5000))
			if !committed {
				f.backend.(*fake.Metadata).FailCommit(true, false)
			}
			if _, err := s.Commit(q); !errors.Is(err, storage.ErrUnavailable) {
				t.Fatal(err)
			}
			if !errors.Is(s.Close(), coordination.ErrReconcilePending) {
				t.Fatal("unknown released")
			}
			probe := storage.PackRef{ID: "probe", Generation: "1"}
			if !errors.Is(f.gate.Retire(probe), storage.ErrConflict) {
				t.Fatal("unknown lost guard")
			}
			got, err := s.Reconcile(context.Background())
			if committed {
				if err != nil || got.Head.OpID != q.Head.OpID || got.Head.Revision != 1 {
					t.Fatal(got, err)
				}
				if err := f.gate.Retire(probe); err != nil {
					t.Fatal(err)
				}
			} else {
				if !errors.Is(err, storage.ErrNotFound) {
					t.Fatal(err)
				}
				if !errors.Is(f.gate.Retire(probe), storage.ErrConflict) {
					t.Fatal("absence became no-commit proof")
				}
			}
		})
	}
}

func TestWriteSessionTwoWorkersHeadCAS(t *testing.T) {
	for _, first := range []int{0, 1} {
		t.Run(map[int]string{0: "A wins", 1: "B wins"}[first], func(t *testing.T) {
			f := coordinatedFixtureFor(t, "fake", coordination.Hooks{})
			a := openSession(t, f, "worker-A")
			next := f.f.Lease
			next.Fence.LeaseID = "worker-B"
			f.backend.(*fake.Metadata).SetWriter(next)
			o := sessionOptions(f, "worker-B")
			o.Fence = next.Fence
			b, err := BeginWriteSession(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = b.Close() })
			sessions := []*WriteSession{a, b}
			labels := []string{"worker-A", "worker-B"}
			requests := make([]storage.CommitRequest, 2)
			// Prepare both against absent head; force BOTH CAS LP orderings rather
			// than allowing host scheduling to choose the expected winner.
			for i, s := range sessions {
				id, err := NewManifestID()
				if err != nil {
					t.Fatal(err)
				}
				body := bytes.Repeat([]byte(labels[i]), 3000)
				p, err := s.Prepare(sessionPipeline(), bytes.NewReader(body), id)
				if err != nil {
					t.Fatal(err)
				}
				q := f.f.Request(labels[i], 0)
				q.Writer = s.writer.Fence()
				requests[i] = q
				if err := s.Reserve(storage.Reservation{Version: 1, Revision: 1, State: storage.ReservationActive, ID: q.ReservationID, OpID: q.Head.OpID, Object: q.Head.Object, OwnerID: f.f.Bucket.OwnerID, Bytes: p.Size, StagedBytes: p.Size, Deadline: f.f.Now.Add(time.Hour), Writer: q.Writer}); err != nil {
					t.Fatal(err)
				}
			}
			winner, err := sessions[first].Commit(requests[first])
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sessions[1-first].Commit(requests[1-first]); !errors.Is(err, storage.ErrConflict) {
				t.Fatal("stale publication silently rebased", err)
			}
			head, err := f.backend.GetObject(context.Background(), requests[first].Head.Object)
			if err != nil || head.OpID != winner.Head.OpID || head.Revision != 1 {
				t.Fatal(head, err)
			}
			if _, err := f.backend.GetOutbox(context.Background(), requests[1-first].Event.ID); !errors.Is(err, storage.ErrNotFound) {
				t.Fatal("partial loser outbox", err)
			}
			for _, s := range sessions {
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
