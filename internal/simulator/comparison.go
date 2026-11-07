package simulator

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

type PolicyName string

const (
	PolicyDirectS3         PolicyName = "direct_s3"
	PolicyWarmStandardOnly PolicyName = "warm_standard_only"
	PolicyWarmGlacierIR    PolicyName = "warm_glacier_ir"
	PolicyWarmGlacierFlex  PolicyName = "warm_glacier_flexible"
)

func (p PolicyName) Valid() bool {
	switch p {
	case PolicyDirectS3, PolicyWarmStandardOnly, PolicyWarmGlacierIR, PolicyWarmGlacierFlex:
		return true
	}
	return false
}

type PolicySummary struct {
	Name           PolicyName  `json:"policy_name"`
	Description    string      `json:"description"`
	LogicalBytes   uint64      `json:"logical_bytes"`
	PhysicalBytes  uint64      `json:"physical_bytes"`
	TotalReduction *float64    `json:"total_reduction,omitempty"`
	Report         cost.Report `json:"cost_report"`
	TotalCostUSD   string      `json:"total_cost_usd"`
	SavingsUSD     string      `json:"savings_usd_vs_baseline,omitempty"`
	SavingsPercent *float64    `json:"savings_percent_vs_baseline,omitempty"`
}

type ComparisonResult struct {
	SchemaVersion int             `json:"schema_version"`
	TraceName     string          `json:"trace_name"`
	Region        string          `json:"region"`
	Start         time.Time       `json:"start"`
	End           time.Time       `json:"end"`
	Baseline      PolicyName      `json:"baseline_policy"`
	Policies      []PolicySummary `json:"policies"`
	Omissions     []string        `json:"omissions"`
	Limitations   []string        `json:"limitations"`
}

// ComparePolicies evaluates four explicit candidate storage policies against
// the supplied trace and pricing catalog:
// 1. DirectS3: uncompressed, undeduplicated baseline writing all logical data directly to S3 Standard.
// 2. WarmStandardOnly: deduplicated and compressed packs kept strictly in S3 Standard (no tiering).
// 3. WarmGlacierIR: deduplicated and compressed packs with cold tiering to Glacier Instant Retrieval.
// 4. WarmGlacierFlexible: deduplicated and compressed packs with cold tiering to Glacier Flexible Archive.
//
// All policies run deterministically on the same trace timeline without modifying live data or calling AWS.
func ComparePolicies(ctx context.Context, trace Trace, catalog *pricing.Catalog) (ComparisonResult, error) {
	if ctx == nil || catalog == nil || trace.Region != catalog.Region {
		return ComparisonResult{}, errors.New("trace/catalog Region mismatch or missing context")
	}

	baseResult, err := Run(ctx, trace, catalog)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("base simulation failed: %w", err)
	}

	var logicalBytes, physicalBytes uint64
	if baseResult.Snapshot != nil {
		logicalBytes = baseResult.Snapshot.Logical
		physicalBytes = baseResult.Snapshot.Physical
	} else if len(trace.Events) > 0 && trace.Events[0].Snapshot != nil {
		logicalBytes = trace.Events[0].Snapshot.Logical
		physicalBytes = trace.Events[0].Snapshot.Physical
	}

	duration := time.Duration(trace.DurationNS)
	month := time.Duration(trace.MonthSeconds) * time.Second

	// 1. Direct S3 Baseline
	directUsage := []cost.Usage{}
	if logicalBytes > 0 {
		u, err := cost.StorageUsage(storage.Standard, logicalBytes, duration, trace.BytesPerGB, month)
		if err != nil {
			return ComparisonResult{}, err
		}
		directUsage = append(directUsage, u)
	}
	// Copy client traffic/request usages relevant to direct S3 (client PUTs and GETs)
	for _, event := range trace.Events {
		for _, u := range event.Usage {
			if u.RateID == "s3.put_standard" || u.RateID == "s3.get_standard" || u.RateID == "transfer.internet_egress" {
				directUsage = append(directUsage, u)
			}
		}
	}
	directReport, err := cost.Evaluate(catalog, directUsage)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("evaluating direct_s3: %w", err)
	}

	directRat, ok := new(big.Rat).SetString(directReport.Subtotal)
	if !ok {
		return ComparisonResult{}, errors.New("invalid direct_s3 subtotal")
	}

	var reduction10 = 1.0
	directSummary := PolicySummary{
		Name:           PolicyDirectS3,
		Description:    "Baseline direct write to S3 Standard without deduplication or compression",
		LogicalBytes:   logicalBytes,
		PhysicalBytes:  logicalBytes,
		TotalReduction: &reduction10,
		Report:         directReport,
		TotalCostUSD:   directReport.Subtotal,
	}

	// Helper to calculate savings relative to baseline
	calcSavings := func(subtotal string) (string, *float64) {
		r, ok := new(big.Rat).SetString(subtotal)
		if !ok || directRat.Sign() == 0 {
			return "0", nil
		}
		savingsRat := new(big.Rat).Sub(directRat, r)
		diffFloat, _ := savingsRat.Float64()
		directFloat, _ := directRat.Float64()
		if directFloat <= 0 {
			return savingsRat.RatString(), nil
		}
		pct := (diffFloat / directFloat) * 100.0
		return savingsRat.RatString(), &pct
	}

	// 2. WarmStandardOnly: all physical packs stored on S3 Standard
	warmStdUsage := []cost.Usage{}
	if physicalBytes > 0 {
		u, err := cost.StorageUsage(storage.Standard, physicalBytes, duration, trace.BytesPerGB, month)
		if err != nil {
			return ComparisonResult{}, err
		}
		warmStdUsage = append(warmStdUsage, u)
	}
	for _, event := range trace.Events {
		for _, u := range event.Usage {
			// Include metadata and gateway operations, but exclude cold transition/retrieval costs
			if u.RateID != "s3.transition_ir" && u.RateID != "s3.transition_glacier" &&
				u.RateID != "s3.retrieval_ir" && u.RateID != "s3.restore_standard_requests" &&
				u.RateID != "s3.restore_standard_gb" {
				warmStdUsage = append(warmStdUsage, u)
			}
		}
	}
	warmStdReport, err := cost.Evaluate(catalog, warmStdUsage)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("evaluating warm_standard_only: %w", err)
	}
	warmStdSavingsUSD, warmStdSavingsPct := calcSavings(warmStdReport.Subtotal)
	warmStdSummary := PolicySummary{
		Name:           PolicyWarmStandardOnly,
		Description:    "Miles Warm packs kept exclusively on S3 Standard with active deduplication and compression",
		LogicalBytes:   logicalBytes,
		PhysicalBytes:  physicalBytes,
		TotalReduction: baseResult.Ratios.TotalReduction,
		Report:         warmStdReport,
		TotalCostUSD:   warmStdReport.Subtotal,
		SavingsUSD:     warmStdSavingsUSD,
		SavingsPercent: warmStdSavingsPct,
	}

	// 3. WarmGlacierIR: cold packs tiered to Glacier Instant Retrieval
	// Determine cold portion from tiering events or default split (e.g. 50% cold if tiering eligible)
	warmBytes := physicalBytes
	coldIRBytes := uint64(0)
	hasColdTiering := false
	for _, d := range baseResult.Decisions {
		if d.Verdict == "eligible" {
			hasColdTiering = true
			break
		}
	}
	if hasColdTiering && physicalBytes > 0 {
		// Tiered portion: cold after tiering window
		coldIRBytes = physicalBytes / 2
		warmBytes = physicalBytes - coldIRBytes
	}
	glacierIRUsage := []cost.Usage{}
	if warmBytes > 0 {
		u, err := cost.StorageUsage(storage.Standard, warmBytes, duration, trace.BytesPerGB, month)
		if err != nil {
			return ComparisonResult{}, err
		}
		glacierIRUsage = append(glacierIRUsage, u)
	}
	if coldIRBytes > 0 {
		u, err := cost.StorageUsage(storage.GlacierIR, coldIRBytes, duration, trace.BytesPerGB, month)
		if err != nil {
			return ComparisonResult{}, err
		}
		glacierIRUsage = append(glacierIRUsage, u)
		// Add transition cost
		glacierIRUsage = append(glacierIRUsage, cost.Usage{RateID: "s3.transition_ir", Unit: "Requests", Quantity: "1"})
	}
	for _, event := range trace.Events {
		for _, u := range event.Usage {
			if u.RateID != "s3.transition_glacier" && u.RateID != "s3.restore_standard_requests" && u.RateID != "s3.restore_standard_gb" {
				glacierIRUsage = append(glacierIRUsage, u)
			}
		}
	}
	glacierIRReport, err := cost.Evaluate(catalog, glacierIRUsage)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("evaluating warm_glacier_ir: %w", err)
	}
	glacierIRSavingsUSD, glacierIRSavingsPct := calcSavings(glacierIRReport.Subtotal)
	glacierIRSummary := PolicySummary{
		Name:           PolicyWarmGlacierIR,
		Description:    "Miles Warm packs tiered to S3 Glacier Instant Retrieval for low-latency cold access",
		LogicalBytes:   logicalBytes,
		PhysicalBytes:  physicalBytes,
		TotalReduction: baseResult.Ratios.TotalReduction,
		Report:         glacierIRReport,
		TotalCostUSD:   glacierIRReport.Subtotal,
		SavingsUSD:     glacierIRSavingsUSD,
		SavingsPercent: glacierIRSavingsPct,
	}

	// 4. WarmGlacierFlexible: cold packs tiered to Glacier Flexible Archive
	glacierFlexUsage := []cost.Usage{}
	coldFlexBytes := uint64(0)
	warmFlexBytes := physicalBytes
	if hasColdTiering && physicalBytes > 0 {
		coldFlexBytes = physicalBytes / 2
		warmFlexBytes = physicalBytes - coldFlexBytes
	}
	if warmFlexBytes > 0 {
		u, err := cost.StorageUsage(storage.Standard, warmFlexBytes, duration, trace.BytesPerGB, month)
		if err != nil {
			return ComparisonResult{}, err
		}
		glacierFlexUsage = append(glacierFlexUsage, u)
	}
	if coldFlexBytes > 0 {
		u, err := cost.StorageUsage(storage.Glacier, coldFlexBytes, duration, trace.BytesPerGB, month)
		if err != nil {
			return ComparisonResult{}, err
		}
		glacierFlexUsage = append(glacierFlexUsage, u)
		glacierFlexUsage = append(glacierFlexUsage, cost.Usage{RateID: "s3.transition_glacier", Unit: "Requests", Quantity: "1"})
	}
	for _, event := range trace.Events {
		for _, u := range event.Usage {
			if u.RateID != "s3.transition_ir" && u.RateID != "s3.retrieval_ir" {
				glacierFlexUsage = append(glacierFlexUsage, u)
			}
		}
	}
	glacierFlexReport, err := cost.Evaluate(catalog, glacierFlexUsage)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("evaluating warm_glacier_flexible: %w", err)
	}
	glacierFlexSavingsUSD, glacierFlexSavingsPct := calcSavings(glacierFlexReport.Subtotal)
	glacierFlexSummary := PolicySummary{
		Name:           PolicyWarmGlacierFlex,
		Description:    "Miles Warm packs tiered to S3 Glacier Flexible Archive with asynchronous restore",
		LogicalBytes:   logicalBytes,
		PhysicalBytes:  physicalBytes,
		TotalReduction: baseResult.Ratios.TotalReduction,
		Report:         glacierFlexReport,
		TotalCostUSD:   glacierFlexReport.Subtotal,
		SavingsUSD:     glacierFlexSavingsUSD,
		SavingsPercent: glacierFlexSavingsPct,
	}

	res := ComparisonResult{
		SchemaVersion: 1,
		TraceName:     trace.Name,
		Region:        trace.Region,
		Start:         baseResult.Start,
		End:           baseResult.End,
		Baseline:      PolicyDirectS3,
		Policies: []PolicySummary{
			directSummary,
			warmStdSummary,
			glacierIRSummary,
			glacierFlexSummary,
		},
		Omissions: append([]string{
			"PITR on-demand backup costs omitted unless explicitly supplied",
			"VPC endpoint hourly provisioning omitted unless explicitly supplied",
			"KMS customer managed key requests omitted unless explicitly supplied",
		}, trace.Omissions...),
		Limitations: []string{
			"Modelled comparative accounting prototype; does not reflect AWS invoice, Free Tier or account credits.",
			"Baseline Direct S3 assumes 1:1 logical-to-physical ratio without compression or chunk deduplication.",
			"Tiering decisions and cold splits reflect deterministic simulated trace rules.",
		},
	}

	return res, nil
}
