package simulator

import (
	"context"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

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

func TestComparePoliciesS3RateIsolationAcrossPolicies(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")
	oneGiB := uint64(1024 * 1024 * 1024)

	// Ensure all required infrastructure dimensions have verified rate tiers
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

	baseUsage := []cost.Usage{
		{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
		{RateID: "s3.get_standard", Unit: "Requests", Quantity: "50"},
		{RateID: "transfer.internet_egress", Unit: "GB", Quantity: "1"},
		{RateID: "ec2.compute", Unit: "Hrs", Quantity: "10"},
		{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "10"},
		{RateID: "alb.hour", Unit: "Hrs", Quantity: "10"},
		{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "10"},
		{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
		{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "10"},
	}

	// Reviewer Finding [P1] (HEAD e834d29 audit):
	// warm_standard_only was wrongly charged for s3.put_ir, s3.get_ir, s3.put_glacier, Bulk Restore;
	// warm_glacier_flexible also wrongly received IR operational charges.
	// Adding 10,000 IR PUTs must NOT alter CompleteTotalUSD or SavingsUSD of warm_standard_only or warm_glacier_flexible!
	traceBase := baseTrace
	traceBase.Name = "rate-isolation-base"
	traceBase.DurationNS = 30 * dayNS
	traceBase.Omissions = []string{"Full verified observation run across all topology dimensions"}
	traceBase.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: oneGiB,
			},
			Usage: baseUsage,
		},
	}

	resBase, err := ComparePolicies(context.Background(), traceBase, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	if resBase.Policies[0].Report.CompleteTotalUSD == nil {
		t.Fatal("expected Direct S3 CompleteTotalUSD to be non-nil")
	}
	if resBase.Policies[1].Report.CompleteTotalUSD == nil {
		t.Fatal("expected WarmStandardOnly CompleteTotalUSD to be non-nil")
	}
	if resBase.Policies[1].SavingsUSD == nil {
		t.Fatal("expected WarmStandardOnly SavingsUSD to be non-nil")
	}

	baseDirectTotal := *resBase.Policies[0].Report.CompleteTotalUSD
	baseStdTotal := *resBase.Policies[1].Report.CompleteTotalUSD
	baseStdSavings := *resBase.Policies[1].SavingsUSD
	baseFlexTotal := *resBase.Policies[3].Report.CompleteTotalUSD
	baseFlexSavings := *resBase.Policies[3].SavingsUSD

	// 1. Add 10,000 s3.put_ir to trace:
	irUsage := make([]cost.Usage, len(baseUsage), len(baseUsage)+1)
	copy(irUsage, baseUsage)
	irUsage = append(irUsage, cost.Usage{RateID: "s3.put_ir", Unit: "Requests", Quantity: "10000"}) // 10,000 IR PUTs = 0.20 USD

	traceWithIR := traceBase
	traceWithIR.Name = "rate-isolation-ir-puts"
	traceWithIR.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: oneGiB,
			},
			Usage: irUsage,
		},
	}

	resWithIR, err := ComparePolicies(context.Background(), traceWithIR, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	// Direct S3 must NOT change
	if *resWithIR.Policies[0].Report.CompleteTotalUSD != baseDirectTotal {
		t.Fatalf("Direct S3 CompleteTotalUSD changed with IR PUTs: expected %q, got %q", baseDirectTotal, *resWithIR.Policies[0].Report.CompleteTotalUSD)
	}

	// WarmStandardOnly CompleteTotalUSD and SavingsUSD must NOT change (Finding P1 regression)!
	if *resWithIR.Policies[1].Report.CompleteTotalUSD != baseStdTotal {
		t.Fatalf("WarmStandardOnly CompleteTotalUSD changed with IR PUTs: expected %q, got %q", baseStdTotal, *resWithIR.Policies[1].Report.CompleteTotalUSD)
	}
	if *resWithIR.Policies[1].SavingsUSD != baseStdSavings {
		t.Fatalf("WarmStandardOnly SavingsUSD changed with IR PUTs: expected %q, got %q", baseStdSavings, *resWithIR.Policies[1].SavingsUSD)
	}

	// WarmGlacierFlexible CompleteTotalUSD and SavingsUSD must NOT change (Finding P1 regression)!
	if *resWithIR.Policies[3].Report.CompleteTotalUSD != baseFlexTotal {
		t.Fatalf("WarmGlacierFlexible CompleteTotalUSD changed with IR PUTs: expected %q, got %q", baseFlexTotal, *resWithIR.Policies[3].Report.CompleteTotalUSD)
	}
	if *resWithIR.Policies[3].SavingsUSD != baseFlexSavings {
		t.Fatalf("WarmGlacierFlexible SavingsUSD changed with IR PUTs: expected %q, got %q", baseFlexSavings, *resWithIR.Policies[3].SavingsUSD)
	}

	// ONLY WarmGlacierIR must incorporate the IR PUTs
	foundIRLine := false
	for _, l := range resWithIR.Policies[2].Report.Lines {
		if l.RateID == "s3.put_ir" {
			foundIRLine = true
			if l.Quantity != "10000" {
				t.Fatalf("expected 10000 s3.put_ir quantity, got %s", l.Quantity)
			}
		}
	}
	if !foundIRLine {
		t.Fatal("expected s3.put_ir line in WarmGlacierIR report")
	}

	// 2. Add Glacier Flexible operations (s3.put_glacier, s3.restore_bulk_requests, s3.restore_bulk_gb):
	flexUsage := make([]cost.Usage, len(baseUsage), len(baseUsage)+3)
	copy(flexUsage, baseUsage)
	flexUsage = append(flexUsage,
		cost.Usage{RateID: "s3.put_glacier", Unit: "Requests", Quantity: "5000"},
		cost.Usage{RateID: "s3.restore_bulk_requests", Unit: "Requests", Quantity: "20"},
		cost.Usage{RateID: "s3.restore_bulk_gb", Unit: "GB", Quantity: "1"},
	)

	traceWithFlex := traceBase
	traceWithFlex.Name = "rate-isolation-flex-ops"
	traceWithFlex.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: oneGiB,
			},
			Usage: flexUsage,
		},
	}

	resWithFlex, err := ComparePolicies(context.Background(), traceWithFlex, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	// Direct S3 must NOT change
	if *resWithFlex.Policies[0].Report.CompleteTotalUSD != baseDirectTotal {
		t.Fatalf("Direct S3 CompleteTotalUSD changed with Flex ops: expected %q, got %q", baseDirectTotal, *resWithFlex.Policies[0].Report.CompleteTotalUSD)
	}

	// WarmStandardOnly CompleteTotalUSD and SavingsUSD must NOT change!
	if *resWithFlex.Policies[1].Report.CompleteTotalUSD != baseStdTotal {
		t.Fatalf("WarmStandardOnly CompleteTotalUSD changed with Flex ops: expected %q, got %q", baseStdTotal, *resWithFlex.Policies[1].Report.CompleteTotalUSD)
	}
	if *resWithFlex.Policies[1].SavingsUSD != baseStdSavings {
		t.Fatalf("WarmStandardOnly SavingsUSD changed with Flex ops: expected %q, got %q", baseStdSavings, *resWithFlex.Policies[1].SavingsUSD)
	}

	// WarmGlacierIR CompleteTotalUSD and SavingsUSD must NOT change!
	baseIRTotal := *resBase.Policies[2].Report.CompleteTotalUSD
	baseIRSavings := *resBase.Policies[2].SavingsUSD
	if *resWithFlex.Policies[2].Report.CompleteTotalUSD != baseIRTotal {
		t.Fatalf("WarmGlacierIR CompleteTotalUSD changed with Flex ops: expected %q, got %q", baseIRTotal, *resWithFlex.Policies[2].Report.CompleteTotalUSD)
	}
	if *resWithFlex.Policies[2].SavingsUSD != baseIRSavings {
		t.Fatalf("WarmGlacierIR SavingsUSD changed with Flex ops: expected %q, got %q", baseIRSavings, *resWithFlex.Policies[2].SavingsUSD)
	}
}
