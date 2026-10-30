// Package contracttest is the reusable backend acceptance suite. Factories must
// return isolated stores and control hooks for authoritative fixture changes.
package contracttest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/manifest"
	s "github.com/KinasTomes/miles/internal/storage"
)

type Fixture struct {
	Now         time.Time
	Bucket      s.Bucket
	Users       []s.User
	Credentials []s.Credential
	Grant       s.Grant
	Quota       s.LogicalQuota
	Packs       []s.PackInfo
	Lease       s.WriterLease
	Upload      *s.Completion
}
type MetadataHarness struct {
	FailReconcile func(bool)
	Store         s.MetadataStore
	SetBucket     func(s.Bucket)
	RemoveBucket  func(string)
	SetCredential func(s.Credential)
	SetUser       func(s.User)
	SetGrant      func(s.Grant)
	SetWriter     func(s.WriterLease)
	SetPack       func(s.PackInfo)
	SetQuota      func(s.LogicalQuota)
	FailCommit    func(before, after bool)
}
type MetadataFactory func(*testing.T, Fixture) MetadataHarness

const fixtureHistoryID = "11111111111111111111111111111111"

func fixtureOpID(label string) string {
	sum := sha256.Sum256([]byte(label))
	return hex.EncodeToString(sum[:16])
}

func fixtureEventID(opID, entity, action string) string {
	id, err := s.EventID(fixtureHistoryID, opID, entity, action)
	if err != nil {
		panic(err)
	}
	return id
}

func NewFixture() Fixture {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	return Fixture{Now: now, Bucket: s.Bucket{Name: "demo", ID: "bucket-1", OwnerID: "owner", Domain: "default", Revision: 1, Active: true}, Users: []s.User{{ID: "owner", Active: true, Revision: 1}, {ID: "writer", Active: true, Revision: 1}, {ID: "admin", Active: true, Revision: 1}}, Credentials: []s.Credential{{ID: "owner-key", UserID: "owner", Active: true, Revision: 1}, {ID: "writer-key", UserID: "writer", Active: true, Revision: 1}, {ID: "admin-key", UserID: "admin", Active: true, SystemAdmin: true, Revision: 1}}, Grant: s.Grant{BucketID: "bucket-1", UserID: "writer", Write: true, Revision: 1}, Quota: s.LogicalQuota{OwnerID: "owner", Limit: 8}, Packs: []s.PackInfo{{Version: s.ModelVersion, Ref: s.PackRef{ID: "pack-a", Generation: "gen-1"}, Size: 64, Class: s.Standard, State: "Warm"}, {Version: s.ModelVersion, Ref: s.PackRef{ID: "pack-b", Generation: "gen-2"}, Size: 64, Class: s.Standard, State: "Warm"}}, Lease: s.WriterLease{Fence: s.WriterFence{LeaseID: "writer-lease", ServiceIncarnation: "history-1", Epoch: 1}, ExpiresAt: now.Add(time.Minute)}}
}
func (f Fixture) Chunk(domain string) s.Chunk {
	return s.Chunk{Version: s.ModelVersion, Key: s.ChunkKey{Domain: domain, Hash: strings.Repeat("a", 64)}, RawBytes: 4, Location: s.ChunkLocation{Pack: f.Packs[0].Ref, Range: s.ByteRange{Offset: 0, Length: 4}, Codec: "raw"}, LocationRevision: 1}
}
func (f Fixture) Request(op string, revision uint64) s.CommitRequest {
	object := s.ObjectKey{Bucket: f.Bucket.Name, BucketID: f.Bucket.ID, Key: "file"}
	opID := fixtureOpID(op)
	return s.CommitRequest{ReservationRevision: 1, Head: s.ObjectHead{Version: s.ModelVersion, Object: object, Domain: f.Bucket.Domain, ManifestID: "manifest-" + op, Size: 4, ETag: "\"fixture-etag\"", ETagKind: "put-md5", Metadata: map[string]string{"content-type": "application/octet-stream"}, OpID: opID}, ExpectedRevision: revision, UserID: "writer", CredentialID: "writer-key", ReservationID: "reservation-" + op, Writer: f.Lease.Fence, Event: s.OutboxEvent{Version: s.ModelVersion, ID: fixtureEventID(opID, "Objects", "object_commit"), HistoryID: fixtureHistoryID, Epoch: 0, Entity: "Objects", Action: "object_commit", Object: object, Revision: revision + 1, OpID: opID, PayloadPointer: "journal/input/" + op, PayloadSHA256: strings.Repeat("b", 64)}}
}
func Prepare(t *testing.T, store s.MetadataStore, f Fixture, r s.CommitRequest, reserved int64) {
	t.Helper()
	ctx := context.Background()
	_, _, err := store.PutChunkIfAbsent(ctx, f.Chunk(f.Bucket.Domain), f.Lease.Fence)
	if err != nil {
		t.Fatal(err)
	}
	page := s.ManifestPage{Version: s.ModelVersion, ManifestID: r.Head.ManifestID, Domain: r.Head.Domain, Number: 0, Chunks: []s.ChunkRef{{Key: f.Chunk(f.Bucket.Domain).Key, RawBytes: 4, LogicalOffset: 0}}}
	if err := store.WriteManifestPages(ctx, []s.ManifestPage{page}, r.Writer); err != nil {
		t.Fatal(err)
	}
	base, err := store.GetObject(ctx, r.Head.Object)
	if err != nil && !errors.Is(err, s.ErrNotFound) {
		t.Fatal(err)
	}
	reserve := s.Reservation{Version: s.ModelVersion, OpID: r.Head.OpID, BaseRevision: base.Revision, BaseSize: base.Size, State: s.ReservationActive, Revision: 1, Deadline: f.Now.Add(30 * time.Minute), ID: r.ReservationID, OwnerID: f.Bucket.OwnerID, Object: r.Head.Object, Bytes: reserved, StagedBytes: r.Head.Size, Writer: r.Writer}
	if r.Receipt != nil {
		reserve.UploadID = r.Receipt.UploadID
	}
	if err := store.ReserveLogicalQuota(ctx, reserve); err != nil {
		t.Fatal(err)
	}
}
func requireError(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error=%v, want %v", err, want)
	}
}
func NoPublication(t *testing.T, store s.MetadataStore, r s.CommitRequest) {
	t.Helper()
	ctx := context.Background()
	_, err := store.GetObject(ctx, r.Head.Object)
	requireError(t, err, s.ErrNotFound)
	_, err = store.GetOutbox(ctx, r.Event.ID)
	requireError(t, err, s.ErrNotFound)
	if r.Receipt != nil {
		_, err = store.GetCompletionReceipt(ctx, r.Receipt.UploadID)
		requireError(t, err, s.ErrNotFound)
	}
}

func RunMetadata(t *testing.T, newStore MetadataFactory) {
	runPackProtection(t, newStore)
	runPartPages(t, newStore)
	runCanonicalDestinations(t, newStore)
	runReservations(t, newStore)
	runQueries(t, newStore)
	runBucketCreate(t, newStore)
	ctx := context.Background()
	t.Run("location CRC survives insert lookup and CAS", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		chunk := f.Chunk("default")
		chunk.Location.CRC32C = 0xe3069283
		inserted, won, err := h.Store.PutChunkIfAbsent(ctx, chunk, f.Lease.Fence)
		if err != nil || !won || inserted.Location.CRC32C != chunk.Location.CRC32C {
			t.Fatalf("insert lost stored-byte CRC: won=%v chunk=%+v err=%v", won, inserted, err)
		}
		check := func(want s.ChunkLocation, revision uint64) {
			t.Helper()
			got, err := h.Store.GetChunks(ctx, []s.ChunkKey{chunk.Key}, true)
			if err != nil || got[chunk.Key].Location != want || got[chunk.Key].LocationRevision != revision {
				t.Fatalf("location round-trip: got=%+v err=%v, want=%+v revision=%d", got[chunk.Key], err, want, revision)
			}
		}
		check(chunk.Location, 1)
		next := chunk.Location
		next.Pack = f.Packs[1].Ref
		next.CRC32C = 0 // Zero is a valid CRC, not a missing field.
		updated, err := h.Store.CompareAndSwapChunkLocation(ctx, chunk.Key, 1, next, f.Lease.Fence)
		if err != nil || updated.Location != next || updated.LocationRevision != 2 {
			t.Fatalf("CAS lost zero CRC: chunk=%+v err=%v", updated, err)
		}
		check(next, 2)
		next.CRC32C = 0x12345678
		updated, err = h.Store.CompareAndSwapChunkLocation(ctx, chunk.Key, 2, next, f.Lease.Fence)
		if err != nil || updated.Location != next || updated.LocationRevision != 3 {
			t.Fatalf("CAS lost replacement CRC: chunk=%+v err=%v", updated, err)
		}
		check(next, 3)
	})
	t.Run("domain isolation and canonical insert race", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		var winners atomic.Int32
		results := make(chan s.Chunk, 24)
		var wg sync.WaitGroup
		for i := 0; i < 24; i++ {
			wg.Go(func() {
				c := f.Chunk("default")
				if i%2 == 1 {
					c.Location.Pack = f.Packs[1].Ref
				}
				v, won, err := h.Store.PutChunkIfAbsent(ctx, c, f.Lease.Fence)
				if err != nil {
					t.Error(err)
					return
				}
				if won {
					winners.Add(1)
				}
				results <- v
			})
		}
		wg.Wait()
		close(results)
		if winners.Load() != 1 {
			t.Fatalf("canonical insert winners=%d", winners.Load())
		}
		var winner *s.Chunk
		for v := range results {
			if winner == nil {
				winner = &v
			} else if v != *winner {
				t.Fatal("loser did not receive canonical winner")
			}
		}
		other := f.Chunk("isolated")
		other.Location.Pack = f.Packs[1].Ref
		if _, won, err := h.Store.PutChunkIfAbsent(ctx, other, f.Lease.Fence); err != nil || !won {
			t.Fatalf("separate domain collapsed: won=%v err=%v", won, err)
		}
		got, err := h.Store.GetChunks(ctx, []s.ChunkKey{f.Chunk("default").Key, other.Key}, true)
		if err != nil || len(got) != 2 {
			t.Fatalf("lookup: %v %v", got, err)
		}
	})
	t.Run("location CAS has one winner", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		c := f.Chunk("default")
		if _, _, err := h.Store.PutChunkIfAbsent(ctx, c, f.Lease.Fence); err != nil {
			t.Fatal(err)
		}
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Go(func() {
				next := c.Location
				next.Pack = f.Packs[1].Ref
				next.Range.Offset = int64(i)
				v, err := h.Store.CompareAndSwapChunkLocation(ctx, c.Key, 1, next, f.Lease.Fence)
				if err == nil {
					wins.Add(1)
					if v.LocationRevision != 2 {
						t.Error("revision not advanced")
					}
				} else if !errors.Is(err, s.ErrConflict) {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("CAS winners=%d", wins.Load())
		}
	})
	t.Run("manifest pages immutable and owned copies", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		r := f.Request("one", 0)
		Prepare(t, h.Store, f, r, 4)
		pages, err := h.Store.GetManifest(ctx, r.Head.ManifestID)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Store.WriteManifestPages(ctx, pages, f.Lease.Fence); err != nil {
			t.Fatal("identical page retry", err)
		}
		pages[0].Chunks[0].RawBytes = 3
		extra := pages[0]
		extra.ManifestID = "must-not-publish"
		requireError(t, h.Store.WriteManifestPages(ctx, []s.ManifestPage{extra, pages[0]}, f.Lease.Fence), s.ErrConflict)
		saved, err := h.Store.GetManifest(ctx, r.Head.ManifestID)
		if err != nil || saved[0].Chunks[0].RawBytes != 4 {
			t.Fatal("page mutation through returned slice")
		}
	})
	t.Run("empty manifest publishes zero pages", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		r := f.Request("empty-object", 0)
		r.Head.Size = 0
		reservation := s.Reservation{Version: s.ModelVersion, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: f.Bucket.OwnerID,
			Object: r.Head.Object, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(30 * time.Minute), Writer: r.Writer}
		if err := h.Store.ReserveLogicalQuota(ctx, reservation); err != nil {
			t.Fatal(err)
		}
		NoPublication(t, h.Store, r)
		result, err := h.Store.CommitObject(ctx, r)
		if err != nil || result.Head.Size != 0 || result.Head.PageCount != 0 || result.Head.ChunkCount != 0 {
			t.Fatal(result, err)
		}
		pages, err := h.Store.GetManifest(ctx, r.Head.ManifestID)
		if err != nil || len(pages) != 0 {
			t.Fatal("empty committed manifest", pages, err)
		}
		quota, _ := h.Store.GetQuota(ctx, f.Bucket.OwnerID)
		if quota.Used != 0 || quota.Reserved != 0 || quota.Staged != 0 {
			t.Fatal(quota)
		}
	})
	t.Run("multi-page manifest publishes only after every page", func(t *testing.T) {
		f := NewFixture()
		r := f.Request("large-manifest", 0)
		capacity, err := manifest.RefCapacity(r.Head.ManifestID, r.Head.Domain)
		if err != nil {
			t.Fatal(err)
		}
		r.Head.Size = int64(capacity+1) * 4
		f.Quota.Limit = r.Head.Size
		h := newStore(t, f)
		chunk := f.Chunk(r.Head.Domain)
		if _, _, err := h.Store.PutChunkIfAbsent(ctx, chunk, r.Writer); err != nil {
			t.Fatal(err)
		}
		first := s.ManifestPage{Version: s.ModelVersion, ManifestID: r.Head.ManifestID, Domain: r.Head.Domain, Number: 0}
		for i := 0; i < capacity; i++ {
			first.Chunks = append(first.Chunks, s.ChunkRef{Key: chunk.Key, RawBytes: 4, LogicalOffset: int64(i) * 4})
		}
		last := s.ManifestPage{Version: s.ModelVersion, ManifestID: r.Head.ManifestID, Domain: r.Head.Domain, Number: 1,
			Chunks: []s.ChunkRef{{Key: chunk.Key, RawBytes: 4, LogicalOffset: int64(capacity) * 4}}}
		if err := h.Store.WriteManifestPages(ctx, []s.ManifestPage{first}, r.Writer); err != nil {
			t.Fatal(err)
		}
		reservation := s.Reservation{Version: s.ModelVersion, ID: r.ReservationID, OpID: r.Head.OpID, OwnerID: f.Bucket.OwnerID,
			Object: r.Head.Object, Revision: 1, State: s.ReservationActive, Deadline: f.Now.Add(30 * time.Minute),
			Bytes: r.Head.Size, StagedBytes: r.Head.Size, Writer: r.Writer}
		if err := h.Store.ReserveLogicalQuota(ctx, reservation); err != nil {
			t.Fatal(err)
		}
		_, err = h.Store.CommitObject(ctx, r)
		requireError(t, err, s.ErrInvalid)
		NoPublication(t, h.Store, r)
		if err := h.Store.WriteManifestPages(ctx, []s.ManifestPage{last}, r.Writer); err != nil {
			t.Fatal(err)
		}
		result, err := h.Store.CommitObject(ctx, r)
		if err != nil || result.Head.PageCount != 2 || result.Head.ChunkCount != uint64(capacity+1) ||
			!reflect.DeepEqual(result.Head.PageIndex, []uint64{0, uint64(capacity * 4)}) {
			t.Fatal(result, err)
		}
		_, err = h.Store.CommitObject(ctx, r)
		if err != nil {
			t.Fatal("large manifest retry", err)
		}
		quota, _ := h.Store.GetQuota(ctx, f.Bucket.OwnerID)
		if quota.Used != r.Head.Size || quota.Reserved != 0 || quota.Staged != 0 {
			t.Fatal(quota)
		}
	})
	t.Run("atomic commit retry and immutable op input", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		r := f.Request("one", 0)
		Prepare(t, h.Store, f, r, 4)
		first, err := h.Store.CommitObject(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if first.Head.Revision != 1 || first.AlreadyCommitted {
			t.Fatal(first)
		}
		q, err := h.Store.GetQuota(ctx, "owner")
		if err != nil || q.Used != 4 || q.Reserved != 0 || q.Staged != 0 {
			t.Fatalf("quota=%+v error=%v", q, err)
		}
		event, err := h.Store.GetOutbox(ctx, r.Event.ID)
		if err != nil || event != r.Event {
			t.Fatal("head/outbox diverged", err)
		}
		renewed := f.Lease
		renewed.Fence.Epoch++
		h.SetWriter(renewed)
		r.Writer = renewed.Fence
		again, err := h.Store.CommitObject(ctx, r)
		if err != nil || !again.AlreadyCommitted || !reflect.DeepEqual(first.Head, again.Head) {
			t.Fatal("idempotent retry", again, err)
		}
		after, _ := h.Store.GetQuota(ctx, "owner")
		if after != q {
			t.Fatal("quota consumed twice")
		}
		first.Head.Metadata["content-type"] = "mutated"
		saved, _ := h.Store.GetObject(ctx, r.Head.Object)
		if saved.Metadata["content-type"] != "application/octet-stream" {
			t.Fatal("returned head aliases stored data")
		}
		r.Head.ETag = "different"
		_, err = h.Store.CommitObject(ctx, r)
		requireError(t, err, s.ErrConflict)
	})
	t.Run("head CAS race publishes one complete result", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		a, b := f.Request("a", 0), f.Request("b", 0)
		Prepare(t, h.Store, f, a, 4)
		Prepare(t, h.Store, f, b, 4)
		var wg sync.WaitGroup
		var won atomic.Int32
		for _, r := range []s.CommitRequest{a, b} {
			wg.Go(func() {
				_, err := h.Store.CommitObject(ctx, r)
				if err == nil {
					won.Add(1)
				} else if !errors.Is(err, s.ErrConflict) {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if won.Load() != 1 {
			t.Fatalf("head winners=%d", won.Load())
		}
		head, _ := h.Store.GetObject(ctx, a.Head.Object)
		eventID := fixtureEventID(head.OpID, "Objects", "object_commit")
		if _, err := h.Store.GetOutbox(ctx, eventID); err != nil {
			t.Fatal(err)
		}
		q, _ := h.Store.GetQuota(ctx, "owner")
		if q.Used != 4 || q.Reserved != 4 || q.Staged != 4 {
			t.Fatalf("loser reservation unexpectedly consumed: %+v", q)
		}
	})
	t.Run("owner and grantee rights without implicit admin data access", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		owner, err := h.Store.Authorize(ctx, "owner-key", f.Bucket.Name)
		if err != nil || !owner.Read || !owner.Write {
			t.Fatal(owner, err)
		}
		writer, err := h.Store.Authorize(ctx, "writer-key", f.Bucket.Name)
		if err != nil || writer.Read || !writer.Write || writer.Bucket.Domain != f.Bucket.Domain {
			t.Fatal(writer, err)
		}
		admin, err := h.Store.Authorize(ctx, "admin-key", f.Bucket.Name)
		if err != nil || !admin.SystemAdmin || admin.Read || admin.Write {
			t.Fatal("admin inherited data access", admin, err)
		}
	})
	// The S3 router relies on this order (API v2 2.2.3 steps 1-3): a revoked
	// credential or a disabled user is denied before the bucket is looked up,
	// so neither learns whether a bucket exists.
	t.Run("Authorize denies credential and user before the bucket lookup", func(t *testing.T) {
		f := NewFixture()
		f.Users[1].Active = false       // writer
		f.Credentials[2].Active = false // admin-key
		h := newStore(t, f)
		for _, key := range []string{"writer-key", "admin-key", "unknown-key"} {
			for _, bucket := range []string{f.Bucket.Name, "missing", ""} {
				_, err := h.Store.Authorize(ctx, key, bucket)
				requireError(t, err, s.ErrAccessDenied)
			}
		}
		_, err := h.Store.Authorize(ctx, "owner-key", "missing")
		requireError(t, err, s.ErrNotFound)
		_, err = h.Store.Authorize(ctx, "owner-key", "")
		requireError(t, err, s.ErrNotFound)
	})
	for _, change := range []string{"credential revoke", "grant revoke", "bucket removed", "bucket inactive", "bucket ABA", "expired writer", "stale epoch", "service incarnation", "quota shrunk"} {
		t.Run("commit rechecks "+change, func(t *testing.T) {
			f := NewFixture()
			h := newStore(t, f)
			r := f.Request("one", 0)
			Prepare(t, h.Store, f, r, 4)
			want := s.ErrAccessDenied
			if _, err := h.Store.Authorize(ctx, r.CredentialID, f.Bucket.Name); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "credential revoke":
				c := f.Credentials[1]
				c.Active = false
				h.SetCredential(c)
			case "grant revoke":
				g := f.Grant
				g.Write = false
				g.Read = true
				g.Revision++
				h.SetGrant(g)
			case "bucket removed":
				h.RemoveBucket(f.Bucket.Name)
				want = s.ErrBucketNotFound
			case "bucket inactive":
				b := f.Bucket
				b.Active = false
				h.SetBucket(b)
				want = s.ErrBucketNotFound
			case "bucket ABA":
				b := f.Bucket
				b.ID = "bucket-2"
				b.Revision = 1
				h.SetBucket(b)
				want = s.ErrBucketNotFound
			case "expired writer":
				lease := f.Lease
				lease.ExpiresAt = f.Now
				h.SetWriter(lease)
				want = s.ErrFenced
			case "stale epoch":
				lease := f.Lease
				lease.Fence.Epoch++
				h.SetWriter(lease)
				want = s.ErrFenced
			case "service incarnation":
				lease := f.Lease
				lease.Fence.ServiceIncarnation = "history-2"
				h.SetWriter(lease)
				want = s.ErrFenced
			case "quota shrunk":
				q, _ := h.Store.GetQuota(ctx, "owner")
				q.Limit = 3
				h.SetQuota(q)
				want = s.ErrQuotaExceeded
			}
			before, _ := h.Store.GetQuota(ctx, "owner")
			_, err := h.Store.CommitObject(ctx, r)
			requireError(t, err, want)
			if change == "bucket removed" || change == "bucket inactive" || change == "bucket ABA" {
				requireError(t, err, s.ErrNotFound) // Preserve the established storage contract.
			}
			NoPublication(t, h.Store, r)
			after, _ := h.Store.GetQuota(ctx, "owner")
			if after != before {
				t.Fatal("failed commit mutated quota")
			}
		})
	}
	t.Run("missing reservation is not a missing bucket", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		r := f.Request("missing-reservation", 0)
		_, err := h.Store.CommitObject(ctx, r)
		requireError(t, err, s.ErrNotFound)
		if errors.Is(err, s.ErrBucketNotFound) {
			t.Fatal("internal absence misclassified as bucket", err)
		}
		NoPublication(t, h.Store, r)
	})
	t.Run("full quota overwrite credit and tombstone CAS", func(t *testing.T) {
		f := NewFixture()
		f.Quota.Limit = 4
		h := newStore(t, f)
		a := f.Request("first", 0)
		Prepare(t, h.Store, f, a, 4)
		if _, err := h.Store.CommitObject(ctx, a); err != nil {
			t.Fatal(err)
		}
		b := f.Request("overwrite", 1)
		Prepare(t, h.Store, f, b, 0)
		if _, err := h.Store.CommitObject(ctx, b); err != nil {
			t.Fatal("same-size overwrite at full quota", err)
		}
		del := f.Request("delete", 2)
		del.Head.Deleted = true
		del.Head.ManifestID = ""
		del.Head.Size = 0
		del.Head.ETag, del.Head.ETagKind, del.Head.Metadata = "", "", nil
		if err := h.Store.ReserveLogicalQuota(ctx, s.Reservation{Version: s.ModelVersion, OpID: del.Head.OpID, BaseRevision: 2, BaseSize: 4, State: s.ReservationActive, Revision: 1, Deadline: f.Now.Add(30 * time.Minute), ID: del.ReservationID, OwnerID: "owner", Object: del.Head.Object, Writer: del.Writer}); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Store.DeleteObject(ctx, del); err != nil {
			t.Fatal(err)
		}
		head, _ := h.Store.GetObject(ctx, del.Head.Object)
		if !head.Deleted || head.Revision != 3 {
			t.Fatal("delete lost tombstone revision")
		}
		q, _ := h.Store.GetQuota(ctx, "owner")
		if q.Used != 0 || q.Reserved != 0 || q.Staged != 0 {
			t.Fatal(q)
		}
		stale := f.Request("stale-create", 0)
		Prepare(t, h.Store, f, stale, 4)
		_, err := h.Store.CommitObject(ctx, stale)
		requireError(t, err, s.ErrConflict)
	})
	t.Run("reservation release idempotent and bounded", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		r := s.Reservation{Version: s.ModelVersion, OpID: fixtureOpID("reserve-op"), State: s.ReservationActive, Revision: 1, Deadline: f.Now.Add(30 * time.Minute), ID: "r", OwnerID: "owner", Object: f.Request("one", 0).Head.Object, Bytes: 4, StagedBytes: 4, Writer: f.Lease.Fence}
		if err := h.Store.ReserveLogicalQuota(ctx, r); err != nil {
			t.Fatal(err)
		}
		if err := h.Store.ReserveLogicalQuota(ctx, r); err != nil {
			t.Fatal(err)
		}
		q, _ := h.Store.GetQuota(ctx, "owner")
		if q.Reserved != 4 || q.Staged != 4 {
			t.Fatal("reservation counted twice", q)
		}
		extra := r
		extra.ID = "extra"
		extra.OpID = fixtureOpID("extra-op")
		extra.Bytes = 5
		requireError(t, h.Store.ReserveLogicalQuota(ctx, extra), s.ErrQuotaExceeded)
		for i := 0; i < 2; i++ {
			if err := h.Store.ReleaseLogicalQuota(ctx, s.ReservationRelease{ID: r.ID, OpID: r.OpID, ExpectedRevision: r.Revision, Writer: r.Writer}); err != nil {
				t.Fatal(err)
			}
		}
		q, _ = h.Store.GetQuota(ctx, "owner")
		if q.Reserved != 0 || q.Staged != 0 {
			t.Fatal(q)
		}
	})
	t.Run("completion all or nothing and lost ack retry", func(t *testing.T) {
		f := NewFixture()
		f.Quota.ActiveUploads = 1
		f.Upload = &s.Completion{UploadID: "upload-1", BucketID: f.Bucket.ID, Key: "file", Initiator: "writer", Revision: 3, Fence: 2, State: "Completing"}
		h := newStore(t, f)
		r := f.Request("complete", 0)
		r.Completion = f.Upload
		r.Receipt = &s.CompletedReceipt{Version: s.ModelVersion, UploadID: f.Upload.UploadID, BucketID: f.Bucket.ID, Key: "file", Initiator: "writer", OpID: r.Head.OpID, InputDigest: strings.Repeat("c", 64), ManifestID: r.Head.ManifestID, ETag: r.Head.ETag, ExpiresAt: f.Now.Add(7 * 24 * time.Hour)}
		Prepare(t, h.Store, f, r, 4)
		q, _ := h.Store.GetQuota(ctx, "owner")
		h.FailCommit(true, false)
		_, err := h.Store.CommitObject(ctx, r)
		requireError(t, err, s.ErrUnavailable)
		NoPublication(t, h.Store, r)
		after, _ := h.Store.GetQuota(ctx, "owner")
		if after != q {
			t.Fatal("partial failure consumed quota/active upload")
		}
		h.FailCommit(false, true)
		_, err = h.Store.CommitObject(ctx, r)
		requireError(t, err, s.ErrUnavailable)
		receipt, err := h.Store.GetCompletionReceipt(ctx, r.Receipt.UploadID)
		if err != nil || receipt != *r.Receipt {
			t.Fatal("missing receipt after atomic commit", err)
		}
		if _, err := h.Store.GetOutbox(ctx, r.Event.ID); err != nil {
			t.Fatal(err)
		}
		head, err := h.Store.GetObject(ctx, r.Head.Object)
		if err != nil || head.Revision != 1 {
			t.Fatal(head, err)
		}
		after, _ = h.Store.GetQuota(ctx, "owner")
		if after.Used != 4 || after.Reserved != 0 || after.Staged != 0 || after.ActiveUploads != 0 {
			t.Fatal("completion counters not atomic", after)
		}
		h.FailCommit(false, false)
		result, err := h.Store.CommitObject(ctx, r)
		if err != nil || !result.AlreadyCommitted {
			t.Fatal(result, err)
		}
		again, _ := h.Store.GetQuota(ctx, "owner")
		if again != after {
			t.Fatal("completion retry consumed twice")
		}
	})
	t.Run("stale completion fence cannot publish", func(t *testing.T) {
		f := NewFixture()
		f.Quota.ActiveUploads = 1
		f.Upload = &s.Completion{UploadID: "upload-1", BucketID: f.Bucket.ID, Key: "file", Initiator: "writer", Revision: 3, Fence: 2, State: "Completing"}
		h := newStore(t, f)
		r := f.Request("complete", 0)
		current := *f.Upload
		current.Fence++
		r.Completion = &current
		r.Receipt = &s.CompletedReceipt{Version: s.ModelVersion, UploadID: current.UploadID, BucketID: current.BucketID, Key: current.Key, Initiator: current.Initiator, OpID: r.Head.OpID, InputDigest: strings.Repeat("c", 64), ManifestID: r.Head.ManifestID, ETag: r.Head.ETag, ExpiresAt: f.Now.Add(7 * 24 * time.Hour)}
		Prepare(t, h.Store, f, r, 4)
		_, err := h.Store.CommitObject(ctx, r)
		requireError(t, err, s.ErrConflict)
		NoPublication(t, h.Store, r)
	})
	t.Run("cancelled mutation does not publish", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		r := f.Request("one", 0)
		Prepare(t, h.Store, f, r, 4)
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := h.Store.CommitObject(cancelled, r)
		requireError(t, err, context.Canceled)
		NoPublication(t, h.Store, r)
	})
}
