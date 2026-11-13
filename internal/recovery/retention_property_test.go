package recovery

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
)

func TestRetentionPinAndPruneSequencesPreservePhysicalGenerations(t *testing.T) {
	for _, seed := range []int64{1, 7, 42, 2026} {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			r, cuts, locations := generationCuts(t)
			rng := rand.New(rand.NewSource(seed))
			start := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
			deadline := start.Add(ProtectedWindow + RetentionBuffer)
			// This independent model tracks advertised epochs and consumer pins.
			// The implementation must prove every resulting location physically.
			present := map[int]bool{}
			pins := map[int]map[string]bool{}
			for i := range cuts {
				present[i], pins[i] = true, map[string]bool{}
			}
			var trace []string
			defer func() {
				if t.Failed() {
					t.Logf("seed=%d trace=%v", seed, trace)
				}
			}()
			check := func() {
				t.Helper()
				roots, err := r.EnumerateProtectedData(t.Context(), 9)
				if err != nil || len(roots) != len(present) {
					t.Fatal("advertised inventory mismatch", roots, present, err)
				}
				want := map[s.PackRef]bool{}
				for _, root := range roots {
					i := int(root.Cut.Epoch)
					if !present[i] || root.Cut.Checkpoint != cuts[i] {
						t.Fatal("withdrawn or rebound cut", root.Cut)
					}
					if i != 3 {
						want[locations[min(i, 2)].Location.Pack] = true
					}
				}
				got, err := r.ProtectedRoots(t.Context())
				if err != nil || len(got) != len(want) {
					t.Fatal(got, want, err)
				}
				for _, ref := range got {
					if !want[ref] {
						t.Fatal("wrong generation retained", ref, want)
					}
				}
			}
			pin := func(epoch int, consumer string, retain bool) {
				t.Helper()
				trace = append(trace, fmt.Sprintf("pin(%d,%s,%t)", epoch, consumer, retain))
				if err := r.Pin(t.Context(), uint64(epoch), consumer, retain); err != nil {
					t.Fatal(err)
				}
				if retain {
					pins[epoch][consumer] = true
				} else {
					delete(pins[epoch], consumer)
				}
				check()
			}
			prune := func(lower time.Time) {
				t.Helper()
				trace = append(trace, "prune("+lower.Format(time.RFC3339Nano)+")")
				// Upper is already past the boundary. Only the trusted lower bound
				// can establish that a cut is old enough to withdraw.
				if err := r.Prune(t.Context(), s.R1Bounds{Lower: lower, Upper: lower.Add(time.Hour), Horizon: lower.Add(2 * time.Hour)}); err != nil {
					t.Fatal(err)
				}
				for i := range present {
					if len(pins[i]) == 0 && lower.After(deadline.Add(time.Duration(i)*time.Minute)) {
						delete(present, i)
					}
				}
				check()
			}
			check()
			prune(deadline.Add(-time.Nanosecond))
			prune(deadline)
			pin(0, "reader", true)
			pin(0, "rebuild", true)
			prune(deadline.Add(time.Nanosecond))
			pin(0, "reader", false)
			prune(deadline.Add(time.Nanosecond)) // the independent rebuild pin remains
			for step := 0; step < 64; step++ {
				i := rng.Intn(len(cuts))
				if present[i] && rng.Intn(3) != 0 {
					consumer := []string{"reader", "rebuild", "anchor-consumer"}[rng.Intn(3)]
					pin(i, consumer, rng.Intn(2) == 0)
				} else {
					prune(deadline.Add(time.Duration(step) * 10 * time.Second))
				}
			}
			for i := range cuts {
				if !present[i] {
					continue
				}
				for _, consumer := range []string{"reader", "rebuild", "anchor-consumer"} {
					pin(i, consumer, false)
				}
			}
			prune(deadline.Add(time.Hour))
			if len(present) != 0 {
				t.Fatal("all consumers released but support did not progress", present)
			}
			for _, cut := range cuts {
				if _, err := r.Checkpointer.Load(t.Context(), cut); err != nil {
					t.Fatal("prune removed immutable chain/anchor", err)
				}
			}
		})
	}
}
