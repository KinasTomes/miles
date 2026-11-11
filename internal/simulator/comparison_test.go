package simulator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestComparePoliciesFourPoliciesDeterminismAndUnpricedOmission(t *testing.T) {
	trace, catalog := fixture(t, "cloud-boundary")
	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	if res.Baseline != PolicyDirectS3 {
		t.Fatalf("expected baseline %v, got %v", PolicyDirectS3, res.Baseline)
	}

	if len(res.Policies) != 4 {
		t.Fatalf("expected 4 policies, got %d", len(res.Policies))
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	for _, name := range []PolicyName{PolicyDirectS3, PolicyWarmStandardOnly, PolicyWarmGlacierIR, PolicyWarmGlacierFlex} {
		p, ok := policies[name]
		if !ok {
			t.Fatalf("missing policy %s", name)
		}
		if p.TotalCostUSD == "" {
			t.Fatalf("policy %s has empty total cost", name)
		}
	}

	warmStd := policies[PolicyWarmStandardOnly]
	// Finding 1 regression: cloud-boundary has unpriced items (transfer.internet_egress).
	// Savings must NOT be concluded from partial priced subtotals!
	if warmStd.SavingsUSD != nil || warmStd.SavingsPercent != nil {
		t.Fatalf("expected savings to be nil when unpriced items exist, got USD=%v, Pct=%v", warmStd.SavingsUSD, warmStd.SavingsPercent)
	}

	// Verify omission recorded
	hasOmission := false
	for _, om := range res.Omissions {
		if strings.Contains(om, "Net savings and savings percent vs baseline omitted") {
			hasOmission = true
			break
		}
	}
	if !hasOmission {
		t.Fatalf("expected unpriced omission in res.Omissions, got: %v", res.Omissions)
	}

	// Finding 2 regression: cloud-boundary has eligible decision at end of month,
	// but NO cold transition or cold inventory was observed.
	// Cold byte-time must NOT be fabricated (no 50% split).
	glacierIR := policies[PolicyWarmGlacierIR]
	for _, line := range glacierIR.Report.Lines {
		if line.RateID == "s3.storage_glacier_ir" || line.RateID == "s3.transition_ir" {
			t.Fatalf("unexpected cold rate in glacierIR policy when no transition observed: %+v", line)
		}
	}

	// Repeat for determinism
	res2, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if res.Policies[0].TotalCostUSD != res2.Policies[0].TotalCostUSD ||
		res.Policies[1].TotalCostUSD != res2.Policies[1].TotalCostUSD {
		t.Fatalf("ComparePolicies is non-deterministic")
	}
}

func TestComparePoliciesTimelineReplayMultiplePackTransitions(t *testing.T) {
	dayNS := int64(24 * time.Hour)

	baseTrace, catalog := fixture(t, "cloud-boundary")
	trace := baseTrace
	trace.Name = "multi-transition-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      2000 * 1024 * 1024,
				UniqueRaw:    1000 * 1024 * 1024,
				UniqueStored: 1000 * 1024 * 1024,
				Physical:     1000 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: 1000 * 1024 * 1024,
			},
		},
		{
			AtNS: 10 * dayNS,
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard:  600 * 1024 * 1024,
				storage.GlacierIR: 400 * 1024 * 1024,
			},
		},
		{
			AtNS: 20 * dayNS,
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard:  400 * 1024 * 1024,
				storage.GlacierIR: 600 * 1024 * 1024,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	// Verify WarmGlacierIR has both Standard and GlacierIR storage lines
	irPolicy := policies[PolicyWarmGlacierIR]
	var stdLine, irLine *cost.Line
	for i := range irPolicy.Report.Lines {
		line := &irPolicy.Report.Lines[i]
		if line.RateID == "s3.storage.standard" {
			stdLine = line
		}
		if line.RateID == "s3.storage.glacier_ir" {
			irLine = line
		}
	}

	if stdLine == nil || irLine == nil {
		t.Fatalf("expected both standard and glacier_ir storage lines, got: %+v", irPolicy.Report.Lines)
	}

	// Expected standard byte-time:
	// Interval 0..10 days (10 days): 1000 MB
	// Interval 10..20 days (10 days): 600 MB
	// Interval 20..30 days (10 days): 400 MB
	// Total standard MB-days = 1000*10 + 600*10 + 400*10 = 20,000 MB-days.
	//
	// Expected glacier IR byte-time:
	// Interval 0..10 days (10 days): 0 MB
	// Interval 10..20 days (10 days): 400 MB
	// Interval 20..30 days (10 days): 600 MB
	// Total glacier MB-days = 400*10 + 600*10 = 10,000 MB-days.
	//
	// In reduced fractions:
	// 20000 / 30720 = 125/192
	// 10000 / 30720 = 125/384
	// Ratio of standard to glacier IR byte-time is exactly 2.0!
	stdGBMo := stdLine.Quantity
	irGBMo := irLine.Quantity
	if stdGBMo != "125/192" || irGBMo != "125/384" {
		t.Fatalf("expected stdGBMo=125/192 and irGBMo=125/384, got std=%s, ir=%s", stdGBMo, irGBMo)
	}
}

func TestComparePoliciesPartialModelYieldsIndicativeDiffWithoutNetSavings(t *testing.T) {
	dayNS := int64(24 * time.Hour)

	baseTrace, catalog := fixture(t, "cloud-boundary")
	trace := baseTrace
	trace.Name = "partial-storage-put-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      2000 * 1024 * 1024,
				UniqueRaw:    1000 * 1024 * 1024,
				UniqueStored: 500 * 1024 * 1024,
				Physical:     500 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: 500 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	warmStd := policies[PolicyWarmStandardOnly]
	// Finding 1 regression: CompleteTotalUSD == nil because request/network/infra is incomplete.
	// Net savings MUST be nil; indicative storage difference MUST be reported!
	if warmStd.SavingsUSD != nil || warmStd.SavingsPercent != nil {
		t.Fatalf("expected nil net savings when CompleteTotalUSD is nil, got: USD=%v, Pct=%v", warmStd.SavingsUSD, warmStd.SavingsPercent)
	}
	if warmStd.IndicativeStorageDiffUSD == nil {
		t.Fatal("expected non-nil indicative storage difference")
	}
	if *warmStd.IndicativeStorageDiffUSD != "75/2048" {
		t.Fatalf("expected indicative storage difference 75/2048, got: %s", *warmStd.IndicativeStorageDiffUSD)
	}
}

func TestComparePoliciesEdgeRevivalNoSynthesizedTransition(t *testing.T) {
	// Finding 3 regression:
	// edge-revival has cold inventory at t=0 but no transition events in trace.
	// Simulator must NOT synthesize transition requests from initial cold inventory!
	trace, catalog := fixture(t, "edge-revival")
	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies edge revival failed: %v", err)
	}

	if len(res.Policies) != 4 {
		t.Fatalf("expected 4 policies, got %d", len(res.Policies))
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	flexPolicy := policies[PolicyWarmGlacierFlex]
	for _, line := range flexPolicy.Report.Lines {
		if line.RateID == "s3.transition_glacier" {
			t.Fatalf("unexpected synthesized s3.transition_glacier line in edge-revival: %+v", line)
		}
	}

	irPolicy := policies[PolicyWarmGlacierIR]
	for _, line := range irPolicy.Report.Lines {
		if line.RateID == "s3.transition_ir" {
			t.Fatalf("unexpected synthesized s3.transition_ir line in edge-revival: %+v", line)
		}
	}
}

func TestComparePoliciesStandardIAPreservedInWarmStandardOnly(t *testing.T) {
	// Finding 5 regression:
	// 1GiB STANDARD_IA inventory in billable_storage_bytes must not be dropped.
	// In WarmStandardOnly, all physical bytes (including STANDARD_IA) stay on Standard.
	dayNS := int64(24 * time.Hour)

	baseTrace, catalog := fixture(t, "cloud-boundary")
	trace := baseTrace
	trace.Name = "standard-ia-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1024 * 1024 * 1024,
				UniqueRaw:    1024 * 1024 * 1024,
				UniqueStored: 1024 * 1024 * 1024,
				Physical:     1024 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.StandardIA: 1024 * 1024 * 1024,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	warmStd := policies[PolicyWarmStandardOnly]
	foundStandardLine := false
	for _, line := range warmStd.Report.Lines {
		if line.RateID == "s3.storage.standard" {
			foundStandardLine = true
			if line.Quantity != "1" {
				t.Fatalf("expected 1 GB-Mo of s3.storage.standard from STANDARD_IA, got %s", line.Quantity)
			}
		}
	}
	if !foundStandardLine {
		t.Fatalf("missing s3.storage.standard line in warm_standard_only when inventory is STANDARD_IA: %+v", warmStd.Report.Lines)
	}
}
