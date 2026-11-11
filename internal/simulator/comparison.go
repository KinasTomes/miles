package simulator

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
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

type PolicyCostBreakdown struct {
	StorageUSD        string            `json:"storage_usd"`
	RequestsUSD       string            `json:"requests_usd"`
	RetrievalUSD      string            `json:"retrieval_usd"`
	TransitionUSD     string            `json:"transition_usd"`
	RestoreUSD        string            `json:"restore_usd"`
	EarlyDeletionUSD  string            `json:"early_deletion_usd"`
	NetworkUSD        string            `json:"network_usd"`
	InfrastructureUSD string            `json:"infrastructure_usd"`
	RequestsByType    map[string]uint64 `json:"requests_by_type,omitempty"`
	TrafficBytes      map[string]uint64 `json:"traffic_bytes,omitempty"`
}

type PolicySummary struct {
	Name                     PolicyName           `json:"policy_name"`
	Description              string               `json:"description"`
	LogicalBytes             uint64               `json:"logical_bytes"`
	PhysicalBytes            uint64               `json:"physical_bytes"`
	StorageBytesByClass      map[string]uint64    `json:"storage_by_class,omitempty"`
	TemporaryRestoredBytes   uint64               `json:"temporary_restored_bytes,omitempty"`
	TotalReduction           *float64             `json:"total_reduction,omitempty"`
	Report                   cost.Report          `json:"cost_report"`
	Breakdown                *PolicyCostBreakdown `json:"breakdown,omitempty"`
	TotalCostUSD             string               `json:"total_cost_usd"`
	SavingsUSD               *string              `json:"savings_usd_vs_baseline"`
	SavingsPercent           *float64             `json:"savings_percent_vs_baseline"`
	IndicativeStorageDiffUSD *string              `json:"indicative_storage_difference_usd,omitempty"`
	BreakEven                *string              `json:"break_even,omitempty"`
	RestoreAmplification     *float64             `json:"restore_amplification,omitempty"`
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

// ComparePolicies evaluates four candidate storage policies against the supplied
// trace and pricing catalog:
// 1. DirectS3: uncompressed, undeduplicated baseline writing all logical data directly to S3 Standard.
// 2. WarmStandardOnly: deduplicated and compressed packs kept strictly in S3 Standard (no cold tiering).
// 3. WarmGlacierIR: deduplicated and compressed packs with cold packs in Glacier Instant Retrieval.
// 4. WarmGlacierFlexible: deduplicated and compressed packs with cold packs in Glacier Flexible Archive.
//
// All policies replay physical and logical inventory step by step across the trace timeline.
// Tiering transitions and byte-time are integrated only when transitions or cold inventory are
// explicitly observed; no synthetic splits or post-hoc allocations are assumed.
func ComparePolicies(ctx context.Context, trace Trace, catalog *pricing.Catalog) (ComparisonResult, error) {
	if ctx == nil || catalog == nil || trace.Region != catalog.Region {
		return ComparisonResult{}, errors.New("trace/catalog Region mismatch or missing context")
	}

	baseResult, err := Run(ctx, trace, catalog)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("base simulation failed: %w", err)
	}

	replayRes, err := replayTimeline(ctx, trace, catalog, baseResult)
	if err != nil {
		return ComparisonResult{}, err
	}

	reports, err := evaluatePolicyReports(trace, catalog, replayRes, baseResult)
	if err != nil {
		return ComparisonResult{}, err
	}

	applyCompletenessTotals(&reports, trace, replayRes.currentLogical, replayRes.hasStorageInconsistency, replayRes.hasUnprovenColdPacks)

	metrics := computePolicyMetrics(trace, replayRes, reports)

	var reduction10 = 1.0
	directSummary := PolicySummary{
		Name:                PolicyDirectS3,
		Description:         "Baseline direct write to S3 Standard without deduplication or compression",
		LogicalBytes:        replayRes.currentLogical,
		PhysicalBytes:       replayRes.currentLogical,
		StorageBytesByClass: replayRes.directClassMap,
		TotalReduction:      &reduction10,
		Report:              reports.directReport,
		Breakdown:           &reports.directBreakdown,
		TotalCostUSD:        reports.directReport.Subtotal,
	}

	warmStdSummary := PolicySummary{
		Name:                     PolicyWarmStandardOnly,
		Description:              "Miles Warm packs kept exclusively on S3 Standard with active deduplication and compression",
		LogicalBytes:             replayRes.currentLogical,
		PhysicalBytes:            replayRes.finalPhysical,
		StorageBytesByClass:      replayRes.warmStdClassMap,
		TotalReduction:           baseResult.Ratios.TotalReduction,
		Report:                   reports.warmStdReport,
		Breakdown:                &reports.warmStdBreakdown,
		TotalCostUSD:             reports.warmStdReport.Subtotal,
		SavingsUSD:               metrics.warmStdSavingsUSD,
		SavingsPercent:           metrics.warmStdSavingsPct,
		IndicativeStorageDiffUSD: metrics.warmStdStorageDiff,
		BreakEven:                metrics.warmStdBreakEven,
	}

	glacierIRSummary := PolicySummary{
		Name:                     PolicyWarmGlacierIR,
		Description:              "Miles Warm packs tiered to S3 Glacier Instant Retrieval for low-latency cold access",
		LogicalBytes:             replayRes.currentLogical,
		PhysicalBytes:            replayRes.finalPhysical,
		StorageBytesByClass:      replayRes.glacierIRClassMap,
		TotalReduction:           baseResult.Ratios.TotalReduction,
		Report:                   reports.glacierIRReport,
		Breakdown:                &reports.glacierIRBreakdown,
		TotalCostUSD:             reports.glacierIRReport.Subtotal,
		SavingsUSD:               metrics.glacierIRSavingsUSD,
		SavingsPercent:           metrics.glacierIRSavingsPct,
		IndicativeStorageDiffUSD: metrics.glacierIRStorageDiff,
		BreakEven:                metrics.glacierIRBreakEven,
		RestoreAmplification:     metrics.glacierIRRestoreAmp,
	}

	glacierFlexSummary := PolicySummary{
		Name:                     PolicyWarmGlacierFlex,
		Description:              "Miles Warm packs tiered to S3 Glacier Flexible Archive with asynchronous restore",
		LogicalBytes:             replayRes.currentLogical,
		PhysicalBytes:            replayRes.finalPhysical,
		StorageBytesByClass:      replayRes.glacierFlexClassMap,
		TemporaryRestoredBytes:   replayRes.temporaryRestoredBytes,
		TotalReduction:           baseResult.Ratios.TotalReduction,
		Report:                   reports.glacierFlexReport,
		Breakdown:                &reports.glacierFlexBreakdown,
		TotalCostUSD:             reports.glacierFlexReport.Subtotal,
		SavingsUSD:               metrics.glacierFlexSavingsUSD,
		SavingsPercent:           metrics.glacierFlexSavingsPct,
		IndicativeStorageDiffUSD: metrics.glacierFlexStorageDiff,
		BreakEven:                metrics.glacierFlexBreakEven,
		RestoreAmplification:     metrics.glacierFlexRestoreAmp,
	}

	omissions := buildComparisonOmissions(trace, replayRes.hasObservedCold, replayRes.hasStorageInconsistency, replayRes.hasUnprovenColdPacks, metrics.hasUnpricedOmission, metrics.hasIncompleteOmission)

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
		Omissions: omissions,
		Limitations: []string{
			"Modelled comparative accounting prototype; does not reflect AWS invoice, Free Tier or account credits.",
			"Baseline Direct S3 assumes 1:1 logical-to-physical ratio without compression or chunk deduplication.",
			"Tiering decisions and cold splits reflect deterministic simulated trace rules.",
		},
	}

	return res, nil
}
