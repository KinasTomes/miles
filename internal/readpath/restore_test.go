package readpath

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/coordination"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
)

func restoreHeaderAt(t time.Time) *string {
	return aws.String(`ongoing-request="false", expiry-date="` + t.Format(http.TimeFormat) + `"`)
}

// RestoreObject pins like GET, reads no payload, and asks the backend only for
// archive packs that are neither restoring nor restored for the requested Days.
// Days=2 from a fixture clock whose deadline+120s threshold is now+3min.
func TestRestoreClassifiesArchivePacksWithoutPayload(t *testing.T) {
	const days = 2
	cases := []struct {
		name    string
		class   storage.StorageClass
		backend func(now time.Time) *string
		created bool
		called  int
		want    RestoreResult
	}{
		{name: "IR is readable, nothing to restore", class: storage.GlacierIR},
		{name: "archive starts a restore", class: storage.Glacier, created: true, called: 1, want: RestoreResult{Archive: 1, Initiated: 1}},
		{name: "archive job already queued", class: storage.Glacier, called: 1, want: RestoreResult{Archive: 1, InProgress: 1}},
		{name: "backend restoring", class: storage.Glacier, backend: func(time.Time) *string { return aws.String(`ongoing-request="true"`) }, want: RestoreResult{Archive: 1, InProgress: 1}},
		{name: "restored for Days needs nothing", class: storage.Glacier, backend: func(now time.Time) *string { return restoreHeaderAt(now.Add(days * 24 * time.Hour)) }, want: RestoreResult{Archive: 1}},
		{name: "shorter restored copy is renewed", class: storage.Glacier, backend: func(now time.Time) *string { return restoreHeaderAt(now.Add(days*24*time.Hour - time.Second)) }, created: true, called: 1, want: RestoreResult{Archive: 1, Initiated: 1}},
		{name: "expired copy is restored again", class: storage.Glacier, backend: func(now time.Time) *string { return restoreHeaderAt(now.Add(-time.Hour)) }, created: true, called: 1, want: RestoreResult{Archive: 1, Initiated: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := coldFixture(t, tc.class, "Cold", time.Time{})
			now := f.clock.Now().WallUTC
			if tc.backend != nil {
				f.backend.restore = tc.backend(now)
			}
			calls := 0
			f.reader.options.Restore = func(ctx context.Context, view *coordination.WarmView, head storage.ObjectHead, ref storage.PackRef, revision uint64, required time.Time, explicit *storage.RestoreRequest) (bool, error) {
				calls++
				if view.Check(ctx) != nil || ref != f.ref || head.ManifestID == "" || revision != f.reader.options.Global.Revision() || !required.Equal(now.Add(3*time.Minute)) {
					t.Fatal("unbound restore", head, ref, revision, required)
				}
				if explicit == nil || *explicit != (storage.RestoreRequest{Days: days, Tier: "Bulk"}) {
					t.Fatal("request parameters lost", explicit)
				}
				if !errors.Is(f.reader.options.Gate.Retire(ref), storage.ErrConflict) {
					t.Fatal("restore without pin")
				}
				return tc.created, nil
			}
			got, err := f.reader.Restore(t.Context(), f.request, storage.RestoreRequest{Days: days, Tier: "Bulk"})
			if err != nil || got != tc.want || calls != tc.called || f.backend.count() != 0 {
				t.Fatalf("result %+v, err %v, calls %d, payload GETs %d", got, err, calls, f.backend.count())
			}
			drained(t, f.reader.options.Admission)
		})
	}
}

// headSpy counts backend HEADs and can rewrite what the backend reports.
type headSpy struct {
	storage.PackStore
	heads  int
	mutate func(*storage.PackInfo)
}

func (h *headSpy) HeadPack(ctx context.Context, ref storage.PackRef) (storage.PackInfo, error) {
	h.heads++
	info, err := h.PackStore.HeadPack(ctx, ref)
	if err == nil && h.mutate != nil {
		h.mutate(&info)
	}
	return info, err
}

func TestRestoreRejectsBeforeSideEffects(t *testing.T) {
	f := coldFixture(t, storage.Glacier, "Cold", time.Time{})
	calls := 0
	f.reader.options.Restore = func(context.Context, *coordination.WarmView, storage.ObjectHead, storage.PackRef, uint64, time.Time, *storage.RestoreRequest) (bool, error) {
		calls++
		return true, nil
	}
	spy := &headSpy{PackStore: f.reader.options.Packs}
	f.reader.options.Packs = spy
	for _, bad := range []storage.RestoreRequest{{Days: 0, Tier: "Standard"}, {Days: 31, Tier: "Standard"}, {Days: 1}, {Days: 1, Tier: "InvalidTier"}, {Days: 1, Tier: "standard"}} {
		if _, err := f.reader.Restore(t.Context(), f.request, bad); !errors.Is(err, storage.ErrInvalid) || spy.heads != 0 {
			t.Fatal(bad, err, spy.heads)
		}
	}
	// No trusted UTC means no restore threshold: nothing is queued.
	f.reader.options.UTC = nil
	if _, err := f.reader.Restore(t.Context(), f.request, storage.RestoreRequest{Days: 1, Tier: "Standard"}); err == nil {
		t.Fatal("restore without UTC proof")
	}
	// A runtime without the restore port never pretends to accept.
	f.reader.options.Restore = nil
	if _, err := f.reader.Restore(t.Context(), f.request, storage.RestoreRequest{Days: 1, Tier: "Standard"}); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatal(err)
	}
	// Read permission is the admission gate, as for GET.
	q := f.request
	q.CredentialID = "missing-key"
	if _, err := f.reader.Restore(t.Context(), q, storage.RestoreRequest{Days: 1, Tier: "Standard"}); err == nil {
		t.Fatal("restore without credential")
	}
	if calls != 0 {
		t.Fatal("rejected request queued restore", calls)
	}
	drained(t, f.reader.options.Admission)
}

// A backend pack that no longer matches the pinned record queues nothing.
func TestRestoreRejectsPhysicalMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*storage.PackInfo)
	}{
		{"size +1", func(p *storage.PackInfo) { p.Size++ }},
		{"other generation", func(p *storage.PackInfo) { p.Ref.Generation = "generation-other" }},
		{"class changed", func(p *storage.PackInfo) { p.Class, p.State = storage.GlacierIR, "Cold" }},
		{"state not archive", func(p *storage.PackInfo) { p.State = "Warm" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := coldFixture(t, storage.Glacier, "Cold", time.Time{})
			calls := 0
			f.reader.options.Restore = func(context.Context, *coordination.WarmView, storage.ObjectHead, storage.PackRef, uint64, time.Time, *storage.RestoreRequest) (bool, error) {
				calls++
				return true, nil
			}
			f.reader.options.Packs = &headSpy{PackStore: f.reader.options.Packs, mutate: tc.mutate}
			out, err := f.reader.Restore(t.Context(), f.request, storage.RestoreRequest{Days: 1, Tier: "Standard"})
			if !errors.Is(err, storage.ErrConflict) || calls != 0 || out.Initiated != 0 || f.backend.count() != 0 {
				t.Fatal(out, err, calls)
			}
			drained(t, f.reader.options.Admission)
		})
	}
}

// HEAD's x-amz-restore input comes from backend HEADs of the archive packs.
func TestArchiveStateReportsBackendRestore(t *testing.T) {
	cases := []struct {
		name    string
		class   storage.StorageClass
		backend func(now time.Time) *string
		want    func(now time.Time) ArchiveState
	}{
		{"IR has no archive", storage.GlacierIR, nil, func(time.Time) ArchiveState { return ArchiveState{} }},
		{"never requested", storage.Glacier, nil, func(time.Time) ArchiveState { return ArchiveState{Archive: 1} }},
		{"restoring", storage.Glacier, func(time.Time) *string { return aws.String(`ongoing-request="true"`) }, func(time.Time) ArchiveState { return ArchiveState{Archive: 1, Restoring: 1} }},
		{"restored", storage.Glacier, func(now time.Time) *string { return restoreHeaderAt(now.Add(time.Hour)) }, func(now time.Time) ArchiveState {
			return ArchiveState{Archive: 1, Restored: 1, Expiry: now.Add(time.Hour).Truncate(time.Second)}
		}},
		{"expired", storage.Glacier, func(now time.Time) *string { return restoreHeaderAt(now.Add(-time.Second)) }, func(time.Time) ArchiveState { return ArchiveState{Archive: 1} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := coldFixture(t, tc.class, "Cold", time.Time{})
			now := f.clock.Now().WallUTC
			if tc.backend != nil {
				f.backend.restore = tc.backend(now)
			}
			head, err := f.metadata.GetObject(t.Context(), storage.ObjectKey{Bucket: "demo", BucketID: f.request.BucketPolicy.BucketID(), Key: "file"})
			if err != nil {
				t.Fatal(err)
			}
			got, err := f.reader.ArchiveState(t.Context(), head)
			want := tc.want(now)
			if err != nil || got.Archive != want.Archive || got.Restoring != want.Restoring || got.Restored != want.Restored || !got.Expiry.Equal(want.Expiry) || f.backend.count() != 0 {
				t.Fatalf("got %+v, want %+v, err %v", got, want, err)
			}
		})
	}
}
