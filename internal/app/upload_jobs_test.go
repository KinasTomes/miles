package app

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/KinasTomes/miles/internal/storage"
)

// sweepFixture is a scan over uploads whose recovery outcome is scripted per
// upload: outcome sets the state the store keeps and returns the call error.
type sweepFixture struct {
	uploads []storage.MultipartUpload
	outcome map[string]func(*storage.MultipartUpload) error
	calls   []string
}

func (f *sweepFixture) GetJob(context.Context, string) (jobs.Job, error) {
	return jobs.Job{State: jobs.Running}, nil
}

func (f *sweepFixture) ScanUploadCandidate(_ context.Context, after string) (*storage.MultipartUpload, string, error) {
	i := 0
	if after != "" {
		i, _ = strconv.Atoi(after)
	}
	u := f.uploads[i]
	next := ""
	if i+1 < len(f.uploads) {
		next = strconv.Itoa(i + 1)
	}
	return &u, next, nil
}

func (f *sweepFixture) GetUpload(_ context.Context, id string) (storage.MultipartUpload, error) {
	for _, u := range f.uploads {
		if u.ID == id {
			return u, nil
		}
	}
	return storage.MultipartUpload{}, storage.ErrNotFound
}

func (f *sweepFixture) run(id string) error {
	f.calls = append(f.calls, id)
	for i := range f.uploads {
		if f.uploads[i].ID == id {
			return f.outcome[id](&f.uploads[i])
		}
	}
	return storage.ErrNotFound
}

func (f *sweepFixture) RecoverUpload(_ context.Context, id string) (storage.ObjectHead, error) {
	return storage.ObjectHead{}, f.run(id)
}

func (f *sweepFixture) PurgeExpiredUpload(_ context.Context, id string) (storage.MultipartUpload, error) {
	return storage.MultipartUpload{}, f.run(id)
}

type fixedBounds time.Time

func (b fixedBounds) Bounds(context.Context) (storage.R1Bounds, error) {
	return storage.R1Bounds{Lower: time.Time(b), Upper: time.Time(b)}, nil
}

// One upload that cannot settle must not hold the rest of the sweep, an
// unknown outcome must keep the step pending, and recover_uploads leaves an
// Aborting upload to purge_uploads.
func TestRecoverSweepSettlesAroundStuckUploads(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-time.Minute) // lease ended 60 s ago, past its 5 s grace
	set := func(state string, err error) func(*storage.MultipartUpload) error {
		return func(u *storage.MultipartUpload) error { u.State = state; return err }
	}
	completing := func(id string, lease time.Time) storage.MultipartUpload {
		return storage.MultipartUpload{ID: id, State: "Completing", LeaseUntil: lease}
	}
	for _, tc := range []struct {
		name      string
		uploads   []storage.MultipartUpload
		outcome   map[string]func(*storage.MultipartUpload) error
		pending   bool
		calls     int
		code      string
		completed uint64
		skipped   uint64
		deferred  uint64
	}{
		{
			name:    "corrupt input, revoked initiator, recoverable",
			uploads: []storage.MultipartUpload{completing("corrupt", stale), completing("revoked", stale), completing("ok", stale)},
			outcome: map[string]func(*storage.MultipartUpload) error{
				"corrupt": set("Completing", storage.ErrInvalid),
				"revoked": set("Aborting", storage.ErrAccessDenied),
				"ok":      set("Completed", nil),
			},
			calls: 3, code: "uploads_deferred", completed: 1, skipped: 1, deferred: 1,
		},
		{
			name:    "unknown outcome keeps the step",
			uploads: []storage.MultipartUpload{completing("unknown", stale), completing("ok", stale)},
			outcome: map[string]func(*storage.MultipartUpload) error{
				"unknown": set("Completing", storage.ErrUnavailable),
				"ok":      set("Completed", nil),
			},
			pending: true, calls: 1,
		},
		{
			name:    "lease or grace still running",
			uploads: []storage.MultipartUpload{completing("leased", now.Add(-4*time.Second)), completing("ok", stale)},
			outcome: map[string]func(*storage.MultipartUpload) error{"ok": set("Completed", nil)},
			calls:   1, completed: 1, skipped: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &sweepFixture{uploads: tc.uploads, outcome: tc.outcome}
			w := uploadJobs{store: f, utc: fixedBounds(now), objects: func(context.Context, string) (uploadRecovery, error) { return f, nil }}
			step, err := w.step(context.Background(), jobs.Job{ID: "job"}, "recover_uploads")
			if len(f.calls) != tc.calls {
				t.Fatal("recovered", f.calls)
			}
			if tc.pending {
				if !errors.Is(err, storage.ErrUnavailable) || step.Settled {
					t.Fatal("unknown outcome settled the step", step, err)
				}
				if f.uploads[0].State != "Completing" {
					t.Fatal(f.uploads[0].State)
				}
				return
			}
			var c uploadCheckpoint
			if err != nil || !step.Settled || !step.Done || step.ErrorCode != tc.code || json.Unmarshal(step.Result, &c) != nil {
				t.Fatal(step, err)
			}
			if c.Completed != tc.completed || c.Skipped != tc.skipped || c.Deferred != tc.deferred || c.Examined != uint64(len(tc.uploads)) {
				t.Fatalf("%+v", c)
			}
		})
	}
}
