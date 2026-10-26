package readpath

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestPackAccessBatchesAreBoundedExactAndDeterministic(t *testing.T) {
	at := time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	observed := map[storage.PackRef]time.Time{}
	for i := 0; i < 253; i++ {
		ref := storage.PackRef{ID: fmt.Sprintf("pack-%03d", i/2), Generation: fmt.Sprint(i % 2)}
		observed[ref] = at.Add(time.Duration(i) * time.Nanosecond)
	}
	before := map[storage.PackRef]time.Time{}
	for k, v := range observed {
		before[k] = v
	}
	var previous []storage.PackAccess
	for run := 0; run < 2; run++ {
		var got []storage.PackAccess
		calls := 0
		err := persistPackAccess(context.Background(), observed, func(_ context.Context, b []storage.PackAccess) error {
			calls++
			if len(b) == 0 || len(b) > 100 {
				t.Fatal("unbounded batch", len(b))
			}
			got = append(got, b...)
			return nil
		})
		if err != nil || calls != 3 || len(got) != len(observed) {
			t.Fatal(calls, len(got), err)
		}
		seen := map[storage.PackRef]bool{}
		for i, v := range got {
			if seen[v.Pack] || !v.LastAccess.Equal(observed[v.Pack]) {
				t.Fatal("wrong/missing generation observation", v)
			}
			seen[v.Pack] = true
			if i > 0 {
				a, b := got[i-1].Pack, v.Pack
				if a.ID > b.ID || a.ID == b.ID && a.Generation >= b.Generation {
					t.Fatal("unstable order", a, b)
				}
			}
		}
		if run == 1 && !reflect.DeepEqual(previous, got) {
			t.Fatal("nondeterministic")
		}
		previous = got
	}
	if !reflect.DeepEqual(before, observed) {
		t.Fatal("mutated observation map")
	}
}

func TestPackAccessFlushStopsOnErrorWithoutRetry(t *testing.T) {
	observed := map[storage.PackRef]time.Time{}
	for i := 0; i < 201; i++ {
		observed[storage.PackRef{ID: fmt.Sprint(i), Generation: "g"}] = time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)
	}
	failure := errors.New("stats unavailable")
	calls := 0
	err := persistPackAccess(context.Background(), observed, func(context.Context, []storage.PackAccess) error { calls++; return failure })
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatal("hidden retry or ignored failure", calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	err = persistPackAccess(ctx, observed, func(context.Context, []storage.PackAccess) error { calls++; return nil })
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatal("write after cancellation", calls, err)
	}
}
