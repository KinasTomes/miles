package badger

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/contracttest"
	db "github.com/dgraph-io/badger/v4"
)

func reopen(t *testing.T, m *Store, path string, now time.Time) *Store {
	t.Helper()
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	n, err := Open(Options{Path: path, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return n
}
func TestLostACKAndOverwriteSurviveReopen(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, path := fixtureStore(t, f)
	r := f.Request("lost-ack", 0)
	prepare(t, m, f, r)
	m.afterPublish = func() error { return s.ErrUnavailable }
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	m = reopen(t, m, path, f.Now)
	defer func() { _ = m.Close() }()
	first, err := m.CommitObject(ctx, r)
	if err != nil || !first.AlreadyCommitted {
		t.Fatalf("retry: %+v %v", first, err)
	}
	second := f.Request("overwrite", 1)
	prepare(t, m, f, second)
	next, err := m.CommitObject(ctx, second)
	if err != nil || next.Head.Revision != 2 {
		t.Fatalf("overwrite: %+v %v", next, err)
	}
	m = reopen(t, m, path, f.Now)
	again, err := m.CommitObject(ctx, r)
	if err != nil || !again.AlreadyCommitted || !reflect.DeepEqual(again.Head, first.Head) {
		t.Fatalf("original outcome lost: %+v %v", again, err)
	}
	head, err := m.GetObject(ctx, r.Head.Object)
	if err != nil || head.OpID != second.Head.OpID || head.Revision != 2 {
		t.Fatalf("retry rewound head: %+v %v", head, err)
	}
	q, err := m.GetQuota(ctx, "owner")
	if err != nil || q.Used != 4 || q.Reserved != 0 || q.Staged != 0 {
		t.Fatalf("quota: %+v %v", q, err)
	}
	if err = m.ReleaseLogicalQuota(ctx, s.ReservationRelease{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 2, Writer: r.Writer}); !errors.Is(err, s.ErrConflict) {
		t.Fatalf("released committed quota: %v", err)
	}
}

// A separate process exits without Close: the test proves recovery does not
// depend on graceful shutdown or an authoritative in-process map.
func TestUncleanExitRecovery(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	if path := os.Getenv("MILES_BADGER_CRASH_TEST_PATH"); path != "" {
		m, err := Open(Options{Path: path, Now: func() time.Time { return f.Now }})
		if err != nil {
			t.Fatal(err)
		}
		u := s.Completion{UploadID: "upload", BucketID: f.Bucket.ID, Key: "file", Initiator: "writer", State: "Completing", Revision: 1, Fence: 1}
		q := f.Quota
		q.ActiveUploads = 1
		err = m.Bootstrap(ctx, Bootstrap{Buckets: []s.Bucket{f.Bucket}, Users: f.Users, Credentials: f.Credentials, Grants: []s.Grant{f.Grant}, Quotas: []s.LogicalQuota{q}, Packs: f.Packs, Writers: []s.WriterLease{f.Lease}, Uploads: []s.Completion{u}})
		if err != nil {
			t.Fatal(err)
		}
		r := f.Request("crash", 0)
		r.Completion = &u
		r.Receipt = &s.CompletedReceipt{Version: 1, UploadID: u.UploadID, BucketID: u.BucketID, Key: u.Key, Initiator: u.Initiator, OpID: r.Head.OpID, InputDigest: strings.Repeat("c", 64), ManifestID: r.Head.ManifestID, ETag: r.Head.ETag, ExpiresAt: f.Now.Add(time.Hour)}
		prepare(t, m, f, r)
		if _, err = m.CommitObject(ctx, r); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	path := t.TempDir()
	childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestUncleanExitRecovery$")
	cmd.Env = append(os.Environ(), "MILES_BADGER_CRASH_TEST_PATH="+path)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, b)
	}
	m, err := Open(Options{Path: path, Now: func() time.Time { return f.Now }})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	r := f.Request("crash", 0)
	head, err := m.GetObject(ctx, r.Head.Object)
	if err != nil || head.Revision != 1 {
		t.Fatalf("head: %+v %v", head, err)
	}
	if receipt, err := m.GetCompletionReceipt(ctx, "upload"); err != nil || receipt.OpID != r.Head.OpID {
		t.Fatalf("receipt: %+v %v", receipt, err)
	}
	if _, err = m.GetOutbox(ctx, r.Event.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = m.GetManifestPage(ctx, r.Head.ManifestID, 0); err != nil {
		t.Fatal(err)
	}
	q, err := m.GetQuota(ctx, "owner")
	if err != nil || q.Used != 4 || q.Reserved != 0 || q.Staged != 0 || q.ActiveUploads != 0 {
		t.Fatalf("counters: %+v %v", q, err)
	}
	resolution, err := m.ClaimReservation(ctx, s.ReservationClaim{ID: r.ReservationID, OpID: r.Head.OpID, ExpectedRevision: 1, Writer: r.Writer})
	if err != nil || resolution.Committed == nil || resolution.Committed.Head.OpID != r.Head.OpID {
		t.Fatalf("outcome: %+v %v", resolution, err)
	}
}

func TestManifestPublicationValidation(t *testing.T) {
	for _, name := range []string{"size", "descriptor", "missing-chunk", "gap", "non-greedy", "domain"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := contracttest.NewFixture()
			m, _ := fixtureStore(t, f)
			defer m.Close()
			r := f.Request("validate", 0)
			prepare(t, m, f, r)
			page, err := m.GetManifestPage(ctx, r.Head.ManifestID, 0)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "size":
				r.Head.Size = 3
			case "descriptor":
				r.Head.PageCount = 2
				r.Head.ChunkCount = 1
				r.Head.PageIndex = []uint64{0, 4}
			case "missing-chunk":
				page.Chunks[0].Key.Hash = strings.Repeat("f", 64)
			case "gap":
				page.Number = 1
			case "non-greedy":
				page.Number = 1
				page.Chunks[0].LogicalOffset = 4
			case "domain":
				page.Domain = "other"
				page.Chunks[0].Key.Domain = "other"
			}
			if name == "size" { // Keep reservation binding valid, exercise manifest size itself.
				err = m.update(ctx, func(tx *db.Txn) error {
					v, e := get[s.Reservation](tx, key("reservations", r.ReservationID))
					if e != nil {
						return e
					}
					v.StagedBytes = 3
					return put(tx, key("reservations", v.ID), v)
				})
			} else if name != "descriptor" {
				err = m.update(ctx, func(tx *db.Txn) error {
					if name != "non-greedy" {
						if e := tx.Delete([]byte(pageKey(page.ManifestID, 0))); e != nil {
							return e
						}
					}
					b, e := manifest.Encode(manifest.ObjectPage, page)
					if e != nil {
						return e
					}
					return tx.Set([]byte(pageKey(page.ManifestID, page.Number)), b)
				})
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.CommitObject(ctx, r); err == nil {
				t.Fatal("invalid manifest published")
			}
			contracttest.NoPublication(t, m, r)
		})
	}
}

func TestManifestSealedAndEmptyObject(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, _ := fixtureStore(t, f)
	defer m.Close()
	r := f.Request("sealed", 0)
	prepare(t, m, f, r)
	if _, err := m.CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
	p, err := m.GetManifestPage(ctx, r.Head.ManifestID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.WriteManifestPages(ctx, []s.ManifestPage{p}, r.Writer); err != nil {
		t.Fatal("identical retry", err)
	}
	p.Number = 1
	p.Chunks[0].LogicalOffset = 4
	if err = m.WriteManifestPages(ctx, []s.ManifestPage{p}, r.Writer); !errors.Is(err, s.ErrConflict) {
		t.Fatalf("appended after publication: %v", err)
	}
	empty := f.Request("empty", 1)
	empty.Head.Size = 0
	res := s.Reservation{Version: 1, ID: empty.ReservationID, OpID: empty.Head.OpID, OwnerID: "owner", Object: empty.Head.Object, BaseRevision: 1, BaseSize: 4, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(time.Minute), Writer: empty.Writer}
	if err = m.ReserveLogicalQuota(ctx, res); err != nil {
		t.Fatal(err)
	}
	v, err := m.CommitObject(ctx, empty)
	if err != nil || v.Head.PageCount != 0 || v.Head.ChunkCount != 0 || len(v.Head.PageIndex) != 0 {
		t.Fatalf("empty: %+v %v", v, err)
	}
	if pages, err := m.GetManifest(ctx, empty.Head.ManifestID); err != nil || len(pages) != 0 {
		t.Fatalf("read empty manifest: %v", err)
	}
	p.ManifestID = empty.Head.ManifestID
	p.Number = 0
	p.Chunks[0].LogicalOffset = 0
	if err = m.WriteManifestPages(ctx, []s.ManifestPage{p}, r.Writer); !errors.Is(err, s.ErrConflict) {
		t.Fatalf("appended into empty object: %v", err)
	}
}

func TestCorruptChunkRequiredCRCAndSchema(t *testing.T) {
	for _, name := range []string{"missing-crc", "null-crc", "unknown-schema"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := contracttest.NewFixture()
			m, _ := fixtureStore(t, f)
			defer m.Close()
			c := f.Chunk("default")
			if _, _, err := m.PutChunkIfAbsent(ctx, c, f.Lease.Fence); err != nil {
				t.Fatal(err)
			}
			err := m.update(ctx, func(tx *db.Txn) error {
				b, e := pageBytes(tx, chunkKey(c.Key))
				if e != nil {
					return e
				}
				var envelope map[string]any
				if e = json.Unmarshal(b, &envelope); e != nil {
					return e
				}
				if name == "unknown-schema" {
					envelope["Schema"] = 2
				} else {
					loc := envelope["Value"].(map[string]any)["Location"].(map[string]any)
					if name == "missing-crc" {
						delete(loc, "CRC32C")
					} else {
						loc["CRC32C"] = nil
					}
				}
				b, e = json.Marshal(envelope)
				if e != nil {
					return e
				}
				return tx.Set([]byte(chunkKey(c.Key)), b)
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = m.GetChunks(ctx, []s.ChunkKey{c.Key}, true); !errors.Is(err, s.ErrUnavailable) {
				t.Fatalf("corruption accepted: %v", err)
			}
		})
	}
}

func TestOrphanSurvivesRestartWithoutHead(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, path := fixtureStore(t, f)
	r := f.Request("orphan", 0)
	prepare(t, m, f, r)
	m = reopen(t, m, path, f.Now)
	defer m.Close()
	contracttest.NoPublication(t, m, r)
	if _, err := m.GetManifestPage(ctx, r.Head.ManifestID, 0); err != nil {
		t.Fatal(err)
	}
	q, err := m.GetQuota(ctx, "owner")
	if err != nil || q.Reserved != 4 || q.Staged != 4 || q.Used != 0 {
		t.Fatalf("orphan reservation lost: %+v %v", q, err)
	}
	if _, err = m.CommitObject(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func TestMultiPageCommitAndIndexedRead(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	f.Quota.Limit = 1 << 20
	m, _ := fixtureStore(t, f)
	defer m.Close()
	r := f.Request("multi", 0)
	capacity, err := manifest.RefCapacity(r.Head.ManifestID, r.Head.Domain)
	if err != nil {
		t.Fatal(err)
	}
	count := capacity + 1
	r.Head.Size = int64(count * 4)
	c := f.Chunk("default")
	if _, _, err = m.PutChunkIfAbsent(ctx, c, r.Writer); err != nil {
		t.Fatal(err)
	}
	i := 0
	err = manifest.Paginate(manifest.ObjectPage, r.Head.ManifestID, r.Head.Domain, func() (s.ChunkRef, error) {
		if i == count {
			return s.ChunkRef{}, io.EOF
		}
		v := s.ChunkRef{Key: c.Key, RawBytes: 4, LogicalOffset: int64(i * 4)}
		i++
		return v, nil
	}, func(p s.ManifestPage) error { return m.WriteManifestPages(ctx, []s.ManifestPage{p}, r.Writer) })
	if err != nil {
		t.Fatal(err)
	}
	res := s.Reservation{Version: 1, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: "owner", Object: r.Head.Object, Bytes: r.Head.Size, StagedBytes: r.Head.Size, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(time.Minute), Writer: r.Writer}
	if err = m.ReserveLogicalQuota(ctx, res); err != nil {
		t.Fatal(err)
	}
	out, err := m.CommitObject(ctx, r)
	if err != nil || out.Head.PageCount != 2 || out.Head.ChunkCount != uint64(count) || !reflect.DeepEqual(out.Head.PageIndex, []uint64{0, uint64(capacity * 4)}) {
		t.Fatalf("multi head: %+v %v", out, err)
	}
	p, err := m.GetManifestPage(ctx, r.Head.ManifestID, 1)
	if err != nil || len(p.Chunks) != 1 || p.Chunks[0].LogicalOffset != int64(capacity*4) {
		t.Fatalf("indexed page: %+v %v", p, err)
	}
}

func TestOversizedCommitDoesNotPartiallyPublish(t *testing.T) {
	ctx := context.Background()
	f := contracttest.NewFixture()
	m, path := fixtureStore(t, f)
	// Lower the real transaction budget and keep the two ~100 KiB records
	// inline, so head+outcome exceed Badger's limit without allocating huge data.
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(db.DefaultOptions(path).WithSyncWrites(true).WithLogger(nil).WithMemTableSize(1 << 20).WithValueThreshold(128 << 10))
	if err != nil {
		t.Fatal(err)
	}
	m.db = d
	defer m.Close()
	r := f.Request("large", 0)
	prepare(t, m, f, r)
	r.Head.Metadata["oversize"] = strings.Repeat("x", 100<<10)
	if _, err := m.CommitObject(ctx, r); !errors.Is(err, s.ErrInvalid) {
		t.Fatalf("large transaction: %v", err)
	}
	contracttest.NoPublication(t, m, r)
	q, err := m.GetQuota(ctx, "owner")
	if err != nil || q.Used != 0 || q.Reserved != 4 {
		t.Fatalf("partial quota: %+v %v", q, err)
	}
	delete(r.Head.Metadata, "oversize")
	if _, err = m.CommitObject(ctx, r); err != nil {
		t.Fatal("failed commit sealed manifest", err)
	}
}
