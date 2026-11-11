package simulator

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
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

type PolicyCostBreakdown struct {
	StorageUSD        string            `json:"storage_usd"`
	RequestsUSD       string            `json:"requests_usd"`
	RetrievalUSD      string            `json:"retrieval_usd"`
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

	var initialLogical, initialPhysical uint64
	if baseResult.Snapshot != nil {
		initialLogical = baseResult.Snapshot.Logical
		initialPhysical = baseResult.Snapshot.Physical
	} else if len(trace.Events) > 0 && trace.Events[0].Snapshot != nil {
		initialLogical = trace.Events[0].Snapshot.Logical
		initialPhysical = trace.Events[0].Snapshot.Physical
	}

	monthDuration := time.Duration(trace.MonthSeconds) * time.Second

	// Accumulators for storage byte-time by policy
	directTotals := map[string]*big.Rat{}
	warmStdTotals := map[string]*big.Rat{}
	glacierIRTotals := map[string]*big.Rat{}
	glacierFlexTotals := map[string]*big.Rat{}

	currentLogical := initialLogical
	currentPhysical := initialPhysical
	currentStorageBytes := map[storage.StorageClass]uint64{}

	hasObservedCold := false
	var previous int64

	addStorageUsage := func(totals map[string]*big.Rat, class storage.StorageClass, bytes uint64, d time.Duration) error {
		if bytes == 0 || d <= 0 {
			return nil
		}
		u, err := cost.StorageUsage(class, bytes, d, trace.BytesPerGB, monthDuration)
		if err != nil {
			return err
		}
		q, ok := new(big.Rat).SetString(u.Quantity)
		if !ok {
			return errors.New("invalid storage usage quantity rational")
		}
		if totals[u.RateID] == nil {
			totals[u.RateID] = new(big.Rat)
		}
		totals[u.RateID].Add(totals[u.RateID], q)
		return nil
	}

	step := func(to int64) error {
		d := time.Duration(to - previous)
		if d <= 0 {
			previous = to
			return nil
		}

		// 1. Direct S3 baseline: stores logical bytes on Standard
		if err := addStorageUsage(directTotals, storage.Standard, currentLogical, d); err != nil {
			return err
		}

		// Calculate current physical split across all valid classes
		totalPhysical := uint64(0)
		standardBytes := uint64(0)
		coldBytes := uint64(0)

		if len(currentStorageBytes) > 0 {
			for class, b := range currentStorageBytes {
				totalPhysical += b
				switch class {
				case storage.Standard, storage.StandardIA:
					standardBytes += b
				case storage.Glacier, storage.GlacierIR:
					coldBytes += b
				}
			}
		} else {
			totalPhysical = currentPhysical
			standardBytes = currentPhysical
			coldBytes = 0
		}

		if coldBytes > 0 {
			hasObservedCold = true
		}

		// 2. WarmStandardOnly: all physical bytes (including IA and Cold) stay on Standard
		if err := addStorageUsage(warmStdTotals, storage.Standard, totalPhysical, d); err != nil {
			return err
		}

		// 3. WarmGlacierIR: standard/IA on Standard, cold on Glacier IR
		// Applies Glacier IR storage rule: 128 KiB minimum billable bytes padding
		irBillableCold := coldBytes
		if rule, ok := catalog.RuleForClass(string(storage.GlacierIR)); ok && coldBytes > 0 {
			adj := cost.ApplyStorageRules(rule, coldBytes, 1)
			irBillableCold = adj.BillableBytes
		}
		if err := addStorageUsage(glacierIRTotals, storage.Standard, standardBytes, d); err != nil {
			return err
		}
		if err := addStorageUsage(glacierIRTotals, storage.GlacierIR, irBillableCold, d); err != nil {
			return err
		}

		// 4. WarmGlacierFlexible: standard/IA on Standard, cold on Glacier
		// Applies Glacier Flexible storage rule: 8 KiB Standard + 32 KiB Glacier metadata per pack
		flexStandardBytes := standardBytes
		flexGlacierBytes := coldBytes
		if rule, ok := catalog.RuleForClass(string(storage.Glacier)); ok && coldBytes > 0 {
			adj := cost.ApplyStorageRules(rule, coldBytes, 1)
			flexGlacierBytes = adj.BillableBytes + adj.MetadataArchiveBytes
			flexStandardBytes += adj.MetadataStandardBytes
		}
		if err := addStorageUsage(glacierFlexTotals, storage.Standard, flexStandardBytes, d); err != nil {
			return err
		}
		if err := addStorageUsage(glacierFlexTotals, storage.Glacier, flexGlacierBytes, d); err != nil {
			return err
		}

		previous = to
		return nil
	}

	// Replay events through timeline
	for _, event := range trace.Events {
		if err := ctx.Err(); err != nil {
			return ComparisonResult{}, err
		}
		if err := step(event.AtNS); err != nil {
			return ComparisonResult{}, err
		}

		if event.Snapshot != nil {
			currentLogical = event.Snapshot.Logical
			currentPhysical = event.Snapshot.Physical
		}
		if event.StorageBytes != nil {
			currentStorageBytes = event.StorageBytes
		}
	}

	if err := ctx.Err(); err != nil {
		return ComparisonResult{}, err
	}
	if err := step(trace.DurationNS); err != nil {
		return ComparisonResult{}, err
	}

	// Track temporary restored Standard storage byte-time during restores in Glacier Flexible
	for _, event := range trace.Events {
		for _, u := range event.Usage {
			if u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb" {
				if q, ok := new(big.Rat).SetString(u.Quantity); ok {
					gbFloat, _ := q.Float64()
					restoredBytes := uint64(gbFloat * float64(trace.BytesPerGB))
					if restoredBytes > 0 {
						tempRestoreDur := 24 * time.Hour // default 1-day temporary Standard copy
						tempU, err := cost.TemporaryRestoredUsage(restoredBytes, tempRestoreDur, trace.BytesPerGB, monthDuration)
						if err == nil {
							if qRat, ok := new(big.Rat).SetString(tempU.Quantity); ok {
								if glacierFlexTotals[tempU.RateID] == nil {
									glacierFlexTotals[tempU.RateID] = new(big.Rat)
								}
								glacierFlexTotals[tempU.RateID].Add(glacierFlexTotals[tempU.RateID], qRat)
							}
						}
					}
				}
			}
		}
	}

	buildUsage := func(totals map[string]*big.Rat, filter func(cost.Usage) bool) []cost.Usage {
		usage := []cost.Usage{}
		ids := make([]string, 0, len(totals))
		for id := range totals {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			usage = append(usage, cost.Usage{RateID: id, Unit: "GB-Mo", Quantity: totals[id].RatString()})
		}
		for _, event := range trace.Events {
			for _, u := range event.Usage {
				if filter(u) {
					usage = append(usage, u)
				}
			}
		}
		return usage
	}

	// 1. Direct S3 Usage
	directUsage := buildUsage(directTotals, func(u cost.Usage) bool {
		return u.RateID == "s3.put_standard" || u.RateID == "s3.get_standard" || u.RateID == "transfer.internet_egress"
	})
	directReport, err := cost.Evaluate(catalog, directUsage)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("evaluating direct_s3: %w", err)
	}

	// 2. WarmStandardOnly Usage
	warmStdUsage := buildUsage(warmStdTotals, func(u cost.Usage) bool {
		return u.RateID != "s3.transition_ir" && u.RateID != "s3.transition_glacier" &&
			u.RateID != "s3.retrieval_ir" && u.RateID != "s3.restore_standard_requests" &&
			u.RateID != "s3.restore_standard_gb"
	})
	warmStdReport, err := cost.Evaluate(catalog, warmStdUsage)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("evaluating warm_standard_only: %w", err)
	}

	// 3. WarmGlacierIR Usage
	glacierIRUsage := buildUsage(glacierIRTotals, func(u cost.Usage) bool {
		return u.RateID != "s3.transition_glacier" && u.RateID != "s3.restore_standard_requests" &&
			u.RateID != "s3.restore_standard_gb"
	})
	glacierIRReport, err := cost.Evaluate(catalog, glacierIRUsage)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("evaluating warm_glacier_ir: %w", err)
	}

	// 4. WarmGlacierFlexible Usage
	glacierFlexUsage := buildUsage(glacierFlexTotals, func(u cost.Usage) bool {
		return u.RateID != "s3.transition_ir" && u.RateID != "s3.retrieval_ir"
	})
	glacierFlexReport, err := cost.Evaluate(catalog, glacierFlexUsage)
	if err != nil {
		return ComparisonResult{}, fmt.Errorf("evaluating warm_glacier_flexible: %w", err)
	}

	isComplete := func(r cost.Report) bool {
		if len(r.Unpriced) > 0 {
			return false
		}
		for _, om := range trace.Omissions {
			lower := strings.ToLower(om)
			if strings.Contains(lower, "omitted") || strings.Contains(lower, "not exercised") ||
				strings.Contains(lower, "not modeled") || strings.Contains(lower, "not modelled") ||
				strings.Contains(lower, "require additional observations") {
				return false
			}
		}
		return true
	}

	if isComplete(directReport) {
		sub := directReport.Subtotal
		directReport.CompleteTotalUSD = &sub
	}
	if isComplete(warmStdReport) {
		sub := warmStdReport.Subtotal
		warmStdReport.CompleteTotalUSD = &sub
	}
	if isComplete(glacierIRReport) {
		sub := glacierIRReport.Subtotal
		glacierIRReport.CompleteTotalUSD = &sub
	}
	if isComplete(glacierFlexReport) {
		sub := glacierFlexReport.Subtotal
		glacierFlexReport.CompleteTotalUSD = &sub
	}

	directBreakdown := categorizeReport(directReport, baseResult.Traffic)
	warmStdBreakdown := categorizeReport(warmStdReport, baseResult.Traffic)
	glacierIRBreakdown := categorizeReport(glacierIRReport, baseResult.Traffic)
	glacierFlexBreakdown := categorizeReport(glacierFlexReport, baseResult.Traffic)

	// Helper to calculate savings vs baseline:
	// Only conclude true net savings if BOTH baseline and candidate policy have CompleteTotalUSD != nil.
	// In partial models, net savings must remain null, but indicative storage difference can be reported.
	hasIncompleteOmission := false
	hasUnpricedOmission := false
	calcSavings := func(polReport cost.Report) (*string, *float64, *string) {
		var indicativeDiff *string
		// Calculate storage-only difference when directReport and candidate policy have storage lines and no unpriced storage
		directStorageRat := new(big.Rat)
		for _, l := range directReport.Lines {
			if strings.HasPrefix(l.RateID, "s3.storage.") {
				if r, ok := new(big.Rat).SetString(l.USD); ok {
					directStorageRat.Add(directStorageRat, r)
				}
			}
		}
		polStorageRat := new(big.Rat)
		for _, l := range polReport.Lines {
			if strings.HasPrefix(l.RateID, "s3.storage.") {
				if r, ok := new(big.Rat).SetString(l.USD); ok {
					polStorageRat.Add(polStorageRat, r)
				}
			}
		}

		hasUnpricedStorage := false
		for _, u := range directReport.Unpriced {
			if strings.HasPrefix(u.RateID, "s3.storage.") {
				hasUnpricedStorage = true
				break
			}
		}
		if !hasUnpricedStorage {
			for _, u := range polReport.Unpriced {
				if strings.HasPrefix(u.RateID, "s3.storage.") {
					hasUnpricedStorage = true
					break
				}
			}
		}

		if !hasUnpricedStorage && currentLogical > 0 && directStorageRat.Sign() > 0 {
			diff := new(big.Rat).Sub(directStorageRat, polStorageRat)
			s := diff.RatString()
			indicativeDiff = &s
		}

		if len(directReport.Unpriced) > 0 || len(polReport.Unpriced) > 0 {
			hasUnpricedOmission = true
		}
		if directReport.CompleteTotalUSD == nil || polReport.CompleteTotalUSD == nil {
			hasIncompleteOmission = true
		}

		if directReport.CompleteTotalUSD == nil || polReport.CompleteTotalUSD == nil ||
			len(directReport.Unpriced) > 0 || len(polReport.Unpriced) > 0 {
			return nil, nil, indicativeDiff
		}

		dRat, okD := new(big.Rat).SetString(*directReport.CompleteTotalUSD)
		pRat, okP := new(big.Rat).SetString(*polReport.CompleteTotalUSD)
		if !okD || !okP || dRat.Sign() == 0 {
			return nil, nil, indicativeDiff
		}
		savingsRat := new(big.Rat).Sub(dRat, pRat)
		diffFloat, _ := savingsRat.Float64()
		directFloat, _ := dRat.Float64()
		if directFloat <= 0 {
			s := savingsRat.RatString()
			return &s, nil, indicativeDiff
		}
		pct := (diffFloat / directFloat) * 100.0
		s := savingsRat.RatString()
		return &s, &pct, indicativeDiff
	}

	calcBreakEven := func(polReport cost.Report, polBreakdown PolicyCostBreakdown) *string {
		dStorageRat, _ := new(big.Rat).SetString(directBreakdown.StorageUSD)
		pStorageRat, _ := new(big.Rat).SetString(polBreakdown.StorageUSD)
		infraRat, _ := new(big.Rat).SetString(polBreakdown.InfrastructureUSD)
		if dStorageRat == nil || pStorageRat == nil || currentLogical == 0 {
			return nil
		}
		storageSaving := new(big.Rat).Sub(dStorageRat, pStorageRat)
		if storageSaving.Sign() <= 0 {
			return nil
		}
		logicalGBRat := new(big.Rat).SetFrac64(int64(currentLogical), int64(trace.BytesPerGB))
		if logicalGBRat.Sign() <= 0 {
			return nil
		}
		unitSaving := new(big.Rat).Quo(storageSaving, logicalGBRat)
		if infraRat != nil && infraRat.Sign() > 0 {
			return cost.BreakEven(infraRat, unitSaving)
		}
		zeroStr := "0 GB (no fixed infrastructure overhead)"
		return &zeroStr
	}

	calcRestoreAmplification := func(polName PolicyName) *float64 {
		if polName != PolicyWarmGlacierIR && polName != PolicyWarmGlacierFlex {
			return nil
		}
		var coldBytesRetrieved uint64
		for _, event := range trace.Events {
			for _, u := range event.Usage {
				if u.RateID == "s3.retrieval_ir" || u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb" {
					if q, ok := new(big.Rat).SetString(u.Quantity); ok {
						gbFloat, _ := q.Float64()
						coldBytesRetrieved += uint64(gbFloat * float64(trace.BytesPerGB))
					}
				}
			}
		}
		if coldBytesRetrieved == 0 {
			return nil
		}
		logicalDelivered := baseResult.Traffic[cost.GatewayClient]
		if logicalDelivered == 0 {
			logicalDelivered = currentLogical
		}
		return cost.RestoreAmplification(coldBytesRetrieved, logicalDelivered)
	}

	warmStdSavingsUSD, warmStdSavingsPct, warmStdStorageDiff := calcSavings(warmStdReport)
	glacierIRSavingsUSD, glacierIRSavingsPct, glacierIRStorageDiff := calcSavings(glacierIRReport)
	glacierFlexSavingsUSD, glacierFlexSavingsPct, glacierFlexStorageDiff := calcSavings(glacierFlexReport)

	var reduction10 = 1.0
	directSummary := PolicySummary{
		Name:           PolicyDirectS3,
		Description:    "Baseline direct write to S3 Standard without deduplication or compression",
		LogicalBytes:   currentLogical,
		PhysicalBytes:  currentLogical,
		TotalReduction: &reduction10,
		Report:         directReport,
		Breakdown:      &directBreakdown,
		TotalCostUSD:   directReport.Subtotal,
	}

	warmStdSummary := PolicySummary{
		Name:                     PolicyWarmStandardOnly,
		Description:              "Miles Warm packs kept exclusively on S3 Standard with active deduplication and compression",
		LogicalBytes:             currentLogical,
		PhysicalBytes:            currentPhysical,
		TotalReduction:           baseResult.Ratios.TotalReduction,
		Report:                   warmStdReport,
		Breakdown:                &warmStdBreakdown,
		TotalCostUSD:             warmStdReport.Subtotal,
		SavingsUSD:               warmStdSavingsUSD,
		SavingsPercent:           warmStdSavingsPct,
		IndicativeStorageDiffUSD: warmStdStorageDiff,
		BreakEven:                calcBreakEven(warmStdReport, warmStdBreakdown),
	}

	glacierIRSummary := PolicySummary{
		Name:                     PolicyWarmGlacierIR,
		Description:              "Miles Warm packs tiered to S3 Glacier Instant Retrieval for low-latency cold access",
		LogicalBytes:             currentLogical,
		PhysicalBytes:            currentPhysical,
		TotalReduction:           baseResult.Ratios.TotalReduction,
		Report:                   glacierIRReport,
		Breakdown:                &glacierIRBreakdown,
		TotalCostUSD:             glacierIRReport.Subtotal,
		SavingsUSD:               glacierIRSavingsUSD,
		SavingsPercent:           glacierIRSavingsPct,
		IndicativeStorageDiffUSD: glacierIRStorageDiff,
		BreakEven:                calcBreakEven(glacierIRReport, glacierIRBreakdown),
		RestoreAmplification:     calcRestoreAmplification(PolicyWarmGlacierIR),
	}

	glacierFlexSummary := PolicySummary{
		Name:                     PolicyWarmGlacierFlex,
		Description:              "Miles Warm packs tiered to S3 Glacier Flexible Archive with asynchronous restore",
		LogicalBytes:             currentLogical,
		PhysicalBytes:            currentPhysical,
		TotalReduction:           baseResult.Ratios.TotalReduction,
		Report:                   glacierFlexReport,
		Breakdown:                &glacierFlexBreakdown,
		TotalCostUSD:             glacierFlexReport.Subtotal,
		SavingsUSD:               glacierFlexSavingsUSD,
		SavingsPercent:           glacierFlexSavingsPct,
		IndicativeStorageDiffUSD: glacierFlexStorageDiff,
		BreakEven:                calcBreakEven(glacierFlexReport, glacierFlexBreakdown),
		RestoreAmplification:     calcRestoreAmplification(PolicyWarmGlacierFlex),
	}

	omissions := append([]string{
		"PITR on-demand backup costs omitted unless explicitly supplied",
		"VPC endpoint hourly provisioning omitted unless explicitly supplied",
		"KMS customer managed key requests omitted unless explicitly supplied",
	}, trace.Omissions...)

	if !hasObservedCold {
		omissions = append(omissions,
			"Trace contains no observed cold inventory or tiering transitions; warm_glacier_ir evaluated with zero cold byte-time",
			"Trace contains no observed cold inventory or tiering transitions; warm_glacier_flexible evaluated with zero cold byte-time",
		)
	}
	if hasUnpricedOmission {
		omissions = append(omissions, "Net savings and savings percent vs baseline omitted: unpriced items present in baseline or candidate policy cost evaluation")
	}
	if hasIncompleteOmission {
		omissions = append(omissions, "Net savings vs baseline omitted: cost evaluation is partial prototype without complete matched totals (CompleteTotalUSD is null)")
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
		Omissions: omissions,
		Limitations: []string{
			"Modelled comparative accounting prototype; does not reflect AWS invoice, Free Tier or account credits.",
			"Baseline Direct S3 assumes 1:1 logical-to-physical ratio without compression or chunk deduplication.",
			"Tiering decisions and cold splits reflect deterministic simulated trace rules.",
		},
	}

	return res, nil
}

func categorizeReport(r cost.Report, traffic map[cost.Route]uint64) PolicyCostBreakdown {
	var storageRat, reqRat, retRat, restRat, earlyRat, netRat, infraRat big.Rat
	reqByType := map[string]uint64{}

	for _, l := range r.Lines {
		val, _ := new(big.Rat).SetString(l.USD)
		if val == nil {
			continue
		}
		switch {
		case strings.HasPrefix(l.RateID, "s3.storage."):
			if strings.Contains(l.RateID, "early_deletion") {
				earlyRat.Add(&earlyRat, val)
			} else {
				storageRat.Add(&storageRat, val)
			}
		case strings.HasPrefix(l.RateID, "s3.put_") || strings.HasPrefix(l.RateID, "s3.get_"):
			reqRat.Add(&reqRat, val)
			if q, ok := new(big.Rat).SetString(l.Quantity); ok {
				qInt := new(big.Int).Quo(q.Num(), q.Denom()).Uint64()
				if strings.HasPrefix(l.RateID, "s3.put_") {
					reqByType["s3_put"] += qInt
				} else {
					reqByType["s3_get"] += qInt
				}
			}
		case strings.HasPrefix(l.RateID, "s3.transition_"):
			reqRat.Add(&reqRat, val)
			if q, ok := new(big.Rat).SetString(l.Quantity); ok {
				qInt := new(big.Int).Quo(q.Num(), q.Denom()).Uint64()
				reqByType["s3_transition"] += qInt
			}
		case strings.HasPrefix(l.RateID, "s3.retrieval_"):
			retRat.Add(&retRat, val)
		case strings.HasPrefix(l.RateID, "s3.restore_"):
			restRat.Add(&restRat, val)
			if strings.Contains(l.RateID, "requests") {
				if q, ok := new(big.Rat).SetString(l.Quantity); ok {
					qInt := new(big.Int).Quo(q.Num(), q.Denom()).Uint64()
					reqByType["s3_restore"] += qInt
				}
			}
		case strings.HasPrefix(l.RateID, "transfer.") || strings.HasPrefix(l.RateID, "network."):
			netRat.Add(&netRat, val)
		case strings.HasPrefix(l.RateID, "ec2.") || strings.HasPrefix(l.RateID, "ebs.") ||
			strings.HasPrefix(l.RateID, "alb.") || strings.HasPrefix(l.RateID, "ddb.") ||
			strings.HasPrefix(l.RateID, "kms.") || strings.HasPrefix(l.RateID, "cw.") ||
			strings.HasPrefix(l.RateID, "secret.") || strings.HasPrefix(l.RateID, "ssm.") ||
			strings.HasPrefix(l.RateID, "edge."):
			infraRat.Add(&infraRat, val)
		}
	}

	trafficMap := map[string]uint64{}
	for route, b := range traffic {
		trafficMap[string(route)] = b
	}

	return PolicyCostBreakdown{
		StorageUSD:        storageRat.RatString(),
		RequestsUSD:       reqRat.RatString(),
		RetrievalUSD:      retRat.RatString(),
		RestoreUSD:        restRat.RatString(),
		EarlyDeletionUSD:  earlyRat.RatString(),
		NetworkUSD:        netRat.RatString(),
		InfrastructureUSD: infraRat.RatString(),
		RequestsByType:    reqByType,
		TrafficBytes:      trafficMap,
	}
}
