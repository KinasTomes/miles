package simulator

import (
	"context"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

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
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
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
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
			},
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
