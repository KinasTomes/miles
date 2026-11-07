package lifecycle

import (
	s "github.com/KinasTomes/miles/internal/storage"
	"time"
)

// RetentionUntil starts from the conservative upper completion bound. The
// storage-class minimum is independent of configurable policy age and R2 cuts.
// This is a deletion floor, never a deletion authorization or AWS billing proof.
func RetentionUntil(class s.StorageClass, upper, existing time.Time) (time.Time, error) {
	var duration time.Duration
	switch class {
	case s.GlacierIR, s.Glacier:
		duration = 90 * 24 * time.Hour
	case s.StandardIA:
		duration = 30 * 24 * time.Hour
	default:
		return time.Time{}, s.ErrInvalid
	}
	if upper.IsZero() || upper.UTC().Year() < 1 || upper.UTC().Year() > 9999 || !existing.IsZero() && (existing.UTC().Year() < 1 || existing.UTC().Year() > 9999) {
		return time.Time{}, s.ErrInvalid
	}
	end := upper.UTC().Add(duration)
	if end.Year() > 9999 || end.Before(upper) {
		return time.Time{}, s.ErrInvalid
	}
	if existing.After(end) {
		end = existing.UTC()
	}
	return end, nil
}
