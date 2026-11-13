package simulator

import (
	"math"
	"math/big"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/storage"
)

type policyMetricsResult struct {
	warmStdSavingsUSD      *string
	warmStdSavingsPct      *float64
	warmStdStorageDiff     *string
	glacierIRSavingsUSD    *string
	glacierIRSavingsPct    *float64
	glacierIRStorageDiff   *string
	glacierFlexSavingsUSD  *string
	glacierFlexSavingsPct  *float64
	glacierFlexStorageDiff *string

	hasUnpricedOmission   bool
	hasIncompleteOmission bool

	warmStdBreakEven     *string
	glacierIRBreakEven   *string
	glacierFlexBreakEven *string

	glacierIRRestoreAmp   *float64
	glacierFlexRestoreAmp *float64
}

func computePolicyMetrics(trace Trace, replayRes timelineReplayResult, reports policyReports) policyMetricsResult {
	wUSD, wPct, wDiff, wUnp, wInc := calcSavings(reports.directReport, reports.warmStdReport, replayRes.currentLogical)
	irUSD, irPct, irDiff, irUnp, irInc := calcSavings(reports.directReport, reports.glacierIRReport, replayRes.currentLogical)
	flUSD, flPct, flDiff, flUnp, flInc := calcSavings(reports.directReport, reports.glacierFlexReport, replayRes.currentLogical)

	hasUnpriced := wUnp || irUnp || flUnp
	hasIncomplete := wInc || irInc || flInc

	wBE := calcBreakEven(reports.warmStdReport, reports.warmStdBreakdown, reports.directReport, reports.directBreakdown, replayRes.directTotals, replayRes.currentLogical, trace, replayRes.monthDuration)
	irBE := calcBreakEven(reports.glacierIRReport, reports.glacierIRBreakdown, reports.directReport, reports.directBreakdown, replayRes.directTotals, replayRes.currentLogical, trace, replayRes.monthDuration)
	flBE := calcBreakEven(reports.glacierFlexReport, reports.glacierFlexBreakdown, reports.directReport, reports.directBreakdown, replayRes.directTotals, replayRes.currentLogical, trace, replayRes.monthDuration)

	irAmp := calcRestoreAmplification(PolicyWarmGlacierIR, trace)
	flAmp := calcRestoreAmplification(PolicyWarmGlacierFlex, trace)

	return policyMetricsResult{
		warmStdSavingsUSD:      wUSD,
		warmStdSavingsPct:      wPct,
		warmStdStorageDiff:     wDiff,
		glacierIRSavingsUSD:    irUSD,
		glacierIRSavingsPct:    irPct,
		glacierIRStorageDiff:   irDiff,
		glacierFlexSavingsUSD:  flUSD,
		glacierFlexSavingsPct:  flPct,
		glacierFlexStorageDiff: flDiff,
		hasUnpricedOmission:    hasUnpriced,
		hasIncompleteOmission:  hasIncomplete,
		warmStdBreakEven:       wBE,
		glacierIRBreakEven:     irBE,
		glacierFlexBreakEven:   flBE,
		glacierIRRestoreAmp:    irAmp,
		glacierFlexRestoreAmp:  flAmp,
	}
}

func calcSavings(directReport cost.Report, polReport cost.Report, currentLogical uint64) (*string, *float64, *string, bool, bool) {
	var indicativeDiff *string
	hasUnpricedOmission := false
	hasIncompleteOmission := false

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
		return nil, nil, indicativeDiff, hasUnpricedOmission, hasIncompleteOmission
	}

	dRat, okD := new(big.Rat).SetString(*directReport.CompleteTotalUSD)
	pRat, okP := new(big.Rat).SetString(*polReport.CompleteTotalUSD)
	if !okD || !okP || dRat.Sign() == 0 {
		return nil, nil, indicativeDiff, hasUnpricedOmission, hasIncompleteOmission
	}
	savingsRat := new(big.Rat).Sub(dRat, pRat)
	diffFloat, _ := savingsRat.Float64()
	directFloat, _ := dRat.Float64()
	if directFloat <= 0 {
		s := savingsRat.RatString()
		return &s, nil, indicativeDiff, hasUnpricedOmission, hasIncompleteOmission
	}
	pct := (diffFloat / directFloat) * 100.0
	s := savingsRat.RatString()
	return &s, &pct, indicativeDiff, hasUnpricedOmission, hasIncompleteOmission
}

// calcBreakEven calculates the logical data volume (GB) required for cumulative deduplication and
// compression savings to offset the gateway's fixed infrastructure overhead.
// Modeling assumption: ddb.storage is treated as fixed baseline metadata table overhead for the
// trace duration; linear DynamoDB storage growth with higher logical footprint is not modeled.
func calcBreakEven(polReport cost.Report, polBreakdown PolicyCostBreakdown, directReport cost.Report, directBreakdown PolicyCostBreakdown, directTotals map[string]*big.Rat, currentLogical uint64, trace Trace, monthDuration time.Duration) *string {
	if directReport.CompleteTotalUSD == nil || polReport.CompleteTotalUSD == nil {
		return nil
	}
	dStorageRat, _ := new(big.Rat).SetString(directBreakdown.StorageUSD)
	pStorageRat, _ := new(big.Rat).SetString(polBreakdown.StorageUSD)
	if dStorageRat == nil || pStorageRat == nil || currentLogical == 0 {
		return nil
	}
	storageSaving := new(big.Rat).Sub(dStorageRat, pStorageRat)
	if storageSaving.Sign() <= 0 {
		return nil
	}
	var logicalGBRat *big.Rat
	if stdTotal, ok := directTotals["s3.storage.standard"]; ok && stdTotal != nil && stdTotal.Sign() > 0 {
		logicalGBRat = new(big.Rat).Set(stdTotal)
	} else {
		logicalGBRat = new(big.Rat).SetFrac64(int64(currentLogical), int64(trace.BytesPerGB))
	}
	if logicalGBRat == nil || logicalGBRat.Sign() <= 0 {
		return nil
	}
	unitSaving := new(big.Rat).Quo(storageSaving, logicalGBRat)

	// Isolate FIXED monthly infrastructure costs (exclude variable charges like alb.lcu and ddb.write/read)
	fixedInfraTraceRat := new(big.Rat)
	hasFixedInfraLines := false
	for _, l := range polReport.Lines {
		val, _ := new(big.Rat).SetString(l.USD)
		if val == nil {
			continue
		}
		isFixedInfra := false
		switch {
		case strings.HasPrefix(l.RateID, "ec2."):
			isFixedInfra = true
		case strings.HasPrefix(l.RateID, "ebs."):
			isFixedInfra = true
		case l.RateID == "alb.hour":
			isFixedInfra = true
		case l.RateID == "ddb.storage":
			isFixedInfra = true
		case strings.HasPrefix(l.RateID, "edge.compute"):
			isFixedInfra = true
		case strings.HasPrefix(l.RateID, "edge.disk"):
			isFixedInfra = true
		case strings.HasPrefix(l.RateID, "network.nat_hour") || strings.HasPrefix(l.RateID, "network.endpoint_hour"):
			isFixedInfra = true
		}
		if isFixedInfra {
			hasFixedInfraLines = true
			fixedInfraTraceRat.Add(fixedInfraTraceRat, val)
		}
	}

	if !hasFixedInfraLines {
		return nil
	}
	if fixedInfraTraceRat.Sign() == 0 {
		zeroStr := "0 GB (no fixed infrastructure overhead)"
		return &zeroStr
	}
	if trace.DurationNS <= 0 {
		return nil
	}

	// Normalize fixed infrastructure from trace duration to a monthly fixed cost basis (USD/month)
	monthlyFixedInfra := new(big.Rat).Mul(fixedInfraTraceRat, new(big.Rat).SetFrac64(int64(monthDuration), trace.DurationNS))
	return cost.BreakEven(monthlyFixedInfra, unitSaving)
}

func countTraceColdClasses(trace Trace) (hasIR, hasFlex bool, coldClassesInTrace int) {
	for _, ev := range trace.Events {
		for _, u := range ev.Usage {
			switch {
			case u.RateID == "s3.retrieval_ir":
				if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
					hasIR = true
				}
			case u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb":
				if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
					hasFlex = true
				}
			}
		}
	}
	if hasIR {
		coldClassesInTrace++
	}
	if hasFlex {
		coldClassesInTrace++
	}
	return
}

func processEventColdRetrieval(polName PolicyName, trace Trace, event Event, eventIdx int, tracker *RestoreTracker, coldBytesRetrieved *uint64) (hasColdUsageForPolicy bool, hasIRInEvent bool, hasFlexInEvent bool, restoredPacksInEvent map[storage.PackRef]bool) {
	restoredPacksInEvent = make(map[storage.PackRef]bool)
	for _, u := range event.Usage {
		switch {
		case u.RateID == "s3.retrieval_ir":
			if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
				hasIRInEvent = true
				if polName == PolicyWarmGlacierIR {
					b, ok := ratGBToBytes(q, trace.BytesPerGB)
					if !ok || b > math.MaxUint64-*coldBytesRetrieved {
						tracker.MarkUnproven(UnprovenColdRestoreDuration)
						continue
					}
					*coldBytesRetrieved += b
					hasColdUsageForPolicy = true
				}
			}
		case u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb":
			if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
				hasFlexInEvent = true
				if polName == PolicyWarmGlacierFlex {
					b, ok := ratGBToBytes(q, trace.BytesPerGB)
					if !ok || b > math.MaxUint64-*coldBytesRetrieved {
						tracker.MarkUnproven(UnprovenColdRestoreDuration)
						continue
					}
					*coldBytesRetrieved += b
					hasColdUsageForPolicy = true

					var packBytes uint64
					if event.Tiering != nil && event.Tiering.Pack.Size > 0 {
						packBytes = uint64(event.Tiering.Pack.Size)
					}
					pInfo, ok := tracker.RegisterRestore(eventIdx, event, trace, b, packBytes)
					if ok && pInfo != nil {
						restoredPacksInEvent[pInfo.PackRef] = true
					}
				}
			}
		}
	}
	return
}

func calcRestoreAmplification(polName PolicyName, trace Trace) *float64 {
	if polName != PolicyWarmGlacierIR && polName != PolicyWarmGlacierFlex {
		return nil
	}
	var coldBytesRetrieved uint64
	var coldDeliveredBytes uint64
	tracker := NewRestoreTracker()

	_, _, coldClassesInTrace := countTraceColdClasses(trace)

	for eventIdx, event := range trace.Events {
		tracker.ExpirePastDeadlines(event.AtNS)

		hasColdUsageForPolicy, hasIRInEvent, hasFlexInEvent, restoredInThisEvent := processEventColdRetrieval(
			polName, trace, event, eventIdx, tracker, &coldBytesRetrieved,
		)

		if polName == PolicyWarmGlacierFlex {
			tracker.ApplyAuthoritativeMetadata(event, trace, restoredInThisEvent)
		}

		coldClassesWithRetrievalInEvent := 0
		if hasIRInEvent {
			coldClassesWithRetrievalInEvent++
		}
		if hasFlexInEvent {
			coldClassesWithRetrievalInEvent++
		}

		clientTraffic := event.Traffic[cost.GatewayClient]
		delivered, ok := tracker.CheckDeliveryProvenance(
			event, polName, trace, clientTraffic, hasColdUsageForPolicy,
			coldClassesWithRetrievalInEvent, coldClassesInTrace,
		)
		if !ok {
			return nil
		}
		coldDeliveredBytes += delivered
	}

	if tracker.HasUnprovenCold || coldBytesRetrieved == 0 || coldDeliveredBytes == 0 {
		return nil
	}
	return cost.RestoreAmplification(coldBytesRetrieved, coldDeliveredBytes)
}
