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

	// 1. Direct S3 Usage
	directUsage := buildUsage(replayRes.directTotals, func(u cost.Usage) bool {
		return u.RateID == "s3.put_standard" || u.RateID == "s3.get_standard" || u.RateID == "transfer.internet_egress"
	})
	directReport, err := cost.Evaluate(catalog, directUsage)
	if err != nil {
		return policyReports{}, fmt.Errorf("evaluating direct_s3: %w", err)
	}

	// 2. WarmStandardOnly Usage
	warmStdUsage := buildUsage(replayRes.warmStdTotals, func(u cost.Usage) bool {
		return u.RateID != "s3.transition_ir" && u.RateID != "s3.transition_glacier" &&
			u.RateID != "s3.retrieval_ir" && u.RateID != "s3.restore_standard_requests" &&
			u.RateID != "s3.restore_standard_gb"
	})
	warmStdReport, err := cost.Evaluate(catalog, warmStdUsage)
	if err != nil {
		return policyReports{}, fmt.Errorf("evaluating warm_standard_only: %w", err)
	}

	// 3. WarmGlacierIR Usage
	glacierIRUsage := buildUsage(replayRes.glacierIRTotals, func(u cost.Usage) bool {
		return u.RateID != "s3.transition_glacier" && u.RateID != "s3.restore_standard_requests" &&
			u.RateID != "s3.restore_standard_gb"
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
		return u.RateID != "s3.transition_ir" && u.RateID != "s3.retrieval_ir"
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
