// Package contracttest is the reusable backend acceptance suite. Factories must
// return isolated stores and control hooks for authoritative fixture changes.
package contracttest

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	Store         s.MetadataStore
	SetBucket     func(s.Bucket)
	SetCredential func(s.Credential)
	SetGrant      func(s.Grant)
	SetWriter     func(s.WriterLease)
	SetQuota      func(s.LogicalQuota)
	FailCommit    func(before, after bool)
}
type MetadataFactory func(*testing.T, Fixture) MetadataHarness

func NewFixture() Fixture {
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	return Fixture{Now: now, Bucket: s.Bucket{Name: "demo", ID: "bucket-1", OwnerID: "owner", Domain: "default", Revision: 1, Active: true}, Users: []s.User{{ID: "owner", Active: true, Revision: 1}, {ID: "writer", Active: true, Revision: 1}, {ID: "admin", Active: true, Revision: 1}}, Credentials: []s.Credential{{ID: "owner-key", UserID: "owner", Active: true, Revision: 1}, {ID: "writer-key", UserID: "writer", Active: true, Revision: 1}, {ID: "admin-key", UserID: "admin", Active: true, SystemAdmin: true, Revision: 1}}, Grant: s.Grant{BucketID: "bucket-1", UserID: "writer", Write: true, Revision: 1}, Quota: s.LogicalQuota{OwnerID: "owner", Limit: 8}, Packs: []s.PackInfo{{Version: s.ModelVersion, Ref: s.PackRef{ID: "pack-a", Generation: "gen-1"}, Size: 64, Class: s.Standard, State: "Warm"}, {Version: s.ModelVersion, Ref: s.PackRef{ID: "pack-b", Generation: "gen-2"}, Size: 64, Class: s.Standard, State: "Warm"}}, Lease: s.WriterLease{Fence: s.WriterFence{LeaseID: "writer-lease", ServiceIncarnation: "history-1", Epoch: 1}, ExpiresAt: now.Add(time.Minute)}}
}
func (f Fixture) Chunk(domain string) s.Chunk {
	return s.Chunk{Version: s.ModelVersion, Key: s.ChunkKey{Domain: domain, Hash: strings.Repeat("a", 64)}, RawBytes: 4, Location: s.ChunkLocation{Pack: f.Packs[0].Ref, Range: s.ByteRange{Offset: 0, Length: 4}, Codec: "raw"}, LocationRevision: 1}
}
func (f Fixture) Request(op string, revision uint64) s.CommitRequest {
	object := s.ObjectKey{Bucket: f.Bucket.Name, BucketID: f.Bucket.ID, Key: "file"}
	return s.CommitRequest{Head: s.ObjectHead{Version: s.ModelVersion, Object: object, Domain: f.Bucket.Domain, ManifestID: "manifest-" + op, Size: 4, ETag: "\"fixture-etag\"", ETagKind: "put-md5", Metadata: map[string]string{"content-type": "application/octet-stream"}, OpID: op}, ExpectedRevision: revision, UserID: "writer", CredentialID: "writer-key", ReservationID: "reservation-" + op, Writer: f.Lease.Fence, Event: s.OutboxEvent{Version: s.ModelVersion, ID: "event-" + op, HistoryID: "history-1", Epoch: 0, Entity: "Objects", Object: object, Revision: revision + 1, OpID: op, PayloadPointer: "journal/input/" + op, PayloadSHA256: strings.Repeat("b", 64)}}
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
	reserve := s.Reservation{ID: r.ReservationID, OwnerID: f.Bucket.OwnerID, Object: r.Head.Object, Bytes: reserved, StagedBytes: r.Head.Size, Writer: r.Writer}
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
	ctx := context.Background()
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
		eventID := "event-" + head.OpID
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
	for _, change := range []string{"credential revoke", "grant revoke", "bucket ABA", "expired writer", "stale epoch", "service incarnation", "quota shrunk"} {
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
			case "bucket ABA":
				b := f.Bucket
				b.ID = "bucket-2"
				b.Revision = 1
				h.SetBucket(b)
				want = s.ErrNotFound
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
			NoPublication(t, h.Store, r)
			after, _ := h.Store.GetQuota(ctx, "owner")
			if after != before {
				t.Fatal("failed commit mutated quota")
			}
		})
	}
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
		if err := h.Store.ReserveLogicalQuota(ctx, s.Reservation{ID: del.ReservationID, OwnerID: "owner", Object: del.Head.Object, Writer: del.Writer}); err != nil {
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
		r := s.Reservation{ID: "r", OwnerID: "owner", Object: f.Request("one", 0).Head.Object, Bytes: 4, StagedBytes: 4, Writer: f.Lease.Fence}
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
		extra.Bytes = 5
		requireError(t, h.Store.ReserveLogicalQuota(ctx, extra), s.ErrQuotaExceeded)
		for i := 0; i < 2; i++ {
			if err := h.Store.ReleaseLogicalQuota(ctx, r.ID, r.Writer); err != nil {
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
