package simulator

import (
	"context"
	"encoding/json"
	"math"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

// Item 1 [P1]: False CompleteTotalUSD and savings in candidate policies lacking cold pricing.
// Trace has IR cold retrieval (1 GiB) and IR delivery (64 MiB) with complete infrastructure and network.
// Glacier Flexible must fail closed (CompleteTotalUSD = nil, SavingsUSD = nil, RestoreUSD = "0")
// because cold reads were performed but Flexible restore costs are unmodeled.
func TestAuditItem1_CandidatePolicyLackingColdPricingFailsClosed(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// Enable verified rates
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
	trace.Name = "item-1-cold-pricing-coverage"
	trace.DurationNS = 30 * dayNS
	trace.Omissions = []string{"Deterministic simulation note"}

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
					State: "Cold",
					Size:  int64(oneGiB),
					Class: storage.GlacierIR,
				},
				LiveBytes: int64(oneGiB),
			},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 64 * 1024 * 1024,
				cost.GatewayClient: 64 * 1024 * 1024,
				cost.GatewayS3:     0,
				cost.S3Gateway:     0,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "s3.get_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1"},
				{RateID: "transfer.internet_egress", Unit: "GB", Quantity: "1"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "720"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "20"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "720"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "720"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "100"},
			},
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "s3.get_standard", Unit: "Requests", Quantity: "10"},
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.GlacierIR: 64 * 1024 * 1024,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	// WarmGlacierIR has IR retrieval pricing line and should be complete
	irPol := res.Policies[2]
	if irPol.Report.CompleteTotalUSD == nil {
		t.Fatal("expected WarmGlacierIR CompleteTotalUSD to be non-nil when IR retrieval is priced")
	}
	if irPol.SavingsUSD == nil {
		t.Fatal("expected WarmGlacierIR SavingsUSD to be non-nil")
	}

	// WarmGlacierFlexible lacks restore pricing lines for the cold reads and must fail closed!
	flexPol := res.Policies[3]
	if flexPol.Report.CompleteTotalUSD != nil {
		t.Fatal("expected WarmGlacierFlexible CompleteTotalUSD to be nil when trace has cold reads but lacks restore pricing")
	}
	if flexPol.SavingsUSD != nil {
		t.Fatal("expected WarmGlacierFlexible SavingsUSD to be nil when candidate cold read pricing is missing")
	}
	if flexPol.Breakdown != nil && flexPol.Breakdown.RestoreUSD != "0" {
		t.Fatalf("expected RestoreUSD to be '0', got %s", flexPol.Breakdown.RestoreUSD)
	}
}

// Item 2 [P2]: Zero PackRef.ID merges distinct restores.
// Two restores with empty PackRef.ID (1 GiB each) and a subsequent 64 MiB cold read without Ref.ID.
// Anonymous restores must not be treated as a single shared restore identity.
// Fails closed with null amplification (ambiguous attribution).
func TestAuditItem2_ZeroPackRefIDDoesNotMergeRestoresAndFailsClosed(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	oneGiB := uint64(1024 * 1024 * 1024)
	trace := baseTrace
	trace.Name = "item-2-anon-restore-isolation"
	trace.DurationNS = 30 * dayNS
	trace.Omissions = []string{"Deterministic simulation note"}

	trace.Events = []Event{
		// Restore #1: anonymous (empty Ref.ID)
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
		// Restore #2: anonymous (empty Ref.ID)
		{
			AtNS: 1 * dayNS,
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		// Read at day 2 without PackRef.ID: ambiguous attribution across multiple restored packs -> fail closed (nil)
		{
			AtNS: 2 * dayNS,
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 64 * 1024 * 1024,
			},
			ColdLogicalDeliveredBytes: 64 * 1024 * 1024,
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	if flexPol.RestoreAmplification != nil {
		t.Fatalf("expected nil restore amplification for ambiguous anonymous restores, got %v", *flexPol.RestoreAmplification)
	}

	// In replay, temporary restored bytes must account for BOTH 1 GiB restores without truncating each other
	if res.Policies[3].TemporaryRestoredBytes < 2*oneGiB {
		t.Fatalf("expected at least 2 GiB temporary restored bytes, got %d", res.Policies[3].TemporaryRestoredBytes)
	}
}

// Item 3 [P2]: Baseline Direct S3 requires independent client request telemetry (DirectUsage).
// Without DirectUsage, Direct S3 fails closed on CompleteTotalUSD and emits omission.
// With DirectUsage, Direct S3 evaluates client requests independently of backend DSG pack requests.
func TestAuditItem3_DirectS3BaselineRequiresIndependentClientRequests(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

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
	traceWithoutDirect := baseTrace
	traceWithoutDirect.Name = "item-3-no-direct-usage"
	traceWithoutDirect.DurationNS = 30 * dayNS
	traceWithoutDirect.Omissions = []string{"Deterministic simulation note"}

	traceWithoutDirect.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
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
				// Backend DSG pack and sidecar upload: 2 PUTs
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "2"},
				{RateID: "transfer.internet_egress", Unit: "GB", Quantity: "1"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "720"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "20"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "720"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "720"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "100"},
			},
			// DirectUsage omitted!
		},
	}

	resWithout, err := ComparePolicies(context.Background(), traceWithoutDirect, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	// 1. Direct S3 must fail closed without DirectUsage
	if resWithout.Policies[0].Report.CompleteTotalUSD != nil {
		t.Fatal("expected Direct S3 CompleteTotalUSD to be nil without independent client requests telemetry")
	}
	if resWithout.Policies[1].SavingsUSD != nil {
		t.Fatal("expected candidate SavingsUSD to be nil when Direct S3 baseline is incomplete")
	}

	// 2. Supply independent DirectUsage (100 client operations)
	traceWithDirect := traceWithoutDirect
	traceWithDirect.Name = "item-3-with-direct-usage"
	traceWithDirect.Events = []Event{
		{
			AtNS:         traceWithoutDirect.Events[0].AtNS,
			Snapshot:     traceWithoutDirect.Events[0].Snapshot,
			StorageBytes: traceWithoutDirect.Events[0].StorageBytes,
			Traffic:      traceWithoutDirect.Events[0].Traffic,
			Usage:        traceWithoutDirect.Events[0].Usage,
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
			},
		},
	}

	resWith, err := ComparePolicies(context.Background(), traceWithDirect, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	if resWith.Policies[0].Report.CompleteTotalUSD == nil {
		t.Fatal("expected Direct S3 CompleteTotalUSD to be non-nil with explicit DirectUsage")
	}
	if resWith.Policies[1].SavingsUSD == nil {
		t.Fatal("expected candidate SavingsUSD to be non-nil with complete baseline")
	}

	// Verify Direct S3 evaluated 100 PUTs, while WarmStandardOnly evaluated 2 PUTs
	var directPUTs, warmPUTs string
	for _, l := range resWith.Policies[0].Report.Lines {
		if l.RateID == "s3.put_standard" {
			directPUTs = l.Quantity
		}
	}
	for _, l := range resWith.Policies[1].Report.Lines {
		if l.RateID == "s3.put_standard" {
			warmPUTs = l.Quantity
		}
	}
	if directPUTs != "100" {
		t.Fatalf("expected Direct S3 to evaluate 100 client PUTs, got %s", directPUTs)
	}
	if warmPUTs != "2" {
		t.Fatalf("expected WarmStandardOnly to evaluate 2 backend PUTs, got %s", warmPUTs)
	}
}

// Item 4 [P2]: Glacier IR overflow on retrieval accumulation.
// Cumulative retrieval 2^63 + 2^63 + 1 GiB overflows uint64.
// Must be caught with checked arithmetic and fail closed with null amplification.
func TestAuditItem4_GlacierIRAccumulationOverflow(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "item-4-ir-overflow"
	trace.DurationNS = 30 * dayNS

	// Two gigantic quantities that sum to > 2^64-1 bytes
	giganticGB := new(big.Int).Div(new(big.Int).SetUint64(math.MaxUint64), big.NewInt(1024*1024*1024)).Int64()
	qStr := big.NewRat(giganticGB, 1).RatString()

	trace.Events = []Event{
		{
			AtNS: 0,
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: qStr},
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 1024 * 1024,
			},
			ColdLogicalDeliveredBytes: 1024 * 1024,
		},
		{
			AtNS: 1 * dayNS,
			Usage: []cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: qStr},
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 1024 * 1024,
			},
			ColdLogicalDeliveredBytes: 1024 * 1024,
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	if res.Policies[2].RestoreAmplification != nil {
		t.Fatalf("expected nil restore amplification on overflow, got %v", *res.Policies[2].RestoreAmplification)
	}
}

// Item 5 [P2]: Conflicting cold delivery telemetry in JSON unmarshal rejected.
// When an event JSON specifies both scalar cold_logical_delivered_bytes and
// cold_logical_delivered_by_class with conflicting amounts, unmarshaling must return an error.
func TestAuditItem5_ConflictingColdDeliveryTelemetryUnmarshalError(t *testing.T) {
	conflictingJSON := []byte(`{
		"at_ns": 1000,
		"traffic_bytes": {"gateway_to_client": 100000000},
		"cold_logical_delivered_bytes": 67108864,
		"cold_logical_delivered_by_class": {
			"GLACIER_IR": 1048576
		}
	}`)

	var ev Event
	err := json.Unmarshal(conflictingJSON, &ev)
	if err == nil {
		t.Fatal("expected json.Unmarshal to fail on conflicting cold delivery telemetry, but got nil error")
	}
	if !strings.Contains(err.Error(), "conflicting cold delivery telemetry") {
		t.Fatalf("expected conflict error message, got: %v", err)
	}
}

// Item 6 [P2]: Transition breakdown disjoint and additive.
// Reconciles breakdown: StorageUSD + RequestsUSD + RetrievalUSD + TransitionUSD + RestoreUSD + EarlyDeletionUSD + NetworkUSD + InfrastructureUSD == Subtotal.
// TransitionUSD must not be double counted in RequestsUSD.
func TestAuditItem6_TransitionBreakdownDisjointAndAdditive(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "item-6-breakdown-reconciliation"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)

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
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
				{RateID: "s3.transition_standard_to_glacier_ir", Unit: "Requests", Quantity: "1000"},
			},
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	for _, p := range res.Policies {
		if p.Breakdown == nil {
			continue
		}
		// Sum all breakdown components
		sum := new(big.Rat)
		for _, sVal := range []string{
			p.Breakdown.StorageUSD,
			p.Breakdown.RequestsUSD,
			p.Breakdown.RetrievalUSD,
			p.Breakdown.TransitionUSD,
			p.Breakdown.RestoreUSD,
			p.Breakdown.EarlyDeletionUSD,
			p.Breakdown.NetworkUSD,
			p.Breakdown.InfrastructureUSD,
		} {
			if sVal == "" {
				sVal = "0"
			}
			q, ok := new(big.Rat).SetString(sVal)
			if !ok {
				t.Fatalf("invalid breakdown rational %q in policy %s", sVal, p.Name)
			}
			sum.Add(sum, q)
		}

		subtotalRat, ok := new(big.Rat).SetString(p.Report.Subtotal)
		if !ok {
			t.Fatalf("invalid subtotal %q in policy %s", p.Report.Subtotal, p.Name)
		}

		if sum.Cmp(subtotalRat) != 0 {
			t.Fatalf("policy %s breakdown is non-additive! sum of components %s != subtotal %s", p.Name, sum.RatString(), subtotalRat.RatString())
		}
	}
}

// Item 8 [P3]: ratGBToBytes exactness and bounds.
func TestAuditItem8_RatGBToBytesExactnessAndBounds(t *testing.T) {
	bytesPerGB := uint64(1024 * 1024 * 1024)

	// 1. Exact 1 GB
	b, ok := ratGBToBytes(big.NewRat(1, 1), bytesPerGB)
	if !ok || b != bytesPerGB {
		t.Fatalf("expected %d bytes, got %d, ok=%v", bytesPerGB, b, ok)
	}

	// 2. Exact 1/1024 GB = 1 MiB
	b, ok = ratGBToBytes(big.NewRat(1, 1024), bytesPerGB)
	if !ok || b != 1024*1024 {
		t.Fatalf("expected 1048576 bytes, got %d, ok=%v", b, ok)
	}

	// 3. Negative or zero -> returns false
	if _, ok := ratGBToBytes(big.NewRat(0, 1), bytesPerGB); ok {
		t.Fatal("expected false for zero quantity")
	}
	if _, ok := ratGBToBytes(big.NewRat(-1, 1), bytesPerGB); ok {
		t.Fatal("expected false for negative quantity")
	}

	// 4. Quantity exceeding uint64 -> returns false
	hugeRat := new(big.Rat).SetInt(new(big.Int).Mul(big.NewInt(math.MaxInt64), big.NewInt(100)))
	if _, ok := ratGBToBytes(hugeRat, bytesPerGB); ok {
		t.Fatal("expected false for overflow quantity")
	}
}

// Item 9 [P3]: Structured omission codes without false incompletes from benign sentences.
func TestAuditItem9_StructuredOmissionCodesNoFalseIncompletes(t *testing.T) {
	benignOmissions := []string{
		"Deterministic simulation note",
		"Omissions resolved by full measurement",
		"Trace inventory snapshot verified against manifest",
	}
	for _, om := range benignOmissions {
		if isIncompleteOmission(om) {
			t.Fatalf("benign omission %q falsely classified as incomplete", om)
		}
	}

	incompleteOmissions := []string{
		"omission:incomplete_infrastructure",
		"omission:unproven_cold_retention",
		"Missing EC2, EBS, ALB, or DynamoDB infrastructure telemetry",
		"Glacier storage unproven: cold storage inventory observed without per-pack tiering transition history",
	}
	for _, om := range incompleteOmissions {
		if !isIncompleteOmission(om) {
			t.Fatalf("incomplete omission %q not detected", om)
		}
	}
}

// Item 15 [P3]: Typed omission reason codes for cold accounting.
func TestAuditItem15_TypedOmissionReasonCodes(t *testing.T) {
	reasons := []UnprovenColdReason{
		UnprovenColdIdentity,
		UnprovenColdRetention,
		UnprovenColdStorageEvidence,
		UnprovenColdRestoreDuration,
		UnprovenColdReactivation,
	}

	for _, r := range reasons {
		text := r.OmissionText()
		if text == "" {
			t.Fatalf("empty omission text for reason %s", r)
		}
		if !strings.HasPrefix(text, "Glacier") {
			t.Fatalf("expected Glacier prefix in %q", text)
		}
	}
}

// Item 16 [P3] & Addendum 1: Restore lifecycle tracker metrics and replay parity.
// Verifies semantic parity between metrics restore-window tracking and replay temporary Standard GB-Mo:
//   - Expiry parity: active at deadline-1ns, expired at deadline
//   - Multi-pack parity: Pack A and Pack B tracked independently
//   - Shorter / longer expiry from authoritative metadata
//   - Illegal reactivation without physical restore rejected by both
func TestAuditItem16_RestoreLifecycleParityBetweenMetricsAndReplay(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	oneGiB := uint64(1024 * 1024 * 1024)

	// 1. Expiry Parity Test:
	// Restore 1 GiB at t=0 with 1 day duration (deadline = 1 dayNS).
	// Read #1 at t=dayNS - 1ns: restore is ACTIVE.
	traceActive := baseTrace
	traceActive.Name = "parity-active-restore"
	traceActive.Start = startTime
	traceActive.DurationNS = 30 * dayNS
	traceActive.Events = []Event{
		{
			AtNS: 0,
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-alpha", Generation: "g1"},
					RestoredUntil: startTime.Add(24 * time.Hour),
				},
			},
		},
		{
			AtNS: dayNS - 1, // 1 nanosecond before deadline: ACTIVE
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 64 * 1024 * 1024,
			},
			ColdLogicalDeliveredBytes: 64 * 1024 * 1024,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref: storage.PackRef{ID: "pack-alpha", Generation: "g1"},
				},
			},
		},
	}

	resActive, err := ComparePolicies(context.Background(), traceActive, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resActive.Policies[3].RestoreAmplification == nil {
		t.Fatal("expected valid restore amplification 1 nanosecond before restore deadline")
	}
	if resActive.Policies[3].TemporaryRestoredBytes != oneGiB {
		t.Fatalf("expected 1 GiB temporary restored bytes, got %d", resActive.Policies[3].TemporaryRestoredBytes)
	}

	// Read #2 at exact deadline t=dayNS: EXPIRED!
	traceExpired := traceActive
	traceExpired.Name = "parity-expired-restore"
	traceExpired.Events[1].AtNS = dayNS // Exact deadline: EXPIRED

	resExpired, err := ComparePolicies(context.Background(), traceExpired, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resExpired.Policies[3].RestoreAmplification != nil {
		t.Fatalf("expected nil restore amplification at exact deadline, got %v", *resExpired.Policies[3].RestoreAmplification)
	}

	// 2. Illegal Reactivation Rejected by Both:
	// Pack expires at hour 24. At hour 48, metadata claims RestoredUntil=hour 72 without restore usage.
	// Both metrics (null amplification) and replay (UnprovenColdReactivation) must reject it!
	traceReactivation := baseTrace
	traceReactivation.Name = "parity-illegal-reactivation"
	traceReactivation.Start = startTime
	traceReactivation.DurationNS = 30 * dayNS
	traceReactivation.Events = []Event{
		{
			AtNS: 0,
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-alpha", Generation: "g1"},
					RestoredUntil: startTime.Add(24 * time.Hour),
				},
			},
		},
		{
			AtNS: 48 * int64(time.Hour), // Expired 24 hours ago!
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-alpha", Generation: "g1"},
					RestoredUntil: startTime.Add(72 * time.Hour), // Illegal reactivation via metadata
				},
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: 64 * 1024 * 1024,
			},
			ColdLogicalDeliveredBytes: 64 * 1024 * 1024,
		},
	}

	resReact, err := ComparePolicies(context.Background(), traceReactivation, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resReact.Policies[3].RestoreAmplification != nil {
		t.Fatalf("expected nil restore amplification on illegal reactivation, got %v", *resReact.Policies[3].RestoreAmplification)
	}
}

// Item 1 [P1]: Cold pricing provenance matches event-by-event obligation.
func TestAuditItem1_ColdPricingProvenanceObligationMatching(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")
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
		case "s3.retrieval_ir":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.01"}}
		case "s3.restore_standard_gb":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.03"}}
		}
	}

	oneGiB := uint64(1024 * 1024 * 1024)
	sixtyFourMiB := uint64(64 * 1024 * 1024)

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

	// Case A: Repro mixed IR and Flex with unrelated restore.
	// Event 1 has GLACIER_IR cold delivery 64 MiB + s3.retrieval_ir.
	// Event 2 (t=10h) has independent restore Flex (no client read).
	// warm_glacier_flexible must NOT claim CompleteTotalUSD non-null!
	traceRepro := baseTrace
	traceRepro.Name = "cold-provenance-mixed"
	traceRepro.DurationNS = 30 * dayNS
	traceRepro.Omissions = []string{"Full verified observation run across all topology dimensions"}
	traceRepro.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard:  oneGiB - sixtyFourMiB,
				storage.GlacierIR: sixtyFourMiB,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-ir-1", Generation: "g1"},
					Size:  int64(sixtyFourMiB),
					Class: storage.GlacierIR,
					State: "Cold",
				},
			},
			Usage: append([]cost.Usage{
				{RateID: "s3.retrieval_ir", Unit: "GB", Quantity: "1/16"},
			}, baseUsage...),
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "s3.get_standard", Unit: "Requests", Quantity: "10"},
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.GlacierIR: sixtyFourMiB,
			},
		},
		{
			AtNS: 10 * int64(time.Hour),
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-flex-unrelated", Generation: "g1"},
					RestoredUntil: baseTrace.Start.Add(34 * time.Hour),
				},
			},
		},
	}

	resRepro, err := ComparePolicies(context.Background(), traceRepro, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	// warm_glacier_ir had matching retrieval at event 1
	if resRepro.Policies[2].Report.CompleteTotalUSD == nil {
		t.Fatalf("expected warm_glacier_ir CompleteTotalUSD to be non-nil; unpriced=%v, omissions=%v", resRepro.Policies[2].Report.Unpriced, resRepro.Omissions)
	}
	// warm_glacier_flexible had NO active restore covering event 1
	if resRepro.Policies[3].Report.CompleteTotalUSD != nil {
		t.Fatalf("expected warm_glacier_flexible CompleteTotalUSD to be nil on unproven cold read, got %s", *resRepro.Policies[3].Report.CompleteTotalUSD)
	}

	// Case B: Positive matched case.
	// Event 1 has physical restore for pack-matched.
	// Event 2 (within restore window) has client cold read on pack-matched.
	traceMatched := baseTrace
	traceMatched.Name = "cold-provenance-matched"
	traceMatched.DurationNS = 30 * dayNS
	traceMatched.Omissions = []string{"Full verified observation run across all topology dimensions"}
	traceMatched.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: oneGiB,
			},
			Usage: append([]cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			}, baseUsage...),
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "s3.get_standard", Unit: "Requests", Quantity: "10"},
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-matched", Generation: "g1"},
					RestoredUntil: baseTrace.Start.Add(24 * time.Hour),
				},
			},
		},
		{
			AtNS: 10 * int64(time.Hour),
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-matched", Generation: "g1"},
					RestoredUntil: baseTrace.Start.Add(24 * time.Hour),
				},
			},
			Traffic: map[cost.Route]uint64{
				cost.GatewayClient: sixtyFourMiB,
			},
			DirectUsage: []cost.Usage{
				{RateID: "s3.get_standard", Unit: "Requests", Quantity: "5"},
			},
			ColdLogicalDeliveredByClass: map[storage.StorageClass]uint64{
				storage.Glacier: sixtyFourMiB,
			},
		},
	}

	resMatched, err := ComparePolicies(context.Background(), traceMatched, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resMatched.Policies[3].Report.CompleteTotalUSD == nil {
		t.Fatal("expected warm_glacier_flexible CompleteTotalUSD to be non-nil in positive matched case")
	}
}

// Item 2 [P2]: Direct S3 baseline requires event-scoped complete DirectUsage.
func TestAuditItem2_DirectS3BaselineEventScopedDirectUsage(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")
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
	baseUsage := []cost.Usage{
		{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
		{RateID: "transfer.internet_egress", Unit: "GB", Quantity: "1"},
		{RateID: "ec2.compute", Unit: "Hrs", Quantity: "10"},
		{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "10"},
		{RateID: "alb.hour", Unit: "Hrs", Quantity: "10"},
		{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "10"},
		{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
		{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "10"},
	}

	// Case A: 2 write events, each has 1 DSG backend PUT. Only event 1 has DirectUsage PUT=1.
	// Direct S3 baseline must fail-closed: CompleteTotalUSD == nil.
	tracePartial := baseTrace
	tracePartial.Name = "direct-partial"
	tracePartial.DurationNS = 30 * dayNS
	tracePartial.Omissions = []string{"Full verified observation run across all topology dimensions"}
	tracePartial.Events = []Event{
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
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "1"},
			},
		},
		{
			AtNS: 10 * dayNS,
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "1"},
			},
			// Missing DirectUsage for event 2!
		},
	}

	resPartial, err := ComparePolicies(context.Background(), tracePartial, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resPartial.Policies[0].Report.CompleteTotalUSD != nil {
		t.Fatalf("expected Direct S3 CompleteTotalUSD to be nil on partial DirectUsage, got %s", *resPartial.Policies[0].Report.CompleteTotalUSD)
	}

	// Case B: Both events have DirectUsage PUT=1.
	traceComplete := tracePartial
	traceComplete.Events[1].DirectUsage = []cost.Usage{
		{RateID: "s3.put_standard", Unit: "Requests", Quantity: "1"},
	}
	resComplete, err := ComparePolicies(context.Background(), traceComplete, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resComplete.Policies[0].Report.CompleteTotalUSD == nil {
		t.Fatal("expected Direct S3 CompleteTotalUSD to be non-nil when DirectUsage is complete across events")
	}

	// Case C: Event 2 has client download traffic but lacks Direct GET.
	traceMissingGET := traceComplete
	traceMissingGET.Events[1].Traffic = map[cost.Route]uint64{
		cost.GatewayClient: 1024,
	}
	resMissingGET, err := ComparePolicies(context.Background(), traceMissingGET, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resMissingGET.Policies[0].Report.CompleteTotalUSD != nil {
		t.Fatal("expected Direct S3 CompleteTotalUSD to be nil when download event lacks direct GET telemetry")
	}
}

// Item 3 [P2]: trace.Omissions operates fail-closed on unknown free text.
func TestAuditItem3_TraceOmissionsFailClosedEndToEnd(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")
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
	baseUsage := []cost.Usage{
		{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
		{RateID: "transfer.internet_egress", Unit: "GB", Quantity: "1"},
		{RateID: "ec2.compute", Unit: "Hrs", Quantity: "10"},
		{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "10"},
		{RateID: "alb.hour", Unit: "Hrs", Quantity: "10"},
		{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "10"},
		{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
		{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "10"},
	}

	makeTraceWithOmission := func(om string) Trace {
		tr := baseTrace
		tr.Name = "omission-test"
		tr.DurationNS = 30 * dayNS
		tr.Omissions = []string{om}
		tr.Events = []Event{
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
				DirectUsage: []cost.Usage{
					{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				},
			},
		}
		return tr
	}

	// Repro 1: "Unobserved client billing routes"
	tr1 := makeTraceWithOmission("Unobserved client billing routes")
	res1, err := ComparePolicies(context.Background(), tr1, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if res1.Policies[0].Report.CompleteTotalUSD != nil || res1.Policies[1].Report.CompleteTotalUSD != nil {
		t.Fatal("expected nil CompleteTotalUSD for unknown omission 'Unobserved client billing routes'")
	}

	// Repro 2: "Missing measured cold GET operations"
	tr2 := makeTraceWithOmission("Missing measured cold GET operations")
	res2, err := ComparePolicies(context.Background(), tr2, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if res2.Policies[0].Report.CompleteTotalUSD != nil {
		t.Fatal("expected nil CompleteTotalUSD for 'Missing measured cold GET operations'")
	}

	// Repro 3: "Partial observations unavailable"
	tr3 := makeTraceWithOmission("Partial observations unavailable")
	res3, err := ComparePolicies(context.Background(), tr3, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if res3.Policies[0].Report.CompleteTotalUSD != nil {
		t.Fatal("expected nil CompleteTotalUSD for 'Partial observations unavailable'")
	}

	// Typed severity info schema passes
	tr4 := makeTraceWithOmission("omission:info: observation window aligned with billing cycle")
	res4, err := ComparePolicies(context.Background(), tr4, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if res4.Policies[0].Report.CompleteTotalUSD == nil || res4.Policies[1].Report.CompleteTotalUSD == nil {
		t.Fatal("expected non-nil CompleteTotalUSD for typed info omission")
	}
}

// Item 5 [P3]: Unified RestoreTracker state machine parity across replay and metrics.
func TestAuditItem5_RestoreStateMachineUnifiedParity(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")
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
		case "s3.restore_standard_gb":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.03"}}
		}
	}

	// Parity Case 1: Same-pack re-restore creates 2 distinct lifecycles without overwriting.
	tracker := NewRestoreTracker()
	ev1 := Event{AtNS: 0}
	p1, ok1 := tracker.RegisterRestore(0, ev1, baseTrace, 1024, 1024)
	if !ok1 || p1 == nil {
		t.Fatal("failed to register initial restore")
	}
	tracker.ExpirePastDeadlines(48 * int64(time.Hour))
	ev2 := Event{AtNS: 48 * int64(time.Hour)}
	p2, ok2 := tracker.RegisterRestore(1, ev2, baseTrace, 1024, 1024)
	if !ok2 || p2 == nil {
		t.Fatal("failed to register re-restore")
	}
	if len(tracker.AllLifecycles) != 2 {
		t.Fatalf("expected 2 distinct restore lifecycles, got %d", len(tracker.AllLifecycles))
	}
	if tracker.AllLifecycles[0].StartNS != 0 || tracker.AllLifecycles[1].StartNS != 48*int64(time.Hour) {
		t.Fatalf("lifecycle start times corrupt: %d, %d", tracker.AllLifecycles[0].StartNS, tracker.AllLifecycles[1].StartNS)
	}

	// Parity Case 2: Multi-pack anonymous restores given unique IDs.
	evAnon := Event{AtNS: 0}
	pAnon1, okA1 := tracker.RegisterRestore(2, evAnon, baseTrace, 512, 512)
	pAnon2, okA2 := tracker.RegisterRestore(2, evAnon, baseTrace, 512, 512)
	if !okA1 || !okA2 || pAnon1.PackRef == pAnon2.PackRef {
		t.Fatalf("expected unique anonymous pack refs, got %v and %v", pAnon1.PackRef, pAnon2.PackRef)
	}

	// Parity Case 3: Same-event multiple restore usages on same pack.
	packRefX := storage.PackRef{ID: "pack-x", Generation: "g1"}
	evSame := Event{
		AtNS: 100 * dayNS,
		Tiering: &Tiering{
			Pack: storage.PackInfo{Ref: packRefX},
		},
	}
	pX1, _ := tracker.RegisterRestore(3, evSame, baseTrace, 100, 100)
	pX2, _ := tracker.RegisterRestore(3, evSame, baseTrace, 200, 200)
	if pX1 != pX2 || pX1.Bytes != 300 {
		t.Fatalf("expected aggregated bytes 300, got %d", pX1.Bytes)
	}

	// Parity Case 4: Cross-event restore extension while active.
	evActiveExt := Event{
		AtNS: 101 * dayNS,
		Tiering: &Tiering{
			Pack: storage.PackInfo{
				Ref:           packRefX,
				RestoredUntil: baseTrace.Start.Add(time.Duration(105 * dayNS)),
			},
		},
	}
	pX3, _ := tracker.RegisterRestore(4, evActiveExt, baseTrace, 50, 0)
	if pX3.PackRef != packRefX {
		t.Fatalf("expected extended same pack, got %v", pX3.PackRef)
	}

	// Parity Case 5: Expired reactivation rejected.
	tracker.ExpirePastDeadlines(200 * dayNS)
	evExpiredMetadata := Event{
		AtNS: 200 * dayNS,
		Tiering: &Tiering{
			Pack: storage.PackInfo{
				Ref:           packRefX,
				RestoredUntil: baseTrace.Start.Add(time.Duration(250 * dayNS)),
			},
		},
	}
	tracker.ApplyAuthoritativeMetadata(evExpiredMetadata, baseTrace, map[storage.PackRef]bool{})
	if !tracker.HasUnprovenCold || !tracker.UnprovenReasons[UnprovenColdReactivation] {
		t.Fatal("expected UnprovenColdReactivation on expired restore reactivation attempt")
	}
}
