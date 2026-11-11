package stats

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/KinasTomes/miles/internal/cost"
)

func measuredSummary(t *testing.T, phase, file string) Summary {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("../../benchmarks/evidence/engine-metadata-pilot/phases", phase, file))
	if err != nil {
		t.Fatal(err)
	}
	var v Summary
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if !v.PhysicalComplete || v.BytesStored == nil {
		t.Fatal("fixture cannot supply a complete cost input")
	}
	return v
}

func TestMeasuredDatasetRatiosAgreeWithCostDefinitions(t *testing.T) {
	var phases []Summary
	for _, phase := range []string{"empty", "owner-a", "owner-b", "owner-c"} {
		v := measuredSummary(t, phase, "global.json")
		input := cost.Snapshot{Logical: uint64(v.BytesLogical), UniqueRaw: uint64(v.BytesUniqueRaw), UniqueStored: uint64(v.BytesUniqueStored), Physical: uint64(*v.BytesStored)}
		if err := input.Validate(); err != nil {
			t.Fatal(err)
		}
		r := input.Ratios()
		for _, pair := range [][2]*float64{{v.DedupRatio, r.Dedup}, {v.CompressionRatio, r.Compression}, {v.TotalReduction, r.TotalReduction}} {
			if (pair[0] == nil) != (pair[1] == nil) {
				t.Fatal("null denominator contract drift", phase)
			}
			if pair[0] != nil && math.Abs(*pair[0]-*pair[1]) > 1e-12 {
				t.Fatal("stats/cost ratio definition drift", phase)
			}
		}
		phases = append(phases, v)
	}
	a, b, c := phases[1], phases[2], phases[3]
	if b.BytesLogical != 2*a.BytesLogical || b.BytesUniqueRaw != a.BytesUniqueRaw || b.BytesUniqueStored != a.BytesUniqueStored || b.Chunks != a.Chunks {
		t.Fatal("same-domain owner was not reused")
	}
	if c.BytesLogical != 3*a.BytesLogical || c.BytesUniqueRaw != 2*a.BytesUniqueRaw || c.BytesUniqueStored != 2*a.BytesUniqueStored || c.Chunks != 2*a.Chunks {
		t.Fatal("cross-domain owner did not add independent identities")
	}
}

func TestAllocatedBucketPhysicalIsNotAGlobalCostSnapshot(t *testing.T) {
	b := measuredSummary(t, "owner-b", "bucket-owner-a.json")
	// Bucket unique totals overlap, while allocated physical is split among
	// distinct referencing buckets. A global cost model must not mix those units.
	if *b.BytesStored >= b.BytesUniqueStored {
		t.Fatal("fixture does not demonstrate shared allocation")
	}
	wrong := cost.Snapshot{Logical: uint64(b.BytesLogical), UniqueRaw: uint64(b.BytesUniqueRaw), UniqueStored: uint64(b.BytesUniqueStored), Physical: uint64(*b.BytesStored)}
	if err := wrong.Validate(); err == nil {
		t.Fatal("allocated bucket bytes accepted as whole-system physical")
	}
}
