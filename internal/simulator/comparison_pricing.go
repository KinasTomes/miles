package simulator

import (
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
)

type policyReports struct {
	directReport      cost.Report
	warmStdReport     cost.Report
	glacierIRReport   cost.Report
	glacierFlexReport cost.Report

	directBreakdown      PolicyCostBreakdown
	warmStdBreakdown     PolicyCostBreakdown
	glacierIRBreakdown   PolicyCostBreakdown
	glacierFlexBreakdown PolicyCostBreakdown
}

// isAllowedUsageForPolicy implements the authoritative rate ID matrix across all four simulated policies:
// Direct S3, Warm Standard Only, Warm Glacier Instant Retrieval, and Warm Glacier Flexible.
// Each policy strictly admits only its own valid storage, request, retrieval, restore, and transfer operations.
func isAllowedUsageForPolicy(polName PolicyName, u cost.Usage) bool {
	// Storage usage (s3.storage.*) is calculated during timeline replay and added via replay totals;
	// never admit raw storage usage from event trace.
	if strings.HasPrefix(u.RateID, "s3.storage.") {
		return false
	}

	// 1. Direct S3 Baseline: Pure S3 Standard direct topology (no gateway infra, no Glacier/IR/IA).
	if polName == PolicyDirectS3 {
		return u.RateID == "s3.put_standard" || u.RateID == "s3.get_standard" ||
			strings.HasPrefix(u.RateID, "transfer.")
	}

	// For candidate policies (all using Dedup Storage Gateway), shared non-S3 operational usage
	// (network transfer and gateway infrastructure) is admitted:
	if strings.HasPrefix(u.RateID, "transfer.") ||
		strings.HasPrefix(u.RateID, "ec2.") || strings.HasPrefix(u.RateID, "ebs.") ||
		strings.HasPrefix(u.RateID, "alb.") || strings.HasPrefix(u.RateID, "ddb.") ||
		strings.HasPrefix(u.RateID, "kms.") || strings.HasPrefix(u.RateID, "cw.") ||
		strings.HasPrefix(u.RateID, "secret.") || strings.HasPrefix(u.RateID, "ssm.") ||
		strings.HasPrefix(u.RateID, "edge.") || strings.HasPrefix(u.RateID, "network.") {
		return true
	}

	// S3 Standard operational requests are common to all 3 candidate DSG policies (for warm packs):
	if u.RateID == "s3.put_standard" || u.RateID == "s3.get_standard" {
		return true
	}

	switch polName {
	case PolicyWarmStandardOnly:
		// Warm Standard Only keeps ALL packs on Standard.
		// Excludes all Glacier IR, Glacier Flexible, and IA operations.
		return false

	case PolicyWarmGlacierIR:
		// Warm Glacier Instant Retrieval admits only Glacier IR operational rates:
		// s3.put_ir, s3.get_ir, s3.retrieval_ir, s3.transition_ir.
		// Excludes all Glacier Flexible rates (s3.put_glacier, s3.transition_glacier, s3.restore_*).
		return u.RateID == "s3.put_ir" || u.RateID == "s3.get_ir" ||
			u.RateID == "s3.retrieval_ir" || u.RateID == "s3.transition_ir"

	case PolicyWarmGlacierFlex:
		// Warm Glacier Flexible admits only Glacier Flexible operational rates:
		// s3.put_glacier, s3.transition_glacier, and restore rates (s3.restore_*).
		// Excludes all Glacier IR rates (s3.put_ir, s3.get_ir, s3.retrieval_ir, s3.transition_ir).
		return u.RateID == "s3.put_glacier" || u.RateID == "s3.transition_glacier" ||
			strings.HasPrefix(u.RateID, "s3.restore_")

	default:
		return false
	}
}

func evaluatePolicyReports(trace Trace, catalog *pricing.Catalog, replayRes timelineReplayResult, baseResult Result) (policyReports, error) {
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

	// Check if the trace provides explicit independent client request telemetry for Direct S3 baseline.
	hasExplicitDirectUsage := false
	for _, ev := range trace.Events {
		if len(ev.DirectUsage) > 0 {
			hasExplicitDirectUsage = true
			break
		}
	}

	// 1. Direct S3 Usage
	// Baseline Direct S3 represents direct client writes to S3 Standard without Dedup Storage Gateway.
	// Dedicated client request telemetry (DirectUsage) must be supplied independently;
	// Direct S3 must never conflate DSG backend pack + sidecar upload requests with client logical operations.
	directUsage := []cost.Usage{}
	directIDs := make([]string, 0, len(replayRes.directTotals))
	for id := range replayRes.directTotals {
		directIDs = append(directIDs, id)
	}
	sort.Strings(directIDs)
	for _, id := range directIDs {
		directUsage = append(directUsage, cost.Usage{RateID: id, Unit: "GB-Mo", Quantity: replayRes.directTotals[id].RatString()})
	}
	if hasExplicitDirectUsage {
		for _, ev := range trace.Events {
			for _, u := range ev.DirectUsage {
				if isAllowedUsageForPolicy(PolicyDirectS3, u) {
					directUsage = append(directUsage, u)
				}
			}
		}
	}
	// Include payload transfer for Direct S3 if present in trace events (unless already supplied in DirectUsage)
	for _, ev := range trace.Events {
		for _, u := range ev.Usage {
			if strings.HasPrefix(u.RateID, "transfer.") {
				already := false
				for _, du := range ev.DirectUsage {
					if du.RateID == u.RateID {
						already = true
						break
					}
				}
				if !already {
					directUsage = append(directUsage, u)
				}
			}
		}
	}
	directReport, err := cost.Evaluate(catalog, directUsage)
	if err != nil {
		return policyReports{}, fmt.Errorf("evaluating direct_s3: %w", err)
	}

	// 2. WarmStandardOnly Usage
	warmStdUsage := buildUsage(replayRes.warmStdTotals, func(u cost.Usage) bool {
		return isAllowedUsageForPolicy(PolicyWarmStandardOnly, u)
	})
	warmStdReport, err := cost.Evaluate(catalog, warmStdUsage)
	if err != nil {
		return policyReports{}, fmt.Errorf("evaluating warm_standard_only: %w", err)
	}

	// 3. WarmGlacierIR Usage
	glacierIRUsage := buildUsage(replayRes.glacierIRTotals, func(u cost.Usage) bool {
		return isAllowedUsageForPolicy(PolicyWarmGlacierIR, u)
	})
	for _, u := range replayRes.earlyDeletionUsageByPolicy[PolicyWarmGlacierIR] {
		glacierIRUsage = append(glacierIRUsage, u)
	}
	glacierIRReport, err := cost.Evaluate(catalog, glacierIRUsage)
	if err != nil {
		return policyReports{}, fmt.Errorf("evaluating warm_glacier_ir: %w", err)
	}

	// 4. WarmGlacierFlexible Usage
	glacierFlexUsage := buildUsage(replayRes.glacierFlexTotals, func(u cost.Usage) bool {
		return isAllowedUsageForPolicy(PolicyWarmGlacierFlex, u)
	})
	for _, u := range replayRes.earlyDeletionUsageByPolicy[PolicyWarmGlacierFlex] {
		glacierFlexUsage = append(glacierFlexUsage, u)
	}
	glacierFlexReport, err := cost.Evaluate(catalog, glacierFlexUsage)
	if err != nil {
		return policyReports{}, fmt.Errorf("evaluating warm_glacier_flexible: %w", err)
	}

	directTraffic := make(map[cost.Route]uint64)
	if b, ok := baseResult.Traffic[cost.ClientGateway]; ok {
		directTraffic[cost.ClientS3] = b
	} else if b, ok := baseResult.Traffic[cost.ClientS3]; ok {
		directTraffic[cost.ClientS3] = b
	}
	if b, ok := baseResult.Traffic[cost.GatewayClient]; ok {
		directTraffic[cost.S3Client] = b
	} else if b, ok := baseResult.Traffic[cost.S3Client]; ok {
		directTraffic[cost.S3Client] = b
	}

	directBreakdown := categorizeReport(directReport, directTraffic, replayRes.earlyDeletionUSDByPolicy[PolicyDirectS3].RatString())
	warmStdBreakdown := categorizeReport(warmStdReport, baseResult.Traffic, replayRes.earlyDeletionUSDByPolicy[PolicyWarmStandardOnly].RatString())
	glacierIRBreakdown := categorizeReport(glacierIRReport, baseResult.Traffic, replayRes.earlyDeletionUSDByPolicy[PolicyWarmGlacierIR].RatString())
	glacierFlexBreakdown := categorizeReport(glacierFlexReport, baseResult.Traffic, replayRes.earlyDeletionUSDByPolicy[PolicyWarmGlacierFlex].RatString())

	return policyReports{
		directReport:         directReport,
		warmStdReport:        warmStdReport,
		glacierIRReport:      glacierIRReport,
		glacierFlexReport:    glacierFlexReport,
		directBreakdown:      directBreakdown,
		warmStdBreakdown:     warmStdBreakdown,
		glacierIRBreakdown:   glacierIRBreakdown,
		glacierFlexBreakdown: glacierFlexBreakdown,
	}, nil
}

func categorizeReport(r cost.Report, traffic map[cost.Route]uint64, earlyDeletionUSD string) PolicyCostBreakdown {
	var storageRat, reqRat, retRat, transRat, restRat, earlyRat, netRat, infraRat big.Rat
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
			transRat.Add(&transRat, val)
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
		case strings.HasPrefix(l.RateID, "transfer."):
			netRat.Add(&netRat, val)
		case strings.HasPrefix(l.RateID, "ec2.") || strings.HasPrefix(l.RateID, "ebs.") ||
			strings.HasPrefix(l.RateID, "alb.") || strings.HasPrefix(l.RateID, "ddb.") ||
			strings.HasPrefix(l.RateID, "kms.") || strings.HasPrefix(l.RateID, "cw.") ||
			strings.HasPrefix(l.RateID, "secret.") || strings.HasPrefix(l.RateID, "ssm.") ||
			strings.HasPrefix(l.RateID, "edge.") || strings.HasPrefix(l.RateID, "network."):
			infraRat.Add(&infraRat, val)
		}
	}

	if earlyRatVal, ok := new(big.Rat).SetString(earlyDeletionUSD); ok && earlyRatVal.Sign() > 0 {
		earlyRat.Add(&earlyRat, earlyRatVal)
		storageRat.Sub(&storageRat, earlyRatVal)
	}

	trafficMap := map[string]uint64{}
	for route, b := range traffic {
		trafficMap[string(route)] = b
	}

	return PolicyCostBreakdown{
		StorageUSD:        storageRat.RatString(),
		RequestsUSD:       reqRat.RatString(),
		RetrievalUSD:      retRat.RatString(),
		TransitionUSD:     transRat.RatString(),
		RestoreUSD:        restRat.RatString(),
		EarlyDeletionUSD:  earlyRat.RatString(),
		NetworkUSD:        netRat.RatString(),
		InfrastructureUSD: infraRat.RatString(),
		RequestsByType:    reqByType,
		TrafficBytes:      trafficMap,
	}
}
