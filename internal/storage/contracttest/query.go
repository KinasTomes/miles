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

func runQueries(t *testing.T, newStore MetadataFactory) {
	ctx := context.Background()
	t.Run("accessible buckets require current rights and incarnation", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		second := f.Bucket
		second.Name, second.ID = "second", "bucket-2"
		h.SetBucket(second)
		first, err := h.Store.ListAccessibleBuckets(ctx, s.BucketListRequest{PrincipalID: "owner", Limit: 1})
		if err != nil || len(first.Buckets) != 1 || first.Buckets[0] != f.Bucket || first.NextAfter != f.Bucket.Name {
			t.Fatal(first, err)
		}
		next, err := h.Store.ListAccessibleBuckets(ctx, s.BucketListRequest{PrincipalID: "owner", After: first.NextAfter, Limit: 1})
		if err != nil || len(next.Buckets) != 1 || next.Buckets[0] != second || next.NextAfter != "" {
			t.Fatal(next, err)
		}
		for _, principal := range []string{"writer", "admin"} {
			page, err := h.Store.ListAccessibleBuckets(ctx, s.BucketListRequest{PrincipalID: principal, Limit: 10})
			want := 1
			if principal == "admin" {
				want = 0
			}
			if err != nil || len(page.Buckets) != want {
				t.Fatal("implicit admin access or missing write-only bucket", page, err)
			}
		}
		replacement := f.Bucket
		replacement.ID = "new-incarnation"
		h.SetBucket(replacement)
		page, err := h.Store.ListAccessibleBuckets(ctx, s.BucketListRequest{PrincipalID: "writer", Limit: 10})
		if err != nil || len(page.Buckets) != 0 {
			t.Fatal("grant survived recreation", page, err)
		}
		_, err = h.Store.ListAccessibleBuckets(ctx, s.BucketListRequest{PrincipalID: "unknown", Limit: 10})
		requireError(t, err, s.ErrAccessDenied)
	})
	t.Run("consistent LIST counts logical entries and skips tombstones", func(t *testing.T) {
		f := NewFixture()
		f.Quota.Limit = 64
		h := newStore(t, f)
		for _, key := range []string{"a", "b", "dir/a", "dir/b", "z"} {
			// Object keys may contain '/', but frozen-format manifest IDs may not.
			r := f.Request("list-"+strings.ReplaceAll(key, "/", "-"), 0)
			r.Head.Object.Key = key
			r.Event.Object = r.Head.Object
			Prepare(t, h.Store, f, r, 4)
			if _, err := h.Store.CommitObject(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		del := f.Request("delete-b", 1)
		del.Head.Object.Key = "b"
		del.Event.Object = del.Head.Object
		del.Head.Deleted, del.Head.Size, del.Head.ManifestID = true, 0, ""
		reservation := s.Reservation{Version: s.ModelVersion, ID: del.ReservationID, OpID: del.Head.OpID, OwnerID: "owner", Object: del.Head.Object, BaseRevision: 1, BaseSize: 4, State: s.ReservationActive, Revision: 1, Deadline: f.Now.Add(time.Minute), Writer: f.Lease.Fence}
		if err := h.Store.ReserveLogicalQuota(ctx, reservation); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Store.DeleteObject(ctx, del); err != nil {
			t.Fatal(err)
		}
		req := s.ListObjectsRequest{PrincipalID: "owner", Bucket: f.Bucket.Name, BucketID: f.Bucket.ID, Delimiter: "/", MaxKeys: 1}
		entries := []string{}
		for i := 0; i < 5; i++ {
			page, err := h.Store.ListObjects(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			for _, o := range page.Objects {
				entries = append(entries, o.Object.Key)
				if o.Deleted {
					t.Fatal("tombstone leaked")
				}
			}
			entries = append(entries, page.CommonPrefixes...)
			if page.Next == nil {
				break
			}
			if page.Next.LastScannedKey == "" {
				t.Fatal("missing scan state")
			}
			req.Cursor = page.Next
			req.StartAfter = "zzzz" // continuation takes precedence
		}
		if !reflect.DeepEqual(entries, []string{"a", "dir/", "z"}) {
			t.Fatal("bad logical pagination", entries)
		}
		req.Cursor = nil
		req.StartAfter = "dir/a"
		req.MaxKeys = 10
		page, err := h.Store.ListObjects(ctx, req)
		if err != nil || len(page.CommonPrefixes) != 0 || len(page.Objects) != 1 || page.Objects[0].Object.Key != "z" {
			t.Fatal("start-after/delimiter", page, err)
		}
		req.StartAfter = ""
		req.Delimiter = ""
		req.Prefix = "dir/"
		req.MaxKeys = 1
		page, err = h.Store.ListObjects(ctx, req)
		if err != nil || page.Next == nil || page.Objects[0].Object.Key != "dir/a" {
			t.Fatal(page, err)
		}
		bound := req
		bound.Cursor = page.Next
		bound.Prefix = ""
		_, err = h.Store.ListObjects(ctx, bound)
		requireError(t, err, s.ErrInvalid)
		req.PrincipalID = "writer"
		_, err = h.Store.ListObjects(ctx, req)
		requireError(t, err, s.ErrAccessDenied)
		grant := f.Grant
		grant.Read = true
		grant.Write = false
		grant.Revision++
		h.SetGrant(grant)
		page, err = h.Store.ListObjects(ctx, req)
		if err != nil || len(page.Objects) != 1 {
			t.Fatal("read-only LIST", page, err)
		}
		page.Objects[0].Metadata["content-type"] = "changed"
		again, err := h.Store.ListObjects(ctx, req)
		if err != nil || again.Objects[0].Metadata["content-type"] == "changed" {
			t.Fatal("LIST aliases stored head", err)
		}
		grant.Read = false
		grant.Revision++
		h.SetGrant(grant)
		req.Cursor = page.Next
		_, err = h.Store.ListObjects(ctx, req)
		requireError(t, err, s.ErrAccessDenied)
		req.Cursor = nil
		req.PrincipalID = "owner"
		req.MaxKeys = 0
		page, err = h.Store.ListObjects(ctx, req)
		if err != nil || len(page.Objects) != 0 || page.Next != nil {
			t.Fatal("max-keys zero", page, err)
		}
		req.MaxKeys = -1
		_, err = h.Store.ListObjects(ctx, req)
		requireError(t, err, s.ErrInvalid)
	})
	t.Run("pack state pagination and monotone access observations", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		req := s.PackListRequest{State: "Warm", Limit: 1}
		first, err := h.Store.ListPacksByState(ctx, req)
		if err != nil || len(first.Packs) != 1 || first.Packs[0].Ref != f.Packs[0].Ref || first.NextAfter == nil {
			t.Fatal(first, err)
		}
		req.After = first.NextAfter
		second, err := h.Store.ListPacksByState(ctx, req)
		if err != nil || len(second.Packs) != 1 || second.Packs[0].Ref != f.Packs[1].Ref || second.NextAfter != nil {
			t.Fatal(second, err)
		}
		newest := f.Now.Add(time.Hour)
		updates := []s.PackAccess{{Pack: f.Packs[0].Ref, LastAccess: newest}, {Pack: f.Packs[0].Ref, LastAccess: f.Now}}
		if err := h.Store.UpdateAccessStats(ctx, updates); err != nil {
			t.Fatal(err)
		}
		got, err := h.Store.GetPacks(ctx, []s.PackRef{f.Packs[0].Ref})
		if err != nil || !got[f.Packs[0].Ref].LastAccess.Equal(newest) || got[f.Packs[0].Ref].StateRevision != f.Packs[0].StateRevision {
			t.Fatal("access changed state or moved backwards", got, err)
		}
		bad := f.Packs[0].Ref
		bad.Generation = "missing"
		requireError(t, h.Store.UpdateAccessStats(ctx, []s.PackAccess{{Pack: bad, LastAccess: newest}}), s.ErrNotFound)
		req.State = "unknown"
		_, err = h.Store.ListPacksByState(ctx, req)
		requireError(t, err, s.ErrInvalid)
	})
	runPolicy(t, newStore)
}

const fixturePolicy = `{"tiering":{"enabled":true,"warm_storage_class":"STANDARD","cold_storage_class":"GLACIER_IR","cold_after_days":30,"min_pack_live_ratio":0.8,"auto_restore":true,"auto_restore_days":1,"auto_restore_tier":"Standard"},"compression":{"enabled":true,"codec":"zstd","level":3,"min_saving_ratio":0.1},"chunking":{"min_bytes":262144,"avg_bytes":1048576,"max_bytes":4194304}}`

func policyRequest(f Fixture, op string, expected uint64, doc string) s.PolicyUpdate {
	return s.PolicyUpdate{Candidate: s.PolicySnapshot{Version: s.ModelVersion, Document: []byte(doc)}, ExpectedRevision: expected, CredentialID: "admin-key", Writer: f.Lease.Fence, Event: &s.OutboxEvent{Version: s.ModelVersion, ID: "policy-event-" + op, HistoryID: "history-1", Entity: "SystemConfig", EntityID: "global-policy", Revision: expected + 1, OpID: op, PayloadPointer: "policy/input/" + op, PayloadSHA256: strings.Repeat("b", 64)}}
}
func runPolicy(t *testing.T, newStore MetadataFactory) {
	ctx := context.Background()
	t.Run("policy CAS history no-op and authorization", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		_, err := h.Store.GetPolicy(ctx, 0)
		requireError(t, err, s.ErrNotFound)
		r := policyRequest(f, "bootstrap", 0, fixturePolicy)
		denied := r
		denied.CredentialID = "owner-key"
		denied.ExpectedRevision = 5
		_, err = h.Store.PutPolicy(ctx, denied)
		requireError(t, err, s.ErrAccessDenied)
		first, err := h.Store.PutPolicy(ctx, r)
		if err != nil || first.Revision != 1 {
			t.Fatal(first, err)
		}
		r.Candidate.Document[0] = 'X'
		stored, err := h.Store.GetPolicy(ctx, 1)
		if err != nil || string(stored.Document) != fixturePolicy {
			t.Fatal("input alias", stored, err)
		}
		stored.Document[0] = 'X'
		noop := policyRequest(f, "noop", 1, strings.Replace(fixturePolicy, "0.1", "0.10", 1))
		noop.Event = nil
		same, err := h.Store.PutPolicy(ctx, noop)
		if err != nil || !reflect.DeepEqual(first, same) {
			t.Fatal("decimal no-op", same, err)
		}
		_, err = h.Store.GetPolicy(ctx, 2)
		requireError(t, err, s.ErrNotFound)
		stale := noop
		stale.ExpectedRevision = 0
		_, err = h.Store.PutPolicy(ctx, stale)
		requireError(t, err, s.ErrConflict)
		// A float64 comparison would mistake this real change for a no-op.
		change := policyRequest(f, "precise", 1, strings.Replace(fixturePolicy, "0.1", "0.1000000000000000001", 1))
		updated, err := h.Store.PutPolicy(ctx, change)
		if err != nil || updated.Revision != 2 {
			t.Fatal("lost exact decimal change", updated, err)
		}
		pinned, err := h.Store.GetPolicy(ctx, 1)
		if err != nil || !reflect.DeepEqual(pinned, first) {
			t.Fatal("history overwritten", pinned, err)
		}
		_, err = h.Store.GetPolicy(ctx, 999)
		requireError(t, err, s.ErrNotFound)
		event, err := h.Store.GetOutbox(ctx, change.Event.ID)
		if err != nil || event != *change.Event {
			t.Fatal("missing policy event", event, err)
		}
		h.SetCredential(s.Credential{ID: "admin-key", UserID: "admin", Active: false, SystemAdmin: true, Revision: 2})
		_, err = h.Store.PutPolicy(ctx, policyRequest(f, "revoked", 2, fixturePolicy))
		requireError(t, err, s.ErrAccessDenied)
	})
	t.Run("policy rejects invalid envelope payload and stale fence", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		for _, doc := range []string{"null", "[]", "{} {}", "{broken"} {
			_, err := h.Store.PutPolicy(ctx, policyRequest(f, "invalid", 0, doc))
			requireError(t, err, s.ErrInvalid)
		}
		bad := policyRequest(f, "checksum", 0, fixturePolicy)
		bad.Event.PayloadSHA256 = strings.Repeat("z", 64)
		_, err := h.Store.PutPolicy(ctx, bad)
		requireError(t, err, s.ErrInvalid)
		bad = policyRequest(f, "wrong-entity", 0, fixturePolicy)
		bad.Event.EntityID = "another-config"
		_, err = h.Store.PutPolicy(ctx, bad)
		requireError(t, err, s.ErrInvalid)
		current := f.Lease
		current.Fence.Epoch++
		h.SetWriter(current)
		_, err = h.Store.PutPolicy(ctx, policyRequest(f, "fenced", 0, fixturePolicy))
		requireError(t, err, s.ErrFenced)
		_, err = h.Store.GetPolicy(ctx, 0)
		requireError(t, err, s.ErrNotFound)
	})
	t.Run("policy publication is atomic across faults", func(t *testing.T) {
		for _, after := range []bool{false, true} {
			t.Run(map[bool]string{false: "before", true: "after"}[after], func(t *testing.T) {
				f := NewFixture()
				h := newStore(t, f)
				r := policyRequest(f, "fault", 0, fixturePolicy)
				h.FailCommit(!after, after)
				_, err := h.Store.PutPolicy(ctx, r)
				requireError(t, err, s.ErrUnavailable)
				h.FailCommit(false, false)
				current, ce := h.Store.GetPolicy(ctx, 0)
				history, he := h.Store.GetPolicy(ctx, 1)
				_, ee := h.Store.GetOutbox(ctx, r.Event.ID)
				if after {
					if ce != nil || he != nil || ee != nil || current.Revision != 1 || !reflect.DeepEqual(current, history) {
						t.Fatal("partial policy publication", ce, he, ee)
					}
					_, err = h.Store.PutPolicy(ctx, r)
					requireError(t, err, s.ErrConflict)
				} else {
					requireError(t, ce, s.ErrNotFound)
					requireError(t, he, s.ErrNotFound)
					requireError(t, ee, s.ErrNotFound)
					if _, err := h.Store.PutPolicy(ctx, r); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	})
	t.Run("concurrent policy changes have one winner", func(t *testing.T) {
		f := NewFixture()
		h := newStore(t, f)
		if _, err := h.Store.PutPolicy(ctx, policyRequest(f, "bootstrap", 0, fixturePolicy)); err != nil {
			t.Fatal(err)
		}
		var winners atomic.Int32
		var wg sync.WaitGroup
		for _, class := range []string{"GLACIER", "GLACIER_IR"} {
			wg.Go(func() {
				doc := strings.Replace(fixturePolicy, "\"cold_after_days\":30", "\"cold_after_days\":31", 1)
				doc = strings.Replace(doc, "GLACIER_IR", class, 1)
				_, err := h.Store.PutPolicy(ctx, policyRequest(f, class, 1, doc))
				if err == nil {
					winners.Add(1)
				} else if !errors.Is(err, s.ErrConflict) {
					t.Error(err)
				}
			})
		}
		wg.Wait()
		if winners.Load() != 1 {
			t.Fatal("policy winners", winners.Load())
		}
	})
}
