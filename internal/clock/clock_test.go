package clock_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	c "github.com/KinasTomes/miles/internal/clock"
)

func virtual(t *testing.T, budget int) *c.Virtual {
	t.Helper()
	v, err := c.NewVirtual(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC), budget)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestInstantDeadlineAndDomains(t *testing.T) {
	if reflect.TypeFor[c.Instant]().Comparable() {
		t.Fatal("Instant must require domain-checked Compare instead of Go ==")
	}
	v := virtual(t, 10)
	start := v.Now().Instant
	d, err := start.Add(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		advance time.Duration
		order   int
	}{
		{time.Second - time.Nanosecond, -1}, {time.Nanosecond, 0}, {time.Nanosecond, 1},
	} {
		if err := v.Advance(tc.advance); err != nil {
			t.Fatal(err)
		}
		if order, err := v.Now().Instant.Compare(d); err != nil || order != tc.order {
			t.Fatalf("compare = %d, %v", order, err)
		}
	}
	if delta, err := d.Sub(start); err != nil || delta != time.Second {
		t.Fatal(delta, err)
	}
	for _, other := range []c.Instant{c.Instant{}, virtual(t, 10).Now().Instant, c.NewReal().Now().Instant} {
		if _, err := start.Compare(other); !errors.Is(err, c.ErrDomain) {
			t.Fatal(err)
		}
		if _, err := start.Sub(other); !errors.Is(err, c.ErrDomain) {
			t.Fatal(err)
		}
	}
	if _, err := (c.Instant{}).Add(0); !errors.Is(err, c.ErrDomain) {
		t.Fatal(err)
	}
	if _, err := (c.Instant{}).Compare(start); !errors.Is(err, c.ErrDomain) {
		t.Fatal(err)
	}
}

func TestInstantOverflow(t *testing.T) {
	start := virtual(t, 10).Now().Instant
	hi, err := start.Add(time.Duration(math.MaxInt64))
	if err != nil {
		t.Fatal(err)
	}
	lo, err := start.Add(time.Duration(math.MinInt64))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		i c.Instant
		d time.Duration
	}{{hi, 1}, {lo, -1}} {
		if _, err := tc.i.Add(tc.d); !errors.Is(err, c.ErrOverflow) {
			t.Fatal(err)
		}
	}
	for _, pair := range [][2]c.Instant{{hi, lo}, {lo, hi}, {start, lo}} {
		if _, err := pair[0].Sub(pair[1]); !errors.Is(err, c.ErrOverflow) {
			t.Fatal(err)
		}
	}
	if n, err := lo.Sub(start); err != nil || n != time.Duration(math.MinInt64) {
		t.Fatal(n, err)
	}
	if n, err := hi.Sub(start); err != nil || n != time.Duration(math.MaxInt64) {
		t.Fatal(n, err)
	}
}

func TestSharedSleepValidation(t *testing.T) {
	for _, clk := range []c.Clock{c.NewReal(), virtual(t, 10)} {
		if err := clk.Sleep(context.Background(), -1); !errors.Is(err, c.ErrDuration) {
			t.Fatal(err)
		}
		before := clk.Now()
		if err := clk.Sleep(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		if v, ok := clk.(*c.Virtual); ok {
			if delta, err := v.Now().Instant.Sub(before.Instant); err != nil || delta != 0 {
				t.Fatal(delta, err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		for _, d := range []time.Duration{0, time.Hour} {
			if err := clk.Sleep(ctx, d); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		}
		if err := clk.Sleep(ctx, -1); !errors.Is(err, c.ErrDuration) {
			t.Fatal(err)
		}
	}
}

func TestInstantRejectsJSON(t *testing.T) {
	v := virtual(t, 10)
	reading := v.Now()
	valid := reading.Instant
	for _, value := range []any{valid, &valid, c.Instant{}, reading, &reading} {
		if _, err := json.Marshal(value); !errors.Is(err, c.ErrSerialization) {
			t.Fatalf("marshal %T: %v", value, err)
		}
	}
	for _, payload := range []string{`{}`, `{"ticks":123,"domain":1}`, `null`, `0`, `"instant"`, `[]`} {
		var zero c.Instant
		if err := json.Unmarshal([]byte(payload), &zero); !errors.Is(err, c.ErrSerialization) {
			t.Fatalf("unmarshal zero %s: %v", payload, err)
		}
		if _, err := zero.Compare(valid); !errors.Is(err, c.ErrDomain) {
			t.Fatal("JSON created a valid Instant", err)
		}
		copyOfValid := valid
		if err := json.Unmarshal([]byte(payload), &copyOfValid); !errors.Is(err, c.ErrSerialization) {
			t.Fatalf("unmarshal existing %s: %v", payload, err)
		}
		if cmp, err := copyOfValid.Compare(valid); err != nil || cmp != 0 {
			t.Fatal("rejected JSON changed the receiver", cmp, err)
		}
	}
	var decoded c.Reading
	if err := json.Unmarshal([]byte(`{"Instant":{}}`), &decoded); !errors.Is(err, c.ErrSerialization) {
		t.Fatal(err)
	}
	// The in-process API and standalone persisted wall timestamps remain usable.
	if cmp, err := reading.Instant.Compare(valid); err != nil || cmp != 0 {
		t.Fatal(cmp, err)
	}
	wallJSON, err := json.Marshal(reading.WallUTC)
	if err != nil {
		t.Fatal(err)
	}
	var wall time.Time
	if err := json.Unmarshal(wallJSON, &wall); err != nil || !wall.Equal(reading.WallUTC) {
		t.Fatal(wall, err)
	}
}
