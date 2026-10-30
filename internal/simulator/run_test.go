package simulator

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/policy"
	"github.com/KinasTomes/miles/internal/pricing"
)

func fixture(t *testing.T, name string) (Trace, *pricing.Catalog) {
	t.Helper()
	b, err := os.ReadFile("../../benchmarks/traces/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	trace, err := Read(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile("../../configs/pricing/pricing.ap-southeast-1.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := pricing.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return trace, c
}

func TestVirtualReplaySharedTieringBoundaryAndDeterminism(t *testing.T) {
	trace, c := fixture(t, "cloud-boundary")
	r, err := Run(context.Background(), trace, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Decisions) != 2 || r.Decisions[0].Reason != policy.PackTooYoung || r.Decisions[1].Verdict != policy.Eligible || r.Cost.Lines[2].RateID != "s3.storage.standard" {
		t.Fatalf("wrong shared decision or implicit transition: %+v", r)
	}
	if !r.End.Equal(trace.Start.Add(time.Duration(trace.DurationNS))) || r.Traffic[cost.GatewayClient] != 3<<30 || r.Traffic[cost.S3Gateway] != 0 || r.Cost.CompleteTotalUSD != nil {
		t.Fatal("clock, topology or coverage changed", r)
	}
	again, err := Run(context.Background(), trace, c)
	if err != nil || !reflect.DeepEqual(r, again) {
		t.Fatal("replay is not deterministic", err)
	}
	trace.Events[2].Tiering.SafeToTransition = policy.Unknown
	r, err = Run(context.Background(), trace, c)
	if err != nil || r.Decisions[1].Verdict != policy.Indeterminate {
		t.Fatal("missing evidence became eligible", r, err)
	}
}

func TestRevivalDoesNotDoubleChargeOrChangeDedupIdentity(t *testing.T) {
	trace, c := fixture(t, "edge-revival")
	r, err := Run(context.Background(), trace, c)
	if err != nil {
		t.Fatal(err)
	}
	if r.Counters.DupOccurrences != 3 || r.Counters.RevivedDistinct != 1 || *r.Ratios.Dedup != 3 || *r.Ratios.TotalReduction != 2 || len(r.Cost.Lines) != 3 {
		t.Fatal("occurrences, distinct identities or retained physical bytes mixed", r)
	}
	// One GiB archive for two days + half GiB Standard for one day + two PUTs.
	if r.Cost.Subtotal != "109/150000" {
		t.Fatal(r.Cost)
	}
	trace.Events[1].Counters = cost.WriteCounters{}
	withoutCounters, err := Run(context.Background(), trace, c)
	if err != nil || !reflect.DeepEqual(r.Cost, withoutCounters.Cost) {
		t.Fatal("revival was charged twice", err)
	}
}

func TestTraceRejectsInvalidInputsWithoutPartialReport(t *testing.T) {
	for name, change := range map[string]func(*Trace){
		"backend":  func(t *Trace) { t.Metadata = "ram" },
		"order":    func(t *Trace) { t.Events[1].AtNS = -1 },
		"duration": func(t *Trace) { t.DurationNS = 1 },
		"policy":   func(t *Trace) { t.Policy = json.RawMessage(`{}`) },
		"revision": func(t *Trace) { t.PolicyRevision = 0 },
		"region":   func(t *Trace) { t.Region = "us-east-1" },
		"route":    func(t *Trace) { t.Events[0].Traffic["unknown"] = 1 },
		"snapshot": func(t *Trace) { t.Events[0].Snapshot.UniqueRaw = 4 << 30 },
		"double storage": func(t *Trace) {
			t.Events[0].Usage = append(t.Events[0].Usage, cost.Usage{RateID: "s3.storage.standard", Unit: "GB-Mo", Quantity: "1"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			trace, c := fixture(t, "cloud-boundary")
			change(&trace)
			r, err := Run(context.Background(), trace, c)
			if err == nil || !reflect.DeepEqual(r, Result{}) {
				t.Fatal("invalid input returned a usable report", err)
			}
		})
	}
	trace, c := fixture(t, "cloud-boundary")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Run(ctx, trace, c); err == nil {
		t.Fatal("cancellation ignored")
	}
	for _, raw := range [][]byte{[]byte(`{"schema_version":1,"unknown":true}`), []byte(`{} {}`), bytes.Repeat([]byte(" "), MaxTraceBytes+1)} {
		if _, err := Read(bytes.NewReader(raw)); err == nil {
			t.Fatal("invalid/unbounded input accepted")
		}
	}
}
