package simulator

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
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

func TestComparePoliciesStorageRulesPaddingAndMetadataOverhead(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// 1. Glacier IR with 1024 bytes (< 128 KiB min billable bytes)
	traceIR := baseTrace
	traceIR.Name = "ir-padding-test"
	traceIR.DurationNS = 30 * dayNS
	traceIR.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1024,
				UniqueRaw:    1024,
				UniqueStored: 1024,
				Physical:     1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: 1024,
			},
		},
	}

	resIR, err := ComparePolicies(context.Background(), traceIR, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies IR failed: %v", err)
	}
	irPol := resIR.Policies[2] // WarmGlacierIR
	var irLine *cost.Line
	for i := range irPol.Report.Lines {
		if irPol.Report.Lines[i].RateID == "s3.storage.glacier_ir" {
			irLine = &irPol.Report.Lines[i]
			break
		}
	}
	if irLine == nil {
		t.Fatalf("missing s3.storage.glacier_ir line: %+v", irPol.Report.Lines)
	}
	// Padded to 131072 bytes (128 KiB) for 30 days -> 131072 / 1073741824 = 1/8192 GB-Mo
	if irLine.Quantity != "1/8192" {
		t.Fatalf("expected padded quantity 1/8192 GB-Mo, got %s", irLine.Quantity)
	}

	// 2. Glacier Flexible with 1 MiB -> adds 8 KiB Standard metadata + 32 KiB Glacier metadata
	traceFlex := baseTrace
	traceFlex.Name = "flex-metadata-test"
	traceFlex.DurationNS = 30 * dayNS
	traceFlex.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1048576,
				UniqueRaw:    1048576,
				UniqueStored: 1048576,
				Physical:     1048576,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: 1048576,
			},
		},
	}

	resFlex, err := ComparePolicies(context.Background(), traceFlex, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies Flex failed: %v", err)
	}
	flexPol := resFlex.Policies[3] // WarmGlacierFlex
	var flexStdLine, flexGlacierLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			flexStdLine = &flexPol.Report.Lines[i]
		}
		if flexPol.Report.Lines[i].RateID == "s3.storage.glacier" {
			flexGlacierLine = &flexPol.Report.Lines[i]
		}
	}
	if flexStdLine == nil || flexGlacierLine == nil {
		t.Fatalf("expected both standard metadata and glacier lines in Flex: %+v", flexPol.Report.Lines)
	}
	// 8192 bytes Standard metadata for 30 days -> 8192 / 1073741824 = 1/131072 GB-Mo
	if flexStdLine.Quantity != "1/131072" {
		t.Fatalf("expected standard metadata quantity 1/131072 GB-Mo, got %s", flexStdLine.Quantity)
	}
	// 1048576 + 32768 = 1081344 bytes Glacier storage for 30 days -> 1081344 / 1073741824 = 33/32768 GB-Mo
	if flexGlacierLine.Quantity != "33/32768" {
		t.Fatalf("expected glacier storage quantity 33/32768 GB-Mo, got %s", flexGlacierLine.Quantity)
	}
}

func TestComparePoliciesTemporaryRestoredStandard(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "temporary-restored-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1073741824,
				UniqueRaw:    1073741824,
				UniqueStored: 1073741824,
				Physical:     1073741824,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: 1073741824,
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	var stdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			stdLine = &flexPol.Report.Lines[i]
			break
		}
	}
	if stdLine == nil {
		t.Fatalf("expected standard storage line from restore and metadata: %+v", flexPol.Report.Lines)
	}
	// Standard includes 1 day of restored Standard copy (1/30 GB-Mo) + 8 KiB metadata (1/131072 GB-Mo)
	if stdLine.USD == "" {
		t.Fatal("empty standard storage USD")
	}
}

func TestComparePoliciesCompleteMatchedSavingsAndBreakEven(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// Set verified rate tiers for all required dimensions
	for i := range catalog.Rates {
		switch catalog.Rates[i].ID {
		case "transfer.internet_egress":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.09"}}
		case "ec2.compute":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.0416"}}
		case "ebs.gp3":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.08"}}
		}
	}

	// Build a complete matched trace with only verified prices (S3 standard, requests, network, and infrastructure: EC2, EBS, ALB, DDB)
	trace := baseTrace
	trace.Name = "complete-matched-model"
	trace.DurationNS = 30 * dayNS
	trace.Omissions = []string{"Full verified observation run across all topology dimensions"}
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
				{RateID: "transfer.internet_egress", Unit: "GB", Quantity: "2"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "10"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "10"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "10"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "10"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "10"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	warmStd := res.Policies[1]
	// Verify complete matched total is computed!
	if warmStd.Report.CompleteTotalUSD == nil {
		t.Fatal("expected CompleteTotalUSD to be non-nil for complete matched model")
	}
	if warmStd.SavingsUSD == nil {
		t.Fatal("expected SavingsUSD to be non-nil for complete matched model")
	}
	if warmStd.SavingsPercent == nil {
		t.Fatal("expected SavingsPercent to be non-nil for complete matched model")
	}
	if warmStd.Breakdown == nil {
		t.Fatal("expected non-nil breakdown")
	}
	if warmStd.Breakdown.StorageUSD == "" || warmStd.Breakdown.InfrastructureUSD == "" {
		t.Fatalf("expected populated breakdown fields, got %+v", warmStd.Breakdown)
	}
	if warmStd.BreakEven == nil {
		t.Fatal("expected non-nil BreakEven")
	}
}

func TestComparePoliciesRestoreAmplification(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "restore-amplification-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1073741824,
				UniqueRaw:    1073741824,
				UniqueStored: 1073741824,
				Physical:     1073741824,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: 1073741824,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 64 * 1024 * 1024, // 64 MiB delivered to client
			},
			Usage: []cost.Usage{
				// 1 GB retrieved from Glacier IR -> amplification = 1024 / 64 = 16.0
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2] // WarmGlacierIR
	if irPol.RestoreAmplification == nil {
		t.Fatal("expected non-nil RestoreAmplification")
	}
	if *irPol.RestoreAmplification != 16.0 {
		t.Fatalf("expected 16.0 amplification, got %f", *irPol.RestoreAmplification)
	}
}

func TestComparePoliciesCloudVsEdgeNetworkTopologyAndRetries(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// 1. Cloud Trace with retries
	cloudTrace := baseTrace
	cloudTrace.Name = "cloud-network-retries-test"
	cloudTrace.DurationNS = 30 * dayNS
	cloudTrace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      100 * 1024 * 1024,
				UniqueRaw:    100 * 1024 * 1024,
				UniqueStored: 50 * 1024 * 1024,
				Physical:     50 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: 50 * 1024 * 1024,
			},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 100 * 1024 * 1024,
				cost.GatewayClient: 100 * 1024 * 1024,
				cost.GatewayS3:     50 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				// 5 standard PUTs + 1 retry PUT = 6 PUT requests
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "6"},
			},
		},
	}

	resCloud, err := ComparePolicies(context.Background(), cloudTrace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies cloud failed: %v", err)
	}

	cloudWarm := resCloud.Policies[1]
	if cloudWarm.Breakdown == nil {
		t.Fatal("expected non-nil breakdown for cloud policy")
	}
	if cloudWarm.Breakdown.RequestsByType["s3_put"] != 6 {
		t.Fatalf("expected 6 s3_put requests (including retries), got %d", cloudWarm.Breakdown.RequestsByType["s3_put"])
	}
	if cloudWarm.Breakdown.TrafficBytes[string(cost.GatewayClient)] != 100*1024*1024 {
		t.Fatalf("expected 100MB gateway_to_client traffic, got %d", cloudWarm.Breakdown.TrafficBytes[string(cost.GatewayClient)])
	}
	if cloudWarm.Breakdown.TrafficBytes[string(cost.GatewayS3)] != 50*1024*1024 {
		t.Fatalf("expected 50MB gateway_to_s3_upload traffic, got %d", cloudWarm.Breakdown.TrafficBytes[string(cost.GatewayS3)])
	}

	// 2. Edge Trace with WAN download
	edgeTrace, _ := fixture(t, "edge-revival")
	resEdge, err := ComparePolicies(context.Background(), edgeTrace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies edge failed: %v", err)
	}
	edgeWarm := resEdge.Policies[1]
	if edgeWarm.Breakdown == nil {
		t.Fatal("expected non-nil breakdown for edge policy")
	}
	if edgeWarm.Breakdown.TrafficBytes[string(cost.ClientGateway)] != 3221225472 {
		t.Fatalf("expected 3221225472 client_to_gateway traffic in edge, got %d", edgeWarm.Breakdown.TrafficBytes[string(cost.ClientGateway)])
	}
}

func TestComparePoliciesIncompleteTelemetrySuppressesCompleteTotalAndSavings(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// 1. Trace with storage only (no requests, no network, no infrastructure)
	trace1 := baseTrace
	trace1.Name = "storage-only-trace"
	trace1.DurationNS = 30 * dayNS
	trace1.Omissions = []string{"Deterministic simulation note"} // No standard omission keywords
	trace1.Events = []Event{
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
		},
	}

	res1, err := ComparePolicies(context.Background(), trace1, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	for _, p := range res1.Policies {
		if p.Report.CompleteTotalUSD != nil {
			t.Fatalf("policy %s must have nil CompleteTotalUSD for storage-only trace", p.Name)
		}
		if p.SavingsUSD != nil {
			t.Fatalf("policy %s must have nil SavingsUSD for storage-only trace", p.Name)
		}
		if p.BreakEven != nil {
			t.Fatalf("policy %s must have nil BreakEven for storage-only trace", p.Name)
		}
	}

	// 2. Trace with storage and requests, but NO network
	trace2 := trace1
	trace2.Name = "missing-network-trace"
	trace2.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2000 * 1024 * 1024,
				Physical: 500 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: 500 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "10"},
			},
		},
	}

	res2, err := ComparePolicies(context.Background(), trace2, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if res2.Policies[1].Report.CompleteTotalUSD != nil || res2.Policies[1].SavingsUSD != nil {
		t.Fatal("expected nil CompleteTotalUSD and SavingsUSD when network telemetry is missing")
	}

	// 3. Trace with storage, requests, and network, but NO infrastructure for candidate policy
	trace3 := trace1
	trace3.Name = "missing-infra-trace"
	// Enable verified transfer.internet_egress
	for i := range catalog.Rates {
		if catalog.Rates[i].ID == "transfer.internet_egress" {
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.09"}}
		}
	}
	trace3.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2000 * 1024 * 1024,
				Physical: 500 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: 500 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "transfer.internet_egress", Unit: "GB", Quantity: "2"},
			},
		},
	}

	res3, err := ComparePolicies(context.Background(), trace3, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	// Direct S3 is complete (no infra required for baseline)
	if res3.Policies[0].Report.CompleteTotalUSD == nil {
		t.Fatal("expected baseline Direct S3 to be complete")
	}
	// Candidate policy warm_standard_only is missing infrastructure
	if res3.Policies[1].Report.CompleteTotalUSD != nil {
		t.Fatal("expected warm_standard_only CompleteTotalUSD to be nil without infrastructure telemetry")
	}
	if res3.Policies[1].SavingsUSD != nil {
		t.Fatal("expected warm_standard_only SavingsUSD to be nil when candidate infra is missing")
	}
	if res3.Policies[1].BreakEven != nil {
		t.Fatal("expected warm_standard_only BreakEven to be nil when candidate infra is missing")
	}

	// 4. Finding 1 regression: trace with alb.hour only (missing EC2, EBS, DDB) must NOT return CompleteTotalUSD or savings
	trace4 := trace1
	trace4.Name = "partial-cloud-infra-trace"
	trace4.Omissions = []string{"Full observation run"} // Neutral omissions without omission keywords
	trace4.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2000 * 1024 * 1024,
				Physical: 500 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: 500 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "1"},
				{RateID: "transfer.internet_egress", Unit: "GB", Quantity: "1"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "1"},
			},
		},
	}

	res4, err := ComparePolicies(context.Background(), trace4, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	warmStd4 := res4.Policies[1]
	if warmStd4.Report.CompleteTotalUSD != nil {
		t.Fatalf("expected nil CompleteTotalUSD for partial cloud infra trace (missing EC2/EBS/DDB), got %v", *warmStd4.Report.CompleteTotalUSD)
	}
	if warmStd4.SavingsUSD != nil {
		t.Fatalf("expected nil SavingsUSD for partial cloud infra trace, got %v", *warmStd4.SavingsUSD)
	}
	if warmStd4.BreakEven != nil {
		t.Fatalf("expected nil BreakEven for partial cloud infra trace, got %v", *warmStd4.BreakEven)
	}

	// 5. Finding 1 regression: trace with storage, PUT, and full cloud infra (EC2, EBS, ALB, DDB)
	// but COMPLETELY lacking network metering (no Traffic map, no transfer.* usage).
	// Must return nil CompleteTotalUSD, SavingsUSD, and BreakEven (fail-closed without network telemetry).
	for i := range catalog.Rates {
		switch catalog.Rates[i].ID {
		case "ec2.compute":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.0416"}}
		case "ebs.gp3":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.08"}}
		}
	}

	trace5 := trace1
	trace5.Name = "missing-network-telemetry-trace"
	trace5.Omissions = []string{"Full observation run"}
	trace5.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2000 * 1024 * 1024,
				Physical: 500 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: 500 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "10"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "10"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "10"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "10"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "10"},
			},
		},
	}

	res5, err := ComparePolicies(context.Background(), trace5, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	warmStd5 := res5.Policies[1]
	if warmStd5.Report.CompleteTotalUSD != nil {
		t.Fatalf("expected nil CompleteTotalUSD for trace missing network telemetry, got %v", *warmStd5.Report.CompleteTotalUSD)
	}
	if warmStd5.SavingsUSD != nil {
		t.Fatalf("expected nil SavingsUSD for trace missing network telemetry, got %v", *warmStd5.SavingsUSD)
	}
	if warmStd5.BreakEven != nil {
		t.Fatalf("expected nil BreakEven for trace missing network telemetry, got %v", *warmStd5.BreakEven)
	}

	// 6. Finding 1 regression (Counterexample 1): trace has network.nat_hour (hourly NAT infrastructure),
	// full cloud infra, storage and requests, but NO Traffic map and NO payload transfer (transfer.*).
	// NAT infrastructure rate must NOT be conflated with payload transfer -> must return nil CompleteTotalUSD.
	for i := range catalog.Rates {
		if catalog.Rates[i].ID == "network.nat_hour" {
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.059"}}
		}
	}
	traceNAT := trace5
	traceNAT.Name = "nat-hour-only-trace"
	traceNAT.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     trace5.Events[0].Snapshot,
			StorageBytes: trace5.Events[0].StorageBytes,
			Usage: append(trace5.Events[0].Usage, cost.Usage{
				RateID: "network.nat_hour", Unit: "Hrs", Quantity: "10",
			}),
		},
	}
	resNAT, err := ComparePolicies(context.Background(), traceNAT, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	warmStdNAT := resNAT.Policies[1]
	if warmStdNAT.Report.CompleteTotalUSD != nil {
		t.Fatalf("expected nil CompleteTotalUSD for NAT-hour only trace, got %v", *warmStdNAT.Report.CompleteTotalUSD)
	}
	if warmStdNAT.SavingsUSD != nil {
		t.Fatalf("expected nil SavingsUSD for NAT-hour only trace, got %v", *warmStdNAT.SavingsUSD)
	}

	// 7. Finding 1 regression (Counterexample 2): trace has Traffic = {client_to_gateway: 0}
	// but lacks observations for egress and S3 routes, and has no transfer.* usage.
	// Zero on one route does not prove full network coverage -> must return nil CompleteTotalUSD.
	traceSingleRoute := trace5
	traceSingleRoute.Name = "single-route-zero-traffic-trace"
	traceSingleRoute.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     trace5.Events[0].Snapshot,
			StorageBytes: trace5.Events[0].StorageBytes,
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 0,
			},
			Usage: trace5.Events[0].Usage,
		},
	}
	resSingle, err := ComparePolicies(context.Background(), traceSingleRoute, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	warmStdSingle := resSingle.Policies[1]
	if warmStdSingle.Report.CompleteTotalUSD != nil {
		t.Fatalf("expected nil CompleteTotalUSD for single-route-zero trace, got %v", *warmStdSingle.Report.CompleteTotalUSD)
	}
	if warmStdSingle.SavingsUSD != nil {
		t.Fatalf("expected nil SavingsUSD for single-route-zero trace, got %v", *warmStdSingle.SavingsUSD)
	}

	// 8. Explicit zero traffic with complete coverage: trace explicitly measures ALL required routes
	// (client_to_gateway, gateway_to_client, gateway_to_s3_upload, s3_to_gateway_download) as 0 bytes.
	// Since all data path routes were explicitly metered and observed to be 0, network coverage is complete!
	traceCompleteZero := trace5
	traceCompleteZero.Name = "explicit-zero-traffic-trace"
	traceCompleteZero.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     trace5.Events[0].Snapshot,
			StorageBytes: trace5.Events[0].StorageBytes,
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 0,
				cost.GatewayClient: 0,
				cost.GatewayS3:     0,
				cost.S3Gateway:     0,
			},
			Usage: trace5.Events[0].Usage,
		},
	}

	resCompleteZero, err := ComparePolicies(context.Background(), traceCompleteZero, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	warmStdCompleteZero := resCompleteZero.Policies[1]
	if warmStdCompleteZero.Report.CompleteTotalUSD == nil {
		t.Fatal("expected non-nil CompleteTotalUSD for explicit zero traffic with complete coverage across all routes")
	}
	if warmStdCompleteZero.SavingsUSD == nil {
		t.Fatal("expected non-nil SavingsUSD for explicit zero traffic with complete coverage across all routes")
	}
	if warmStdCompleteZero.BreakEven == nil {
		t.Fatal("expected non-nil BreakEven for explicit zero traffic with complete coverage across all routes")
	}
}

func TestComparePoliciesRestoreCappedToRemainingTraceDuration(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "restore-time-cap-test"
	trace.DurationNS = 30 * dayNS
	// Restore happens at 30 days minus 2 hours (only 2 hours remaining in simulation period)
	restoreAtNS := trace.DurationNS - int64(2*time.Hour)
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: 1073741824,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: 1073741824,
			},
		},
		{
			AtNS: restoreAtNS,
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3] // WarmGlacierFlex
	// Find temporary restored Standard line
	var stdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			stdLine = &flexPol.Report.Lines[i]
			break
		}
	}
	if stdLine == nil {
		t.Fatal("expected s3.storage.standard line for temporary restored Standard copy")
	}

	// Expected usage: 1 GB for 2 hours in a 30-day (720 hour) month = 2/720 GB-Mo = 1/360 GB-Mo
	expectedRat := new(big.Rat).SetFrac64(2, 720)
	actualRat, ok := new(big.Rat).SetString(stdLine.Quantity)
	if !ok {
		t.Fatalf("invalid quantity rational %q", stdLine.Quantity)
	}
	// stdLine combines 8 KiB Glacier metadata Standard copy (8192/1073741824) with 2h restore copy
	metadataRat := new(big.Rat).SetFrac64(8192, 1073741824)
	restoreRat := new(big.Rat).Sub(actualRat, metadataRat)
	if restoreRat.Cmp(expectedRat) != 0 {
		t.Fatalf("expected %s GB-Mo for 2 hours restore copy, got %s (actual total: %s)", expectedRat.RatString(), restoreRat.RatString(), actualRat.RatString())
	}
}

func TestComparePoliciesEarlyDeletionAccrued(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "early-deletion-test"
	trace.DurationNS = 30 * dayNS
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trace.Start = startTime

	packBytes := int64(100 * 1024 * 1024) // 100 MiB
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: uint64(packBytes),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(packBytes),
			},
		},
		{
			AtNS: 10 * dayNS, // Observed as Deleting at day 10 (GLACIER_IR has 90-day minimum retention)
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "same-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              packBytes,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: packBytes,
			},
		},
		{
			AtNS: 10 * dayNS, // Second observation as Retiring for the same pack identity and generation
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "same-pack", Generation: "g1"},
					State:             "Retiring",
					Size:              packBytes,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: packBytes,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2] // WarmGlacierIR
	if irPol.Breakdown == nil {
		t.Fatal("expected breakdown")
	}
	if irPol.Breakdown.EarlyDeletionUSD == "0" || irPol.Breakdown.EarlyDeletionUSD == "" {
		t.Fatalf("expected early deletion fees to be accrued, got %q", irPol.Breakdown.EarlyDeletionUSD)
	}

	// Verify deduplication: single charge for 100 MiB deleted at day 10 (80 days unexpired out of 90)
	minRetention := 90 * 24 * time.Hour
	monthDuration := 30 * 24 * time.Hour
	uSingle, err := cost.EarlyDeletionUsage(storage.GlacierIR, uint64(packBytes), 10*24*time.Hour, minRetention, trace.BytesPerGB, monthDuration)
	if err != nil {
		t.Fatal(err)
	}
	singleReport, err := cost.Evaluate(catalog, []cost.Usage{uSingle})
	if err != nil {
		t.Fatal(err)
	}

	if irPol.Breakdown.EarlyDeletionUSD != singleReport.Subtotal {
		t.Fatalf("expected single early deletion charge (%s), got %s (did it charge duplicate lifecycle observations?)", singleReport.Subtotal, irPol.Breakdown.EarlyDeletionUSD)
	}

	// Finding 2 regression: Two distinct packs with identical size without pack Ref (ID == "")
	// must NOT be collapsed together by size. Each pack deleted before minimum retention must be charged.
	traceTwoPacks := baseTrace
	traceTwoPacks.Name = "two-packs-without-ref"
	traceTwoPacks.DurationNS = 30 * dayNS
	traceTwoPacks.Start = startTime
	traceTwoPacks.BytesPerGB = 1024 * 1024 * 1024

	oneGiB := int64(1024 * 1024 * 1024)
	traceTwoPacks.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  uint64(2 * oneGiB),
				Physical: uint64(2 * oneGiB),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(2 * oneGiB),
			},
		},
		{
			AtNS: 10 * dayNS, // Pack 1 deleted at day 10
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "pack-1", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 10 * dayNS, // Pack 2 deleted at day 10 - distinct pack identity
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "pack-2", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}

	resTwo, err := ComparePolicies(context.Background(), traceTwoPacks, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	irTwo := resTwo.Policies[2] // WarmGlacierIR
	// Rate is $0.005/GB-Mo. 1 GiB for 80 unexpired days = 1/75 USD per pack.
	// Two packs = 2/75 USD.
	if irTwo.Breakdown.EarlyDeletionUSD != "2/75" {
		t.Fatalf("expected early deletion fee '2/75' for two 1GiB packs, got %q", irTwo.Breakdown.EarlyDeletionUSD)
	}

	// Two observations (Deleting + Retiring) of the same pack must be charged exactly once (idempotent)
	traceDupObservation := traceTwoPacks
	traceDupObservation.Name = "dup-observation-early-deletion"
	traceDupObservation.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: uint64(oneGiB), Physical: uint64(oneGiB)},
			StorageBytes: map[storage.StorageClass]uint64{storage.GlacierIR: uint64(oneGiB)},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "single-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "single-pack", Generation: "g1"},
					State:             "Retiring",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}
	resDup, err := ComparePolicies(context.Background(), traceDupObservation, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resDup.Policies[2].Breakdown.EarlyDeletionUSD != "1/75" {
		t.Fatalf("expected single charge '1/75' for duplicate observations of same pack, got %q", resDup.Policies[2].Breakdown.EarlyDeletionUSD)
	}

	// Pack without Ref.ID cannot prove identity; must not be charged early deletion
	traceNoRef := traceTwoPacks
	traceNoRef.Name = "no-ref-early-deletion"
	traceNoRef.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: uint64(oneGiB), Physical: uint64(oneGiB)},
			StorageBytes: map[storage.StorageClass]uint64{storage.GlacierIR: uint64(oneGiB)},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}
	resNoRef, err := ComparePolicies(context.Background(), traceNoRef, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resNoRef.Policies[2].Breakdown.EarlyDeletionUSD != "0" {
		t.Fatalf("expected 0 early deletion for pack without Ref identity, got %q", resNoRef.Policies[2].Breakdown.EarlyDeletionUSD)
	}

	// Finding 2 regression: A pack with the same Ref (ID:generation) changing storage class
	// (GLACIER_IR:Retiring on day 10, then GLACIER:Deleting on day 11)
	// must NOT have its Glacier Flexible fee swallowed due to a global ID:generation key.
	// Both retention obligations must be charged to their respective policies.
	traceClassChange := baseTrace
	traceClassChange.Name = "class-change-early-deletion"
	traceClassChange.DurationNS = 30 * dayNS
	traceClassChange.Start = startTime
	traceClassChange.BytesPerGB = 1024 * 1024 * 1024

	traceClassChange.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  uint64(oneGiB),
				Physical: uint64(oneGiB),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(oneGiB),
			},
		},
		{
			AtNS: 10 * dayNS, // Pack retiring from Glacier IR at day 10
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "shared-pack", Generation: "g1"},
					State:             "Retiring",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 11 * dayNS, // Same pack deleting from Glacier Flexible at day 11
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "shared-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.Glacier,
					MinRetentionUntil: startTime.Add(10 * 24 * time.Hour).Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}

	resChange, err := ComparePolicies(context.Background(), traceClassChange, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	irChange := resChange.Policies[2]   // WarmGlacierIR
	flexChange := resChange.Policies[3] // WarmGlacierFlex

	if irChange.Breakdown.EarlyDeletionUSD == "0" || irChange.Breakdown.EarlyDeletionUSD == "" {
		t.Fatalf("expected Glacier IR early deletion fee to be charged, got %q", irChange.Breakdown.EarlyDeletionUSD)
	}
	if flexChange.Breakdown.EarlyDeletionUSD == "0" || flexChange.Breakdown.EarlyDeletionUSD == "" {
		t.Fatalf("expected Glacier Flexible early deletion fee to be charged (did global pack identity swallow it?), got %q", flexChange.Breakdown.EarlyDeletionUSD)
	}
}

func TestComparePoliciesGlacierPerPackRules(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// 2 packs of 1024 bytes tiered to cold storage
	trace := baseTrace
	trace.Name = "two-cold-packs"
	trace.DurationNS = 30 * dayNS
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trace.Start = startTime

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2048,
				Physical: 2048,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: 2048,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-1", Generation: "g1"},
					Size:  1024,
					Class: storage.GlacierIR,
					State: "Cold",
				},
				LiveBytes: 1024,
			},
		},
		{
			AtNS: 0,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-2", Generation: "g1"},
					Size:  1024,
					Class: storage.GlacierIR,
					State: "Cold",
				},
				LiveBytes: 1024,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2] // WarmGlacierIR
	var irLine *cost.Line
	for i := range irPol.Report.Lines {
		if irPol.Report.Lines[i].RateID == "s3.storage.glacier_ir" {
			irLine = &irPol.Report.Lines[i]
			break
		}
	}
	if irLine == nil {
		t.Fatalf("missing s3.storage.glacier_ir line: %+v", irPol.Report.Lines)
	}

	// 2 packs × 128 KiB (131072) = 256 KiB (262144 bytes).
	// 262144 / 1073741824 = 1/4096 GB-Mo (NOT 1/8192!).
	if irLine.Quantity != "1/4096" {
		t.Fatalf("expected 2 packs padded to 256 KiB (1/4096 GB-Mo), got %s", irLine.Quantity)
	}

	flexPol := res.Policies[3] // WarmGlacierFlex
	var flexGlacierLine, flexStdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.glacier" {
			flexGlacierLine = &flexPol.Report.Lines[i]
		}
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			flexStdLine = &flexPol.Report.Lines[i]
		}
	}
	if flexGlacierLine == nil || flexStdLine == nil {
		t.Fatalf("missing flex lines: %+v", flexPol.Report.Lines)
	}

	// 2 packs × 32 KiB archive metadata = 64 KiB = 65536 bytes.
	// Total glacier = 2048 + 65536 = 67584 bytes -> 67584 / 1073741824 = 33/524288 GB-Mo.
	if flexGlacierLine.Quantity != "33/524288" {
		t.Fatalf("expected 2 packs glacier metadata quantity 33/524288, got %s", flexGlacierLine.Quantity)
	}
	// 2 packs × 8 KiB standard metadata = 16 KiB = 16384 bytes.
	// 16384 / 1073741824 = 1/65536 GB-Mo.
	if flexStdLine.Quantity != "1/65536" {
		t.Fatalf("expected 2 packs standard metadata quantity 1/65536, got %s", flexStdLine.Quantity)
	}
}

func TestComparePoliciesEarlyDeletionExpiredRetentionNoRecharge(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "expired-retention-test"
	trace.DurationNS = 100 * dayNS
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trace.Start = startTime
	packBytes := int64(100 * 1024 * 1024)

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: uint64(packBytes),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(packBytes),
			},
		},
		{
			AtNS: 95 * dayNS, // Deleted at day 95, where MinRetentionUntil was day 90 (expired!)
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "expired-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              packBytes,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour), // Retention expired 5 days ago!
					LastAccess:        startTime.Add(94 * 24 * time.Hour), // LastAccess was day 94
				},
				LiveBytes: packBytes,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2] // WarmGlacierIR
	if irPol.Breakdown == nil {
		t.Fatal("expected breakdown")
	}
	if irPol.Breakdown.EarlyDeletionUSD != "0" {
		t.Fatalf("expected EarlyDeletionUSD to be '0' for pack deleted after expired retention, got %q (did it recharge from LastAccess?)", irPol.Breakdown.EarlyDeletionUSD)
	}

	// Finding 3 regression: Deadline set further than minimum retention (e.g. 105 days for 90-day rule)
	// must be clamped to at most 90 days penalty; retained cannot be negative and fee cannot exceed 90 days.
	oneGiB := int64(1024 * 1024 * 1024)
	traceClamp := baseTrace
	traceClamp.Name = "clamp-retention-test"
	traceClamp.DurationNS = 30 * dayNS
	traceClamp.Start = startTime
	traceClamp.BytesPerGB = 1024 * 1024 * 1024
	traceClamp.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: uint64(oneGiB), Physical: uint64(oneGiB)},
			StorageBytes: map[storage.StorageClass]uint64{storage.GlacierIR: uint64(oneGiB)},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "clamp-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(120 * 24 * time.Hour), // 120 days > 90 days!
				},
				LiveBytes: oneGiB,
			},
		},
	}
	resClamp, err := ComparePolicies(context.Background(), traceClamp, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	// Maximum penalty for 1 GiB on Glacier IR (90 days = 3 months) at $0.005/GB-Mo is 3 * 0.005 = 3/200 USD.
	if resClamp.Policies[2].Breakdown.EarlyDeletionUSD != "3/200" {
		t.Fatalf("expected early deletion fee clamped to maximum 90 days '3/200', got %q", resClamp.Policies[2].Breakdown.EarlyDeletionUSD)
	}
}

func TestComparePoliciesRestoreAmplificationPolicyIsolation(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// 1. Trace with IR retrieval only: Flex amplification must be nil
	traceIR := baseTrace
	traceIR.Name = "ir-amplification-only"
	traceIR.DurationNS = 30 * dayNS
	traceIR.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: 1073741824,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 64 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	resIR, err := ComparePolicies(context.Background(), traceIR, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	irPol := resIR.Policies[2]
	flexPol := resIR.Policies[3]
	if irPol.RestoreAmplification == nil || *irPol.RestoreAmplification != 16.0 {
		t.Fatalf("expected IR amplification 16.0, got %v", irPol.RestoreAmplification)
	}
	if flexPol.RestoreAmplification != nil {
		t.Fatalf("expected Flex amplification to be nil when only IR retrieval was present, got %v", *flexPol.RestoreAmplification)
	}

	// Finding 5 regression: Denominator must not be diluted by warm reads in other events.
	// 1 GiB cold retrieval with 64 MiB delivered in event 1 + 1 GiB warm read in event 2.
	// Amplification must remain 1024 / 64 = 16.0, NOT diluted by 1 GiB warm read (0.94117647).
	traceWithWarmRead := traceIR
	traceWithWarmRead.Name = "ir-warm-read-not-diluted"
	traceWithWarmRead.Events = append(traceWithWarmRead.Events, Event{
		AtNS: 10 * dayNS,
		Traffic: map[cost.Route]uint64{
			cost.GatewayClient: 1073741824, // 1 GiB warm read delivered to client
		},
		Usage: []cost.Usage{
			{RateID: "s3.get_standard", Unit: "Requests", Quantity: "100"},
		},
	})
	resWarm, err := ComparePolicies(context.Background(), traceWithWarmRead, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resWarm.Policies[2].RestoreAmplification == nil || *resWarm.Policies[2].RestoreAmplification != 16.0 {
		t.Fatalf("expected IR amplification to stay 16.0 without warm-read dilution, got %v", resWarm.Policies[2].RestoreAmplification)
	}

	// 2. Denominator: GatewayClient traffic == 0 must return nil (not fallback to logical)
	traceNoClient := traceIR
	traceNoClient.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: 1073741824,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 0, // 0 delivered bytes
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	resNoClient, err := ComparePolicies(context.Background(), traceNoClient, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resNoClient.Policies[2].RestoreAmplification != nil {
		t.Fatalf("expected nil amplification when GatewayClient traffic is 0, got %v", *resNoClient.Policies[2].RestoreAmplification)
	}
}

func TestComparePoliciesStalePhysicalClassMapInconsistency(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	oneGiB := uint64(1024 * 1024 * 1024)
	trace := baseTrace
	trace.Name = "stale-inventory-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: oneGiB,
			},
		},
		{
			AtNS: 15 * dayNS,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: 2 * oneGiB, // Physical increased 1 -> 2 GiB
			},
			// StorageBytes omitted or still 1 GiB -> sumStorageBytes != currentPhysical!
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	warmStd := res.Policies[1]
	if warmStd.PhysicalBytes != 2*oneGiB {
		t.Fatalf("expected summary PhysicalBytes to be updated to 2 GiB (%d), got %d", 2*oneGiB, warmStd.PhysicalBytes)
	}
	if warmStd.StorageBytesByClass["STANDARD"] != 2*oneGiB {
		t.Fatalf("expected warmStd class map STANDARD to be 2 GiB, got %d", warmStd.StorageBytesByClass["STANDARD"])
	}
	if warmStd.Report.CompleteTotalUSD != nil {
		t.Fatalf("expected CompleteTotalUSD to be nil on inconsistent inventory, got %v", *warmStd.Report.CompleteTotalUSD)
	}
	if warmStd.SavingsUSD != nil {
		t.Fatalf("expected SavingsUSD to be nil on inconsistent inventory, got %v", *warmStd.SavingsUSD)
	}

	foundInconsistentOmission := false
	for _, om := range res.Omissions {
		if strings.Contains(om, "Storage inventory inconsistent") {
			foundInconsistentOmission = true
			break
		}
	}
	if !foundInconsistentOmission {
		t.Fatalf("expected storage inventory inconsistent omission, got: %v", res.Omissions)
	}

	// Finding 2 regression: Inconsistency at exact trace.DurationNS (end-of-trace)
	// must NOT allow CompleteTotalUSD or SavingsUSD to be published.
	traceEndInconsistent := baseTrace
	traceEndInconsistent.Name = "end-of-trace-inconsistent"
	traceEndInconsistent.DurationNS = 30 * dayNS
	traceEndInconsistent.Omissions = []string{"Full verified observation"}
	for i := range catalog.Rates {
		switch catalog.Rates[i].ID {
		case "transfer.internet_egress":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.09"}}
		case "ec2.compute":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.0416"}}
		case "ebs.gp3":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.08"}}
		}
	}
	traceEndInconsistent.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: oneGiB, Physical: oneGiB},
			StorageBytes: map[storage.StorageClass]uint64{storage.Standard: oneGiB},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 0, cost.GatewayClient: 0, cost.GatewayS3: 0, cost.S3Gateway: 0,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "720"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "20"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "720"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "720"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "10"},
			},
		},
		{
			AtNS:         30 * dayNS,                                            // At the very end of trace
			Snapshot:     &cost.Snapshot{Logical: oneGiB, Physical: 2 * oneGiB}, // Inconsistent
			StorageBytes: map[storage.StorageClass]uint64{storage.Standard: oneGiB},
		},
	}
	resEnd, err := ComparePolicies(context.Background(), traceEndInconsistent, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resEnd.Policies[1].Report.CompleteTotalUSD != nil {
		t.Fatalf("expected CompleteTotalUSD to be nil on end-of-trace inconsistent inventory, got %v", *resEnd.Policies[1].Report.CompleteTotalUSD)
	}
	if resEnd.Policies[1].SavingsUSD != nil {
		t.Fatalf("expected SavingsUSD to be nil on end-of-trace inconsistent inventory, got %v", *resEnd.Policies[1].SavingsUSD)
	}
}

func TestComparePoliciesDirectS3ExcludesGatewayTraffic(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	oneGiB := uint64(1024 * 1024 * 1024)
	trace := baseTrace
	trace.Name = "direct-traffic-isolation"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: oneGiB,
				cost.GatewayClient: oneGiB,
				cost.GatewayS3:     oneGiB,
				cost.S3Gateway:     500,
				cost.InterNode:     100,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	directPol := res.Policies[0]
	if directPol.Breakdown == nil {
		t.Fatal("expected breakdown")
	}
	tb := directPol.Breakdown.TrafficBytes
	if tb["gateway_to_s3_upload"] != 0 {
		t.Fatalf("direct_s3 must not have gateway_to_s3_upload, got %d", tb["gateway_to_s3_upload"])
	}
	if tb["s3_to_gateway_download"] != 0 {
		t.Fatalf("direct_s3 must not have s3_to_gateway_download, got %d", tb["s3_to_gateway_download"])
	}
	if tb["inter_node"] != 0 {
		t.Fatalf("direct_s3 must not have inter_node, got %d", tb["inter_node"])
	}
	if tb["client_to_s3"] != oneGiB {
		t.Fatalf("expected client_to_s3 = 1 GiB, got %d", tb["client_to_s3"])
	}
	if tb["s3_to_client"] != oneGiB {
		t.Fatalf("expected s3_to_client = 1 GiB, got %d", tb["s3_to_client"])
	}
}

func TestComparePoliciesBreakEvenTimeWeightedStorage(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// Set verified rate tiers for all required dimensions
	for i := range catalog.Rates {
		switch catalog.Rates[i].ID {
		case "transfer.internet_egress":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.09"}}
		case "ec2.compute":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.0416"}}
		case "ebs.gp3":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.08"}}
		}
	}

	oneGiB := uint64(1024 * 1024 * 1024)
	trace := baseTrace
	trace.Name = "breakeven-time-weighted"
	trace.DurationNS = 30 * dayNS
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trace.Start = startTime
	trace.Omissions = []string{"Full verified observation"}

	// Logical storage 2 GiB for first 15 days, 4 GiB for second 15 days -> time-weighted 3 GB-Mo
	// Physical storage is 1 GiB throughout -> savings = 2 GB-Mo Standard storage.
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2 * oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: oneGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 0,
				cost.GatewayClient: 0,
				cost.GatewayS3:     0,
				cost.S3Gateway:     0,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "720"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "20"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "720"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "720"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "100"},
			},
		},
		{
			AtNS: 15 * dayNS,
			Snapshot: &cost.Snapshot{
				Logical:  4 * oneGiB, // Final snapshot is 4 GiB
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: oneGiB,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	warmStd := res.Policies[1]
	if warmStd.BreakEven == nil {
		t.Fatal("expected non-nil BreakEven")
	}

	// Verify break-even basis:
	// Direct S3 storage: 2 GiB * 15/30 + 4 GiB * 15/30 = 3 GB-Mo.
	// WarmStd storage: 1 GiB * 30/30 = 1 GB-Mo.
	// Storage saving: 2 GB-Mo * $0.025/GB = $0.05 (1/20 USD).
	// Logical GB basis must be 3 GB-Mo (time-weighted), NOT 4 GB (final snapshot).
	// unitSaving = 0.05 / 3 = 1/60 USD/GB.
	// Fixed infrastructure must exclude variable usage (alb.lcu and ddb.write):
	// Fixed infra in trace = 720h EC2 (29.952) + 20 GB-Mo EBS (1.6) + 720h ALB (18.144) + 1 GB-Mo DDB (0.285) = 49.981 USD/month.
	// BreakEven = 49.981 / (1/60) = 2998.86 GB = 149943/50 GB.
	beStr := *warmStd.BreakEven
	if beStr != "149943/50" {
		t.Fatalf("expected break-even to be '149943/50' GB (isolating fixed infra from variable LCU/writes), got %s", beStr)
	}

	// Finding 1 regression: Break-even threshold must be independent of trace duration!
	// A 2-day trace with the same provisioned resources and storage profile must produce the EXACT SAME break-even threshold (149943/50),
	// rather than a 15-fold difference.
	trace2Days := trace
	trace2Days.Name = "breakeven-2-days"
	trace2Days.DurationNS = 2 * dayNS
	trace2Days.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: 2 * oneGiB, Physical: oneGiB},
			StorageBytes: map[storage.StorageClass]uint64{storage.Standard: oneGiB},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 0, cost.GatewayClient: 0, cost.GatewayS3: 0, cost.S3Gateway: 0,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "48"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "4/3"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "48"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "48"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1/15"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "10"},
			},
		},
		{
			AtNS:         1 * dayNS,
			Snapshot:     &cost.Snapshot{Logical: 4 * oneGiB, Physical: oneGiB},
			StorageBytes: map[storage.StorageClass]uint64{storage.Standard: oneGiB},
		},
	}
	res2Days, err := ComparePolicies(context.Background(), trace2Days, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies 2 days failed: %v", err)
	}
	if res2Days.Policies[1].BreakEven == nil {
		t.Fatal("expected non-nil BreakEven for 2 days trace")
	}
	be2Days := *res2Days.Policies[1].BreakEven
	if be2Days != "149943/50" {
		t.Fatalf("expected identical break-even threshold for 2-day trace '149943/50', got %q (break-even must not vary with trace duration!)", be2Days)
	}
}

func TestComparePoliciesStandardPackDeletionWithoutRefDoesNotSuppressGlacierCompleteness(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// Set verified rate tiers for all required dimensions
	for i := range catalog.Rates {
		switch catalog.Rates[i].ID {
		case "transfer.internet_egress":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.09"}}
		case "ec2.compute":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.0416"}}
		case "ebs.gp3":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.08"}}
		case "s3.storage.glacier_ir":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.005"}}
		case "s3.storage.glacier":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.004"}}
		}
	}

	oneGiB := uint64(1024 * 1024 * 1024)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	trace := baseTrace
	trace.Name = "std-pack-deletion-without-ref"
	trace.DurationNS = 30 * dayNS
	trace.Start = startTime
	trace.Omissions = []string{"Full verified observation"}

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2 * oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard:  512 * 1024 * 1024,
				storage.GlacierIR: 512 * 1024 * 1024,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "ir-pack-1", Generation: "g1"},
					State:             "Cold",
					Size:              int64(512 * 1024 * 1024),
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: int64(512 * 1024 * 1024),
			},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 0, cost.GatewayClient: 0, cost.GatewayS3: 0, cost.S3Gateway: 0,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "720"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "20"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "720"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "720"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "100"},
			},
		},
		{
			AtNS: 10 * dayNS,
			// Standard pack deleting without Ref.ID: Standard has no minimum retention obligation,
			// so it must NOT trigger hasUnprovenColdPacks and must NOT suppress Glacier completeness!
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					State: "Deleting",
					Size:  int64(64 * 1024 * 1024),
					Class: storage.Standard,
				},
				LiveBytes: int64(64 * 1024 * 1024),
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.Report.CompleteTotalUSD == nil {
		t.Fatal("expected CompleteTotalUSD to be non-nil for Glacier IR when deleted pack is Standard without Ref.ID (Standard has no retention obligation)")
	}
	if irPol.SavingsUSD == nil {
		t.Fatal("expected SavingsUSD to be non-nil for Glacier IR")
	}
}

func TestComparePoliciesEarlyDeletionAccruedAcrossLifecycleProgression(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	oneGiB := int64(1024 * 1024 * 1024)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	trace := baseTrace
	trace.Name = "lifecycle-progression-early-deletion"
	trace.DurationNS = 30 * dayNS
	trace.Start = startTime
	trace.Omissions = []string{"Full verified observation"}

	// Pack 1: Retiring without MinRetentionUntil at day 5, then Deleted with valid MinRetentionUntil at day 10.
	// Early deletion fee MUST be charged based on verified deadline from event 2!
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  uint64(oneGiB),
				Physical: uint64(oneGiB),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(oneGiB),
			},
		},
		{
			AtNS: 5 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-progression", Generation: "g1"},
					State: "Retiring",
					Size:  oneGiB,
					Class: storage.GlacierIR,
					// MinRetentionUntil is zero at this observation
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "pack-progression", Generation: "g1"},
					State:             "Deleted",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour), // Verified retention deadline present here!
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 12 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "pack-progression", Generation: "g1"},
					State:             "Retired",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	// 1 GiB for 80 unexpired days in Glacier IR ($0.005/GB-Mo) = 1/75 USD.
	if irPol.Breakdown.EarlyDeletionUSD != "1/75" {
		t.Fatalf("expected early deletion fee to be '1/75', got %q (was early deletion dropped when deadline appeared in later event?)", irPol.Breakdown.EarlyDeletionUSD)
	}

	// Now verify that if a cold pack NEVER receives a verified deadline, completeness is suppressed
	traceUnresolved := trace
	traceUnresolved.Name = "unresolved-retention-deadline"
	traceUnresolved.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  uint64(oneGiB),
				Physical: uint64(oneGiB),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(oneGiB),
			},
		},
		{
			AtNS: 5 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-unresolved", Generation: "g1"},
					State: "Retiring",
					Size:  oneGiB,
					Class: storage.GlacierIR,
					// MinRetentionUntil remains zero forever
				},
				LiveBytes: oneGiB,
			},
		},
	}
	resUnresolved, err := ComparePolicies(context.Background(), traceUnresolved, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resUnresolved.Policies[2].Report.CompleteTotalUSD != nil {
		t.Fatalf("expected CompleteTotalUSD to be nil when cold pack retention deadline is never resolved, got %v", *resUnresolved.Policies[2].Report.CompleteTotalUSD)
	}
}

func TestComparePoliciesRestoreAmplificationMixedReadsInSameEventReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "mixed-reads-same-event"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Single event with 1 GiB cold retrieval, 64 MiB cold-delivered, AND 1 GiB warm-delivered:
	// Total gateway_to_client = 64 MiB + 1 GiB = 1140850688 bytes.
	// Since warm and cold reads are mixed in the same event, cold-delivered bytes cannot be
	// isolated from warm-delivered bytes.
	// Simulator must return nil (null) rather than an unfounded ratio like 0.9412!
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: oneGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB + oneGiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
				{RateID: "s3.get_standard", Unit: "Requests", Quantity: "100"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.RestoreAmplification != nil {
		t.Fatalf("expected RestoreAmplification to be nil for mixed warm and cold reads in same event, got %v", *irPol.RestoreAmplification)
	}
}
