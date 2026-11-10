package object

import (
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/readpath"
)

// HEAD aggregates every archive pack of the object (API v2 4.4).
func TestRestoreHeaderAggregatesArchivePacks(t *testing.T) {
	expiry := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		state readpath.ArchiveState
		want  string
	}{
		{"Warm/IR only", readpath.ArchiveState{}, ""},
		{"never requested", readpath.ArchiveState{Archive: 2}, ""},
		{"partly restored, none running", readpath.ArchiveState{Archive: 2, Restored: 1, Expiry: expiry}, ""},
		{"one running", readpath.ArchiveState{Archive: 3, Restoring: 1, Restored: 1, Expiry: expiry}, `ongoing-request="true"`},
		{"all restored: earliest expiry", readpath.ArchiveState{Archive: 2, Restored: 2, Expiry: expiry}, `ongoing-request="false", expiry-date="Fri, 13 Nov 2026 00:00:00 GMT"`},
	} {
		if got := restoreHeader(tc.state); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}
