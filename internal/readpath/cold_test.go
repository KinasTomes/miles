package readpath

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type readUTC func(context.Context) (storage.R1Bounds, error)

func (f readUTC) Bounds(ctx context.Context) (storage.R1Bounds, error) { return f(ctx) }

func coldFixture(t *testing.T, class storage.StorageClass, state string, expiry time.Time) *warmFixture {
	t.Helper()
	f := newWarmFixture(t, 0)
	mutateWarmRecord[storage.PackInfo](t, f, "packs", []string{f.ref.ID, f.ref.Generation}, func(p *storage.PackInfo) {
		p.Class, p.State, p.RestoredUntil = class, state, expiry
		p.StateRevision++
	})
	f.backend.class = types.StorageClass(class)
	if !expiry.IsZero() {
		f.backend.restore = aws.String(`ongoing-request="false", expiry-date="` + expiry.Format(http.TimeFormat) + `"`)
	}
	f.reader.options.UTC = readUTC(func(ctx context.Context) (storage.R1Bounds, error) {
		now := f.clock.Now().WallUTC
		return storage.R1Bounds{Lower: now, Upper: now, Horizon: now.Add(time.Hour)}, ctx.Err()
	})
	return f
}

func TestColdReadIRAndArchivePreflight(t *testing.T) {
	for _, class := range []storage.StorageClass{storage.GlacierIR, storage.Glacier} {
		t.Run(string(class), func(t *testing.T) {
			f := coldFixture(t, class, "Cold", time.Time{})
			for pass := range 2 {
				before := f.backend.count()
				_, got, err := f.read(t.Context(), f.request)
				if class == storage.Glacier {
					if !errors.Is(err, storage.ErrCold) || len(got) != 0 || f.backend.count() != 0 {
						t.Fatal(got, err)
					}
				} else if err != nil || !bytes.Equal(got, f.body) || pass == 1 && f.backend.count() != before {
					t.Fatal(got, err)
				}
				drained(t, f.reader.options.Admission)
			}
		})
	}
}

func TestRestoredReadBackendExpiryDeadlineAndUTC(t *testing.T) {
	for _, delta := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
		t.Run(delta.String(), func(t *testing.T) {
			f := newWarmFixture(t, 0)
			now := f.clock.Now().WallUTC
			// S3 exposes seconds; -1ns encodes the previous second, while equality
			// and +1ns encode the exact accepted boundary. Pure policy tests cover ns.
			f = coldFixture(t, storage.Glacier, "Restored", now.Add(3*time.Minute).Add(delta))
			for range 2 {
				_, got, err := f.read(t.Context(), f.request)
				if delta < 0 {
					if !errors.Is(err, storage.ErrCold) || len(got) != 0 || f.backend.count() != 0 {
						t.Fatal(got, err)
					}
				} else if err != nil || !bytes.Equal(got, f.body) {
					t.Fatal(got, err)
				}
				drained(t, f.reader.options.Admission)
			}
		})
	}
	t.Run("missing UTC does not authorize cached restore", func(t *testing.T) {
		f := coldFixture(t, storage.Glacier, "Restored", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
		f.reader.options.UTC = nil
		_, got, err := f.read(t.Context(), f.request)
		if err == nil || len(got) != 0 || f.backend.count() != 0 {
			t.Fatal(got, err)
		}
		drained(t, f.reader.options.Admission)
	})
}

func TestFlexibleRestoreTimelineWithVirtualClock(t *testing.T) {
	f := coldFixture(t, storage.Glacier, "Cold", time.Time{})
	const requestBudget = time.Minute
	const restoreDelay = 4 * time.Hour // Fixture delay, independent of AWS latency.
	const restoredLifetime = 24 * time.Hour
	expiry := f.clock.Now().WallUTC.Add(restoreDelay + restoredLifetime)
	// Model a backend acknowledgement followed by a separate completion event.
	// GET uses the production S3 adapter's HEAD parser at each step.
	f.backend.restore = aws.String(`ongoing-request="true"`)
	if _, err := f.clock.AfterFunc(restoreDelay, func() {
		f.backend.restore = aws.String(`ongoing-request="false", expiry-date="` + expiry.Format(http.TimeFormat) + `"`)
	}); err != nil {
		t.Fatal(err)
	}
	advance := func(d time.Duration) {
		t.Helper()
		if err := f.clock.Advance(d); err != nil {
			t.Fatal(err)
		}
	}
	checkRead := func(readable bool) {
		t.Helper()
		q := f.request
		var err error
		q.Deadline, err = f.clock.Now().Instant.Add(requestBudget)
		if err != nil {
			t.Fatal(err)
		}
		ready := false
		q.Ready = func(WarmResult) error { ready = true; return nil }
		before := f.reader.options.Metrics.Snapshot()
		gets := f.backend.count()
		_, got, err := f.read(t.Context(), q)
		drained(t, f.reader.options.Admission)
		if readable {
			if err != nil || !ready || !bytes.Equal(got, f.body) {
				t.Fatalf("read at %v: ready=%t, bytes=%d, err=%v", f.clock.Now().WallUTC, ready, len(got), err)
			}
			return
		}
		after := f.reader.options.Metrics.Snapshot()
		if !errors.Is(err, storage.ErrCold) || ready || len(got) != 0 || f.backend.count() != gets ||
			after.CacheHits != before.CacheHits || after.CacheMisses != before.CacheMisses {
			t.Fatalf("archive read at %v: ready=%t, bytes=%d, err=%v, GETs=%d→%d, metrics=%+v→%+v", f.clock.Now().WallUTC, ready, len(got), err, gets, f.backend.count(), before, after)
		}
	}
	checkRead(false) // Restore accepted, but still pending.
	advance(restoreDelay - time.Nanosecond)
	checkRead(false) // Still pending after nearly four hours.
	advance(time.Nanosecond)
	gets := f.backend.count()
	checkRead(true) // Completion HEAD provides expiry; backend fill is now allowed.
	if f.backend.count() <= gets {
		t.Fatal("completed restore did not fetch the backend on cache miss")
	}
	gets = f.backend.count()
	hits := f.reader.options.Metrics.Snapshot().CacheHits
	advance(restoredLifetime - requestBudget - 120*time.Second)
	checkRead(true) // Equality meets the margin and reuses the resident cache.
	if f.backend.count() != gets || f.reader.options.Metrics.Snapshot().CacheHits <= hits {
		t.Fatal("eligible read did not hit the resident cache")
	}
	advance(time.Nanosecond)
	checkRead(false) // Expiry still lies ahead, but no longer covers deadline +120s.
	advance(requestBudget + 120*time.Second - time.Nanosecond)
	checkRead(false) // Even a stale completion header cannot revive expired bytes.
}

func TestColdCacheNeverBypassesExpiryAndAutoRestore(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "disabled", true: "enabled"}[enabled], func(t *testing.T) {
			f := coldFixture(t, storage.Glacier, "Restored", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
			if _, _, err := f.read(t.Context(), f.request); err != nil {
				t.Fatal(err)
			}
			before := f.backend.count()
			beforeCache := f.reader.options.Metrics.Snapshot()
			// Keep resident bytes while the backend reports restore pending.
			f.backend.restore = aws.String(`ongoing-request="true"`)
			v, _ := config.NewPolicyValidator(config.DSGPolicyLimits())
			p := f.reader.options.Global.Policy()
			p.Tiering.AutoRestore = enabled
			f.reader.options.Global, _ = v.NewGlobalSnapshot(2, p)
			calls := 0
			f.reader.options.Restore = func(ctx context.Context, view *coordination.WarmView, head storage.ObjectHead, ref storage.PackRef, revision uint64, required time.Time, explicit *storage.RestoreRequest) (bool, error) {
				calls++
				if view.Check(ctx) != nil || ref != f.ref || revision != 2 || head.ManifestID == "" || !required.Equal(f.clock.Now().WallUTC.Add(3*time.Minute)) || explicit != nil {
					t.Fatal("unbound restore", head, ref, required)
				}
				if !errors.Is(f.reader.options.Gate.Retire(ref), storage.ErrConflict) {
					t.Fatal("restore without pin")
				}
				return false, storage.ErrUnavailable // diagnostics cannot change archive error
			}
			f.request.Ready = func(WarmResult) error { t.Error("archive committed headers"); return nil }
			_, got, err := f.read(t.Context(), f.request)
			want := 0
			if enabled {
				want = 1
			}
			if !errors.Is(err, storage.ErrCold) || len(got) != 0 || f.backend.count() != before || calls != want || f.reader.options.Metrics.Snapshot().RestoreErrors != uint64(want) {
				t.Fatal(got, err, calls)
			}
			afterCache := f.reader.options.Metrics.Snapshot()
			if afterCache.CacheHits != beforeCache.CacheHits || afterCache.CacheMisses != beforeCache.CacheMisses {
				t.Fatal("cache was accessed before archive preflight", beforeCache, afterCache)
			}
			drained(t, f.reader.options.Admission)
		})
	}
}

func TestRestoredReadRechecksBeforeOrderedEmit(t *testing.T) {
	f := coldFixture(t, storage.Glacier, "Restored", time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC))
	count := 0
	_, err := f.reader.Read(t.Context(), f.request, func(context.Context, []byte) error {
		count++
		f.backend.mu.Lock()
		f.backend.restore = aws.String(`ongoing-request="true"`)
		f.backend.mu.Unlock()
		return nil
	})
	if err == nil || count != 1 {
		t.Fatal(count, err)
	}
	drained(t, f.reader.options.Admission)
}

func TestColdRestoreErrorStateForDebugHeader(t *testing.T) {
	for _, tc := range []struct {
		name        string
		autoRestore bool
		created     bool
		restoreErr  error
		wantState   string
	}{
		{name: "requested", autoRestore: true, created: true, wantState: "requested"},
		{name: "in-progress", autoRestore: true, created: false, wantState: "in-progress"},
		{name: "auto_restore disabled", autoRestore: false, created: true, wantState: ""},
		{name: "restore failed", autoRestore: true, restoreErr: storage.ErrUnavailable, wantState: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := coldFixture(t, storage.Glacier, "Cold", time.Time{})
			v, err := config.NewPolicyValidator(config.DSGPolicyLimits())
			if err != nil {
				t.Fatal(err)
			}
			gp := config.DefaultGlobalPolicy()
			gp.Tiering.AutoRestore = tc.autoRestore
			gs, err := v.NewGlobalSnapshot(2, gp)
			if err != nil {
				t.Fatal(err)
			}
			f.reader.options.Global = gs
			f.reader.options.Restore = func(context.Context, *coordination.WarmView, storage.ObjectHead, storage.PackRef, uint64, time.Time, *storage.RestoreRequest) (bool, error) {
				return tc.created, tc.restoreErr
			}
			_, _, err = f.read(t.Context(), f.request)
			if !errors.Is(err, storage.ErrCold) {
				t.Fatalf("got err=%v, want storage.ErrCold", err)
			}
			var cold *ColdRestoreError
			if tc.wantState == "" {
				if errors.As(err, &cold) {
					t.Fatalf("unexpected ColdRestoreError %+v", cold)
				}
			} else if !errors.As(err, &cold) || cold.State != tc.wantState {
				t.Fatalf("got cold=%+v (err=%v), want state %q", cold, err, tc.wantState)
			}
			drained(t, f.reader.options.Admission)
		})
	}
}
