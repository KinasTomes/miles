package badger

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
)

func fixtureStore(t *testing.T, f contracttest.Fixture) (*Store, string) {
	t.Helper()
	path := t.TempDir()
	m, err := Open(Options{Path: path, Now: func() time.Time { return f.Now }})
	if err != nil {
		t.Fatal(err)
	}
	seed := Bootstrap{Buckets: []s.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []s.Grant{f.Grant}, Quotas: []s.LogicalQuota{f.Quota}, Packs: f.Packs, Writers: []s.WriterLease{f.Lease}}
	if f.Upload != nil {
		seed.Uploads = []s.Completion{*f.Upload}
	}
	if err = m.Bootstrap(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	return m, path
}
func prepare(t *testing.T, m *Store, f contracttest.Fixture, r s.CommitRequest) {
	t.Helper()
	ctx := context.Background()
	c := f.Chunk(f.Bucket.Domain)
	if _, _, err := m.PutChunkIfAbsent(ctx, c, r.Writer); err != nil {
		t.Fatal(err)
	}
	if err := m.WriteManifestPages(ctx, []s.ManifestPage{{Version: 1, ManifestID: r.Head.ManifestID, Domain: c.Key.Domain, Chunks: []s.ChunkRef{{Key: c.Key, RawBytes: 4}}}}, r.Writer); err != nil {
		t.Fatal(err)
	}
	base, err := m.GetObject(ctx, r.Head.Object)
	if err != nil && !errors.Is(err, s.ErrNotFound) {
		t.Fatal(err)
	}
	res := s.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: f.Quota.OwnerID, Object: r.Head.Object, Bytes: max(0, r.Head.Size-base.Size), StagedBytes: r.Head.Size, BaseRevision: base.Revision, BaseSize: base.Size, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(time.Hour), Writer: r.Writer}
	if r.Receipt != nil {
		res.UploadID = r.Receipt.UploadID
	}
	if err := m.ReserveLogicalQuota(ctx, res); err != nil {
		t.Fatal(err)
	}
}
func TestDurableFoundation(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, path := fixtureStore(t, f)
	r := f.Request("durable", 0)
	prepare(t, m, f, r)
	if _, err := m.GetObject(ctx, r.Head.Object); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("orphan published: %v", err)
	}
	first, err := m.CommitObject(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if first.Head.PageCount != 1 || first.Head.ChunkCount != 1 || !reflect.DeepEqual(first.Head.PageIndex, []uint64{0}) {
		t.Fatalf("descriptor: %+v", first.Head)
	}
	if err = m.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = Open(Options{Path: path, Now: func() time.Time { return f.Now }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	again, err := m.CommitObject(ctx, r)
	if err != nil || !again.AlreadyCommitted || !reflect.DeepEqual(again.Head, first.Head) {
		t.Fatalf("reopen retry: %+v %v", again, err)
	}
	q, err := m.GetQuota(ctx, "owner")
	if err != nil || q.Used != 4 || q.Reserved != 0 || q.Staged != 0 {
		t.Fatalf("quota: %+v %v", q, err)
	}
	if _, err = m.GetOutbox(ctx, r.Event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = m.GetManifestPage(ctx, r.Head.ManifestID, 0); err != nil {
		t.Fatal(err)
	}
	if err = m.Bootstrap(ctx, Bootstrap{}); !errors.Is(err, s.ErrConflict) {
		t.Fatalf("reseed: %v", err)
	}
}
func TestAtomicAbort(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	defer m.Close()
	r := f.Request("abort", 0)
	prepare(t, m, f, r)
	m.beforePublish = func() error { return s.ErrUnavailable }
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	if _, err := m.GetObject(ctx, r.Head.Object); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("head escaped: %v", err)
	}
	if _, err := m.GetOutbox(ctx, r.Event.ID); !errors.Is(err, s.ErrNotFound) {
		t.Fatalf("event escaped: %v", err)
	}
	q, err := m.GetQuota(ctx, "owner")
	if err != nil || q.Used != 0 || q.Reserved != 4 || q.Staged != 4 {
		t.Fatalf("quota escaped: %+v %v", q, err)
	}
	m.beforePublish = nil
	if _, err = m.CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
}
