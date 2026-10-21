// Package clock separates persisted wall UTC from process-local elapsed time.
// It supplies no UTC uncertainty proof, lease authority, or recovery identity.
//
// Concrete APIs are a Phase 1 PROPOSAL for consumer review, not a frozen API.
// Instants must not be serialized, transferred between clocks, or restored after
// restart. A wakeup never grants permission for a side effect: callers recheck
// cancellation, deadlines, and their independent authority/protection proofs.
package clock

import (
	"context"
	"errors"
	"math"
	"time"
)

var (
	ErrDuration      = errors.New("clock: negative duration")
	ErrOverflow      = errors.New("clock: time arithmetic overflow")
	ErrDomain        = errors.New("clock: invalid or incompatible instant domain")
	ErrBudget        = errors.New("clock: event budget must be positive")
	ErrEventLimit    = errors.New("clock: advance event budget exhausted")
	ErrAdvancing     = errors.New("clock: advance already in progress")
	ErrCallback      = errors.New("clock: nil callback")
	ErrSerialization = errors.New("clock: process-local instant cannot be JSON serialized")
)

// Clock can be shared by runtime and simulator consumers. Constructors are
// required; zero-value Real and Virtual clocks are not usable.
type Clock interface {
	Now() Reading
	Sleep(context.Context, time.Duration) error
}

// Reading pairs observations; WallUTC is not an uncertainty interval or a source
// of elapsed deadlines. Real observations are sampled from one time.Now call.
// Reading is process-local too: JSON encoding rejects its Instant field. Persist
// WallUTC separately where a timestamp is required; never persist elapsed rights.
type Reading struct {
	WallUTC time.Time
	Instant Instant
}

// domain has nonzero size so distinct allocations cannot compare equal.
type domain struct{ marker byte }

// Instant is an opaque signed nanosecond coordinate in one Clock's domain.
// A zero value is invalid. The representation is intentionally private and may
// change after consumer review. The representable horizon is time.Duration.
// It is deliberately not Go-comparable: use Compare to validate the domain.
type Instant struct {
	_      [0]func()
	domain *domain
	ticks  int64
}

// MarshalJSON explicitly rejects persistence of local elapsed coordinates.
// This is a Phase 1 proposal enforcing W1's no-serialization requirement.
func (Instant) MarshalJSON() ([]byte, error) { return nil, ErrSerialization }

// UnmarshalJSON rejects reconstruction, including into an existing valid
// Instant. The receiver is left unchanged on failure.
func (*Instant) UnmarshalJSON([]byte) error { return ErrSerialization }

// Add accepts signed offsets, including negative offsets. Sleep and Advance,
// unlike arithmetic, reject negative durations.
func (i Instant) Add(d time.Duration) (Instant, error) {
	if i.domain == nil {
		return Instant{}, ErrDomain
	}
	n, err := add(i.ticks, int64(d))
	if err != nil {
		return Instant{}, err
	}
	return Instant{domain: i.domain, ticks: n}, nil
}

func (i Instant) Sub(other Instant) (time.Duration, error) {
	if _, err := i.Compare(other); err != nil {
		return 0, err
	}
	if other.ticks > 0 && i.ticks < math.MinInt64+other.ticks || other.ticks < 0 && i.ticks > math.MaxInt64+other.ticks {
		return 0, ErrOverflow
	}
	return time.Duration(i.ticks - other.ticks), nil
}

// Compare returns -1, 0, or 1. Equality is expired for a hard deadline:
// now.Compare(deadline) >= 0, after checking the returned error.
func (i Instant) Compare(other Instant) (int, error) {
	if i.domain == nil || other.domain == nil || i.domain != other.domain {
		return 0, ErrDomain
	}
	if i.ticks < other.ticks {
		return -1, nil
	}
	if i.ticks > other.ticks {
		return 1, nil
	}
	return 0, nil
}

func add(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, ErrOverflow
	}
	return a + b, nil
}
