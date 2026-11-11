package simulator

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

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
			ColdLogicalDeliveredBytes: 64 * 1024 * 1024,
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

func TestComparePoliciesRestoreAmplificationPolicyIsolation(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// Trace with cold retrieval only on Glacier IR (s3.retrieval_ir)
	traceIR := baseTrace
	traceIR.Name = "ir-amplification-isolation"
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
			ColdLogicalDeliveredBytes: 64 * 1024 * 1024,
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

	// S3 GET standard reads in a separate event must NOT dilute cold restore amplification
	traceWithWarm := traceIR
	traceWithWarm.Name = "warm-reads-do-not-dilute-amplification"
	traceWithWarm.Events = append(traceWithWarm.Events, Event{
		AtNS: 10 * dayNS,
		Traffic: map[cost.Route]uint64{
			cost.GatewayClient: 100 * 1024 * 1024, // 100 MB warm read delivery
		},
		Usage: []cost.Usage{
			{RateID: "s3.get_standard", Unit: "Requests", Quantity: "50"},
		},
	})
	resWarm, err := ComparePolicies(context.Background(), traceWithWarm, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies warm failed: %v", err)
	}
	if resWarm.Policies[2].RestoreAmplification == nil || *resWarm.Policies[2].RestoreAmplification != 16.0 {
		t.Fatalf("expected IR amplification to stay 16.0 without warm-read dilution, got %v", resWarm.Policies[2].RestoreAmplification)
	}

	// If GatewayClient traffic is 0, denominator is 0 -> RestoreAmplification must be nil
	traceNoClient := traceIR
	traceNoClient.Name = "zero-client-traffic"
	traceNoClient.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: 1073741824,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}
	resNoClient, err := ComparePolicies(context.Background(), traceNoClient, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies no client failed: %v", err)
	}
	if resNoClient.Policies[2].RestoreAmplification != nil {
		t.Fatalf("expected nil amplification when GatewayClient traffic is 0, got %v", *resNoClient.Policies[2].RestoreAmplification)
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

func TestComparePoliciesRestoreAmplificationWarmCacheHitsWithoutPerOperationTelemetryReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "warm-cache-hits-amplification"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)

	// Extaseq regression:
	// - Cold retrieval: 1 GiB
	// - Cold-delivered: 64 MiB
	// - Warm cache-delivered: 128 MiB (no S3 GET usage)
	// - Total gateway_to_client: 192 MiB
	// When the trace only records aggregate traffic (192 MiB) without per-operation cold delivery telemetry,
	// the simulator must return nil (null) because the denominator cannot be proven (do not assume clientBytes <= coldBytesRetrieved is all cold).
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
				cost.GatewayClient: (64 + 128) * 1024 * 1024, // 192 MiB total
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.RestoreAmplification != nil {
		t.Fatalf("expected RestoreAmplification to be nil when per-operation cold delivery telemetry is missing, got %v (must not assume aggregate traffic 192 MiB is cold)", *irPol.RestoreAmplification)
	}

	// When per-operation cold delivery telemetry IS provided via ColdLogicalDeliveredBytes:
	traceWithTelemetry := trace
	traceWithTelemetry.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: oneGiB,
			},
			ColdLogicalDeliveredBytes: 64 * 1024 * 1024,
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: (64 + 128) * 1024 * 1024, // 192 MiB total
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	resWithTel, err := ComparePolicies(context.Background(), traceWithTelemetry, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies with telemetry failed: %v", err)
	}
	irWithTel := resWithTel.Policies[2]
	if irWithTel.RestoreAmplification == nil || *irWithTel.RestoreAmplification != 16.0 {
		t.Fatalf("expected RestoreAmplification to be 16.0 with verified per-operation cold delivery telemetry, got %v", irWithTel.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationPackLiveBytesDoesNotDiluteClientDeliveredBytes(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "pack-live-bytes-vs-delivered"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)

	// Regression:
	// - Pack has 512 MiB live data (Tiering.LiveBytes = 512 MiB)
	// - Client reads 64 MiB (ColdLogicalDeliveredBytes = 64 MiB)
	// - Cold retrieval = 1 GiB
	// Simulator must return 16x (1024 / 64), NOT 2x (1024 / 512).
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
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "ir-pack-1", Generation: "g1"},
					Size:  int64(oneGiB),
					Class: storage.GlacierIR,
				},
				LiveBytes:  512 * 1024 * 1024, // 512 MiB live data in pack for tiering decisions
				TotalBytes: int64(oneGiB),
			},
			ColdLogicalDeliveredBytes: 64 * 1024 * 1024, // 64 MiB delivered to client for this cold read
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 64 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.RestoreAmplification == nil {
		t.Fatal("expected non-nil RestoreAmplification")
	}
	if *irPol.RestoreAmplification != 16.0 {
		t.Fatalf("expected RestoreAmplification to be 16.0 (1024/64), got %v (must not use Tiering.LiveBytes 512 MiB to yield 2x)", *irPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationUnrelatedTieringObservationDoesNotProvideDeliveryProof(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "unrelated-tiering-amplification"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)

	// Regression:
	// - Cold retrieval: 1 GiB
	// - Event contains a Tiering observation belonging to an unrelated pack (e.g. background scan)
	// - No ColdLogicalDeliveredBytes telemetry provided for the cold read
	// Simulator must return nil (null) because Tiering observation does not prove cold delivery for this read.
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
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "unrelated-pack", Generation: "g99"},
					Size:  int64(oneGiB),
					Class: storage.GlacierIR,
				},
				LiveBytes:  64 * 1024 * 1024, // Unrelated pack live bytes
				TotalBytes: int64(oneGiB),
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 64 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.RestoreAmplification != nil {
		t.Fatalf("expected RestoreAmplification to be nil when cold read lacks delivery telemetry (must not repurpose unrelated Tiering observation), got %v", *irPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationZeroClientTrafficReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "zero-client-traffic-amplification"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)

	// Regression 1:
	// - gateway_to_client = 0
	// - cold-delivered = 64 MiB
	// - cold retrieval = 1 GiB
	// When measured client traffic in the scope is 0, any claimed cold delivery contradicts
	// measured traffic. Simulator must return nil (null) instead of 16x.
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
			ColdLogicalDeliveredBytes: 64 * 1024 * 1024,
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 0,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.RestoreAmplification != nil {
		t.Fatalf("expected RestoreAmplification to be nil when gateway_to_client is 0 despite cold delivery telemetry, got %v", *irPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationMultiClassDeliveryExceedingClientTrafficReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "multi-class-delivery-exceeds-client-traffic"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Regression 2:
	// - IR cold retrieval = 1 GiB, Flexible cold restore = 1 GiB
	// - Total cold-delivered = 64 MiB (IR) + 64 MiB (Flexible) = 128 MiB
	// - Client traffic (gateway_to_client) = 64 MiB
	// Total claimed cold delivery across classes (128 MiB) exceeds measured client traffic (64 MiB).
	// Since the data is contradictory, BOTH policies must return nil (null).
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2 * oneGiB,
				Physical: 2 * oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: oneGiB,
				storage.Glacier:   oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.GlacierIR: sixtyFourMiB,
				storage.Glacier:   sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB, // Only 64 MiB measured client traffic
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	flexPol := res.Policies[3]

	if irPol.RestoreAmplification != nil {
		t.Fatalf("expected IR RestoreAmplification to be nil when total cold delivery exceeds client traffic, got %v", *irPol.RestoreAmplification)
	}
	if flexPol.RestoreAmplification != nil {
		t.Fatalf("expected Flex RestoreAmplification to be nil when total cold delivery exceeds client traffic, got %v", *flexPol.RestoreAmplification)
	}

	// Conversely, when client traffic is sufficient (>= 128 MiB), both policies calculate 16.0x:
	traceValid := trace
	traceValid.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2 * oneGiB,
				Physical: 2 * oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: oneGiB,
				storage.Glacier:   oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.GlacierIR: sixtyFourMiB,
				storage.Glacier:   sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 2 * sixtyFourMiB, // 128 MiB measured client traffic accommodates both
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	resValid, err := ComparePolicies(context.Background(), traceValid, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies valid failed: %v", err)
	}
	if resValid.Policies[2].RestoreAmplification == nil || *resValid.Policies[2].RestoreAmplification != 16.0 {
		t.Fatalf("expected IR 16.0x with sufficient client traffic, got %v", resValid.Policies[2].RestoreAmplification)
	}
	if resValid.Policies[3].RestoreAmplification == nil || *resValid.Policies[3].RestoreAmplification != 16.0 {
		t.Fatalf("expected Flex 16.0x with sufficient client traffic, got %v", resValid.Policies[3].RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationScalarFallbackRules(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "scalar-fallback-rules"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Rule 1: When multiple cold storage classes have retrieval in the same event,
	// scalar telemetry cannot disambiguate delivered bytes between classes -> must return nil.
	traceMultiScalar := trace
	traceMultiScalar.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2 * oneGiB,
				Physical: 2 * oneGiB,
			},
			ColdLogicalDeliveredBytes: sixtyFourMiB, // Scalar number
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 128 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	resMulti, err := ComparePolicies(context.Background(), traceMultiScalar, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies multi-scalar failed: %v", err)
	}
	if resMulti.Policies[2].RestoreAmplification != nil {
		t.Fatalf("expected IR amplification nil when scalar telemetry is used with multiple cold classes, got %v", *resMulti.Policies[2].RestoreAmplification)
	}
	if resMulti.Policies[3].RestoreAmplification != nil {
		t.Fatalf("expected Flex amplification nil when scalar telemetry is used with multiple cold classes, got %v", *resMulti.Policies[3].RestoreAmplification)
	}

	// Rule 2: When class-specific telemetry is provided, NEVER fall back to scalar for missing classes.
	traceClassMap := trace
	traceClassMap.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2 * oneGiB,
				Physical: 2 * oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.GlacierIR: sixtyFourMiB, // Only IR specified
			},
			ColdLogicalDeliveredBytes: sixtyFourMiB, // Scalar also present
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 128 * 1024 * 1024,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	resClass, err := ComparePolicies(context.Background(), traceClassMap, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies class-map failed: %v", err)
	}
	// IR has explicit class-specific telemetry -> 16.0x
	if resClass.Policies[2].RestoreAmplification == nil || *resClass.Policies[2].RestoreAmplification != 16.0 {
		t.Fatalf("expected IR 16.0x with class-specific telemetry, got %v", resClass.Policies[2].RestoreAmplification)
	}
	// Flex has no entry in class map -> must NOT fall back to scalar -> nil
	if resClass.Policies[3].RestoreAmplification != nil {
		t.Fatalf("expected Flex amplification nil (must not fall back to scalar when class-specific map is present), got %v", *resClass.Policies[3].RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationCumulativeLogicalDeliveryCanExceedSingleRestore(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "cumulative-logical-delivery-reuse"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	twoGiB := uint64(2 * 1024 * 1024 * 1024)

	// Regression [P2]:
	// - 1 GiB pack restored once from Glacier Flexible
	// - 2 × 1 GiB client logical reads against that restored copy
	// - gateway_to_client = 2 GiB
	// - ColdLogicalDeliveredByClass[GLACIER] = 2 GiB
	// Contract ratio = backend_pack_bytes_restored / logical_range_bytes_requested = 1 GiB / 2 GiB = 0.5x.
	// Simulator must NOT reject delivered > coldBytesInThisEvent when client traffic is proven.
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: twoGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: twoGiB,
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
	if flexPol.RestoreAmplification == nil {
		t.Fatal("expected non-nil RestoreAmplification for reused restored pack")
	}
	if *flexPol.RestoreAmplification != 0.5 {
		t.Fatalf("expected 0.5x amplification (1 GiB / 2 GiB), got %v", *flexPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationDirectS3RouteDoesNotProveGatewayTraffic(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "s3-client-does-not-prove-gateway-traffic"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Regression [P2]:
	// - IR cold retrieval: 1 GiB
	// - cold-delivered: 64 MiB
	// - gateway_to_client: 0
	// - s3_to_client: 64 MiB
	// For Glacier policies, client reads are served through the DSG gateway (gateway_to_client).
	// Route s3_to_client belongs to the Direct S3 baseline and is NOT evidence of gateway traffic.
	// Simulator must return nil (null).
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
			ColdLogicalDeliveredBytes: sixtyFourMiB,
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 0,
				cost.S3Client:      sixtyFourMiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.RestoreAmplification != nil {
		t.Fatalf("expected nil amplification when gateway_to_client is 0 even if s3_to_client has traffic, got %v", *irPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationClientTrafficOverflowDetection(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "client-traffic-overflow-detection"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Regression [P3]:
	// Verify ClientTrafficBytes checked addition prevents uint64 wrap-around:
	eventOverflow := Event{
		Traffic: map[cost.Route]uint64{
			cost.GatewayClient: math.MaxUint64,
			cost.S3Client:      sixtyFourMiB + 1,
		},
	}
	traffic, ok := eventOverflow.ClientTrafficBytes()
	if ok {
		t.Fatalf("expected ClientTrafficBytes to detect overflow and return ok=false, got ok=true traffic=%d", traffic)
	}

	// Verify TotalColdDeliveredBytes checked addition prevents uint64 wrap-around:
	eventColdOverflow := Event{
		ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
			storage.GlacierIR: math.MaxUint64,
			storage.Glacier:   1,
		},
	}
	totalCold, okCold := eventColdOverflow.TotalColdDeliveredBytes()
	if okCold {
		t.Fatalf("expected TotalColdDeliveredBytes to detect overflow and return ok=false, got ok=true totalCold=%d", totalCold)
	}

	// Trace with overflow telemetry must fail closed and return nil:
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
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.GlacierIR: math.MaxUint64,
				storage.Glacier:   1,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.RestoreAmplification != nil {
		t.Fatalf("expected nil amplification on overflow telemetry, got %v", *irPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationGlacierFlexReadsAcrossEvents(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "flex-reads-across-events"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Reviewer Regression [P2]:
	// - Event 1: 1 GiB physical restore, 64 MiB logical delivered, gateway_to_client = 64 MiB
	// - Event 2: 0 GiB physical restore, 1 GiB logical delivered, gateway_to_client = 1 GiB (within 24h)
	// - Total physical restore = 1 GiB (1024 MiB)
	// - Total logical delivered = 64 MiB + 1024 MiB = 1088 MiB (1.0625 GiB)
	// Expected amplification: 1024 / 1088 ≈ 0.941176... (0.9412x).
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(time.Hour),
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: oneGiB,
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	if flexPol.RestoreAmplification == nil {
		t.Fatal("expected non-nil RestoreAmplification for Glacier Flex across events")
	}

	expectedRatio := float64(oneGiB) / float64(sixtyFourMiB+oneGiB) // ~0.941176...
	diff := math.Abs(*flexPol.RestoreAmplification - expectedRatio)
	if diff > 1e-4 {
		t.Fatalf("expected amplification ~%.4f (0.9412x), got %v", expectedRatio, *flexPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationGlacierFlexReadsAcrossEventsWithScalarTelemetry(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "flex-reads-across-events-scalar"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Verify scalar telemetry works across events when Glacier Flexible is the only cold class in trace:
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredBytes: sixtyFourMiB,
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(2 * time.Hour),
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredBytes: oneGiB,
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: oneGiB,
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	if flexPol.RestoreAmplification == nil {
		t.Fatal("expected non-nil RestoreAmplification for Glacier Flex with scalar telemetry across events")
	}

	expectedRatio := float64(oneGiB) / float64(sixtyFourMiB+oneGiB)
	diff := math.Abs(*flexPol.RestoreAmplification - expectedRatio)
	if diff > 1e-4 {
		t.Fatalf("expected amplification ~%.4f (0.9412x), got %v", expectedRatio, *flexPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationGlacierFlexDeliveryWithoutPriorRestoreReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "flex-delivery-without-prior-restore"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)

	// Event claims cold delivery from Glacier Flexible, but NO physical restore was ever performed:
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: oneGiB,
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	if flexPol.RestoreAmplification != nil {
		t.Fatalf("expected nil amplification when delivery is claimed without prior restore, got %v", *flexPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationGlacierFlexDeliveryAfterRestoreExpiredReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "flex-delivery-after-restore-expired"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Event 1 restores at t=0 (active for 24 hours).
	// Event 2 claims cold delivery at t=48 hours (after 24-hour restore window expired).
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(48 * time.Hour), // 48h > 24h -> expired
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: oneGiB,
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	if flexPol.RestoreAmplification != nil {
		t.Fatalf("expected nil amplification when delivery occurs after restore expired, got %v", *flexPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationGlacierIRDeliveryWithoutRetrievalUsageInEventReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "ir-delivery-without-retrieval-usage"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Glacier IR is instant retrieval (no temporary restored copy).
	// A cold read in Event 2 cannot be attributed without an s3.retrieval_ir in Event 2.
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
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.GlacierIR: sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(time.Hour),
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.GlacierIR: oneGiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: oneGiB,
			},
			Usage: []cost.Usage{}, // No s3.retrieval_ir in this event
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.RestoreAmplification != nil {
		t.Fatalf("expected nil amplification when Glacier IR delivery has no retrieval usage in event, got %v", *irPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationAutoRestoreDaysThreePassesAtHour48(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "flex-auto-restore-days-3"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Reviewer Regression [P2]:
	// auto_restore_days = 3
	// - Restore 1 GiB at hour 0.
	// - Client reads 64 MiB at hour 48.
	// - Expected amplification: 16x because restore is active for 72h.
	trace.Policy = json.RawMessage(`{
		"tiering": {
			"enabled": true,
			"warm_storage_class": "STANDARD",
			"cold_storage_class": "GLACIER",
			"cold_after_days": 30,
			"min_pack_live_ratio": 0.8,
			"auto_restore": true,
			"auto_restore_days": 3,
			"auto_restore_tier": "Standard"
		},
		"compression": {
			"enabled": true,
			"codec": "zstd",
			"level": 3,
			"min_saving_ratio": 0.1
		},
		"chunking": {
			"min_bytes": 262144,
			"avg_bytes": 1048576,
			"max_bytes": 4194304
		}
	}`)

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(48 * time.Hour), // hour 48 is within 3-day (72h) restore window
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	if flexPol.RestoreAmplification == nil {
		t.Fatal("expected non-nil RestoreAmplification for Glacier Flex at hour 48 when auto_restore_days = 3")
	}

	if *flexPol.RestoreAmplification != 16.0 {
		t.Fatalf("expected amplification 16.0x, got %v", *flexPol.RestoreAmplification)
	}

	// Verify that at hour 96 (past 72h window), delivery is rejected as expired
	traceExpired := trace
	traceExpired.Name = "flex-auto-restore-days-3-expired"
	traceExpired.Events = []Event{
		trace.Events[0],
		{
			AtNS: int64(96 * time.Hour), // 96h > 72h -> expired
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{},
		},
	}

	resExp, err := ComparePolicies(context.Background(), traceExpired, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resExp.Policies[3].RestoreAmplification != nil {
		t.Fatalf("expected nil amplification when read occurs at hour 96 with 3-day restore, got %v", *resExp.Policies[3].RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationPackRestoredUntilEvidenceOverridesPolicy(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "flex-pack-restored-until-override"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Policy has auto_restore_days = 1 (24 hours).
	// But pack metadata explicitly specifies RestoredUntil = 48 hours.
	// Pack/operation metadata takes precedence over policy.
	// At hour 36:
	// - If policy (24h) were used, read at 36h would be expired (nil).
	// - With pack RestoredUntil (48h), read at 36h is active -> returns 16.0x!
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					State:         "Restored",
					Class:         storage.Glacier,
					RestoredUntil: trace.Start.Add(48 * time.Hour),
				},
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(36 * time.Hour), // 36h > 24h (policy) but <= 48h (pack metadata)
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	if flexPol.RestoreAmplification == nil {
		t.Fatal("expected non-nil RestoreAmplification when pack RestoredUntil overrides policy")
	}

	if *flexPol.RestoreAmplification != 16.0 {
		t.Fatalf("expected amplification 16.0x, got %v", *flexPol.RestoreAmplification)
	}
}

func TestComparePoliciesRestoreAmplificationMissingExpiryEvidenceReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, _ := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "flex-missing-expiry-evidence"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Reviewer requirement: "Với trace thiếu bằng chứng expiry, cần giữ kết quả chưa xác định thay vì tự suy đoán."
	// Trace has no auto_restore_days in policy, and no pack RestoredUntil metadata.
	trace.Policy = json.RawMessage(`{}`)

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	amp := calcRestoreAmplification(PolicyWarmGlacierFlex, trace)
	if amp != nil {
		t.Fatalf("expected nil amplification when trace lacks restore expiry evidence, got %v", *amp)
	}
}

func TestComparePoliciesRestoreAmplificationPackRestoredUntilAlreadyExpiredReturnsNil(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "flex-pack-already-expired"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

	// Pack RestoredUntil is 1 hour from trace start.
	// Read is at hour 2.
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					State:         "Restored",
					Class:         storage.Glacier,
					RestoredUntil: trace.Start.Add(time.Hour),
				},
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(2 * time.Hour), // 2h > 1h -> already expired
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: sixtyFourMiB,
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	if flexPol.RestoreAmplification != nil {
		t.Fatalf("expected nil amplification when pack RestoredUntil proves pack was expired, got %v", *flexPol.RestoreAmplification)
	}
}
