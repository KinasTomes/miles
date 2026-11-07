package lifecycle

import (
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
	"time"
)

func TestMinimumRetentionIsIndependentOfPolicyAge(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, class := range []s.StorageClass{s.GlacierIR, s.Glacier, s.StandardIA} {
		want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
		if class == s.StandardIA {
			want = time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
		}
		got, e := RetentionUntil(class, start, time.Time{})
		if e != nil || !got.Equal(want) {
			t.Fatal(class, got, want, e)
		}
		old := want.Add(time.Nanosecond)
		got, e = RetentionUntil(class, start, old)
		if e != nil || !got.Equal(old) {
			t.Fatal("reduced old retention", got, e)
		}
	}
	if _, e := RetentionUntil(s.Glacier, time.Date(9999, 12, 1, 0, 0, 0, 0, time.UTC), time.Time{}); e == nil {
		t.Fatal("retention overflow accepted")
	}
	if _, e := RetentionUntil(s.StorageClass("DEEP_ARCHIVE"), start, time.Time{}); e == nil {
		t.Fatal("unsupported P2 class")
	}
}
