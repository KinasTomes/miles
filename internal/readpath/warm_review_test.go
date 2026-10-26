package readpath

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/storage"
	db "github.com/dgraph-io/badger/v4"
)

// Test-only authoritative mutation. W=0 and the synchronous emitter guarantee
// all workers are quiescent at this boundary. Close the adapter, edit its real
// persisted record, reopen and rebind the fixture. This is not an online Admin
// API, concurrent Badger writers, fake authorization, or a runtime restart.
func mutateWarmRecord[T any](t *testing.T, f *warmFixture, table string, parts []string, change func(*T)) {
	t.Helper()
	if f.request.BucketPolicy.Cache().ReadaheadChunks != 0 {
		t.Fatal("mutation requires W=0")
	}
	if err := f.metadata.Close(); err != nil {
		t.Fatal(err)
	}
	f.metadata = nil
	raw, err := db.Open(db.DefaultOptions(f.metadataPath).WithSyncWrites(true).WithLogger(nil))
	if err != nil {
		t.Fatal(err)
	}
	key := table
	for _, p := range parts {
		key += "/" + hex.EncodeToString([]byte(p))
	}
	err = raw.Update(func(tx *db.Txn) error {
		item, err := tx.Get([]byte(key))
		if err != nil {
			return err
		}
		var record struct {
			Schema uint32
			Value  T
		}
		if err = item.Value(func(b []byte) error { return json.Unmarshal(b, &record) }); err != nil {
			return err
		}
		if record.Schema != 1 {
			return storage.ErrInvalid
		}
		change(&record.Value)
		b, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return tx.Set([]byte(key), b)
	})
	closeErr := raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	f.metadata, err = badger.Open(badger.Options{Path: f.metadataPath, Now: func() time.Time { return f.clock.Now().WallUTC }})
	if err != nil {
		t.Fatal(err)
	}
	f.reader.options.Metadata = f.metadata
}

func TestWarmReaderAdmittedReadSurvivesCredentialRevoke(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	var out []byte
	revoked := false
	_, err := f.reader.Read(ctx, f.request, func(_ context.Context, b []byte) error {
		out = append(out, b...)
		if !revoked {
			revoked = true
			mutateWarmRecord[storage.Credential](t, f, "credentials", []string{f.request.CredentialID}, func(c *storage.Credential) { c.Active = false; c.Revision++ })
			c, err := f.metadata.GetCredential(ctx, f.request.CredentialID)
			if err != nil || c.Active {
				t.Fatal("revocation premise", c, err)
			}
			if _, err = f.metadata.Authorize(ctx, f.request.CredentialID, f.request.Bucket); !errors.Is(err, storage.ErrAccessDenied) {
				t.Fatal("authoritative denial premise", err)
			}
		}
		return nil
	})
	if err != nil || !revoked || !bytes.Equal(out, f.body) {
		t.Fatalf("admitted stream cut: bytes=%d/%d err=%v", len(out), len(f.body), err)
	}
	before := f.reader.options.Metrics.Snapshot()
	calls := f.backend.count()
	_, next, err := f.read(ctx, f.request)
	if !errors.Is(err, storage.ErrAccessDenied) || len(next) != 0 || f.backend.count() != calls || f.reader.options.Metrics.Snapshot().CacheHits != before.CacheHits {
		t.Fatal("new admission bypassed revocation", err)
	}
	drained(t, f.reader.options.Admission)
}

func TestWarmReaderLastAccessUpdateDoesNotInvalidateBinding(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	var out []byte
	updated := false
	at := f.clock.Now().WallUTC.Add(time.Second)
	_, err := f.reader.Read(ctx, f.request, func(_ context.Context, b []byte) error {
		out = append(out, b...)
		if !updated {
			updated = true
			return f.metadata.UpdateAccessStats(ctx, []storage.PackAccess{{Pack: f.ref, LastAccess: at}})
		}
		return nil
	})
	if err != nil || !updated || !bytes.Equal(out, f.body) {
		t.Fatalf("stats-only update cut read: bytes=%d/%d err=%v", len(out), len(f.body), err)
	}
	infos, err := f.metadata.GetPacks(ctx, []storage.PackRef{f.ref})
	if err != nil || !infos[f.ref].LastAccess.Equal(at) {
		t.Fatal("observation lost/reduced", infos, err)
	}
	drained(t, f.reader.options.Admission)
}

func TestWarmReaderRecordsAccessOnMissAndHit(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx := context.Background()
	old := f.clock.Now().WallUTC.Add(-24 * time.Hour)
	if err := f.metadata.UpdateAccessStats(ctx, []storage.PackAccess{{Pack: f.ref, LastAccess: old}}); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if err := f.clock.Advance(time.Second); err != nil {
			t.Fatal(err)
		}
		now := f.clock.Now().WallUTC
		calls := f.backend.count()
		before := f.reader.options.Metrics.Snapshot()
		_, out, err := f.read(ctx, f.request)
		if err != nil || !bytes.Equal(out, f.body) {
			t.Fatal(err)
		}
		infos, err := f.metadata.GetPacks(ctx, []storage.PackRef{f.ref})
		if err != nil || !infos[f.ref].LastAccess.Equal(now) {
			t.Errorf("pass=%d logical access not recorded: got=%v want=%v err=%v", pass, infos[f.ref].LastAccess, now, err)
		}
		if pass == 1 && (f.backend.count() != calls || f.reader.options.Metrics.Snapshot().CacheHits-before.CacheHits != 3) {
			t.Fatal("second read not all cache hits")
		}
		drained(t, f.reader.options.Admission)
	}
}

func TestWarmReaderReadLifecycleChangesStillAbort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*storage.PackInfo)
	}{
		{"version", func(p *storage.PackInfo) { p.Version++ }},
		{"generation", func(p *storage.PackInfo) { p.Ref.Generation = "different-generation" }},
		{"size", func(p *storage.PackInfo) { p.Size++ }},
		{"class", func(p *storage.PackInfo) { p.Class = storage.Glacier }},
		{"state", func(p *storage.PackInfo) { p.State = "Retiring" }},
		{"state revision", func(p *storage.PackInfo) { p.StateRevision++ }},
		{"restore expiry", func(p *storage.PackInfo) { p.RestoredUntil = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newWarmFixture(t, 0)
			var out []byte
			changed := false
			_, err := f.reader.Read(context.Background(), f.request, func(_ context.Context, b []byte) error {
				out = append(out, b...)
				if !changed {
					changed = true
					mutateWarmRecord[storage.PackInfo](t, f, "packs", []string{f.ref.ID, f.ref.Generation}, tc.change)
				}
				return nil
			})
			if !errors.Is(err, storage.ErrConflict) || !bytes.Equal(out, f.first) {
				t.Fatal("read-safety change accepted", len(out), err)
			}
			drained(t, f.reader.options.Admission)
		})
	}
}

func TestWarmReaderIncarnationAndDeadlineStillStopAdmittedRead(t *testing.T) {
	for _, mode := range []string{"incarnation", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			f := newWarmFixture(t, 0)
			var out []byte
			changed := false
			_, err := f.reader.Read(context.Background(), f.request, func(_ context.Context, b []byte) error {
				out = append(out, b...)
				if !changed {
					changed = true
					if mode == "incarnation" {
						mutateWarmRecord[storage.Bucket](t, f, "buckets", []string{f.request.Bucket}, func(b *storage.Bucket) { b.ID = "new-incarnation"; b.Revision++ })
					} else {
						return f.clock.Advance(time.Minute)
					}
				}
				return nil
			})
			want := storage.ErrConflict
			if mode == "deadline" {
				want = storage.ErrFenced
			}
			if !errors.Is(err, want) || !bytes.Equal(out, f.first) {
				t.Fatal("view-lifetime check lost", len(out), err)
			}
			drained(t, f.reader.options.Admission)
		})
	}
}

func TestWarmReaderAccessStatsFailureDoesNotFailDeliveredRead(t *testing.T) {
	f := newWarmFixture(t, 0)
	var out []byte
	_, err := f.reader.Read(context.Background(), f.request, func(_ context.Context, b []byte) error {
		out = append(out, b...)
		if len(out) == len(f.body) {
			if err := f.metadata.Close(); err != nil {
				return err
			}
			f.metadata = nil // cleanup skips this intentionally closed fixture adapter
		}
		return nil
	})
	if err != nil || !bytes.Equal(out, f.body) {
		t.Fatal("stats failure changed delivered outcome", len(out), err)
	}
	s := f.reader.options.Metrics.Snapshot()
	if s.AccessStatsErrors != 1 || s.Aborted != 0 || s.EmittedBytes != uint64(len(f.body)) {
		t.Fatal(s)
	}
	drained(t, f.reader.options.Admission)
}
