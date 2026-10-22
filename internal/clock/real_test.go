package clock_test

import (
	"context"
	"errors"
	"testing"
	"time"

	c "github.com/KinasTomes/miles/internal/clock"
)

func TestRealReadingAndSleep(t *testing.T) {
	r := c.NewReal()
	before := r.Now()
	if before.WallUTC.Location() != time.UTC || before.WallUTC.IsZero() {
		t.Fatal(before)
	}
	if err := r.Sleep(context.Background(), time.Nanosecond); err != nil {
		t.Fatal(err)
	}
	after := r.Now()
	if n, err := after.Instant.Sub(before.Instant); err != nil || n < time.Nanosecond {
		t.Fatal(n, err)
	}
	if _, err := before.Instant.Compare(c.NewReal().Now().Instant); !errors.Is(err, c.ErrDomain) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- r.Sleep(ctx, time.Hour) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("real sleep did not cancel")
	}
}
