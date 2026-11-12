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

func calcRestoreAmplification(polName PolicyName, trace Trace) *float64 {
	if polName != PolicyWarmGlacierIR && polName != PolicyWarmGlacierFlex {
		return nil
	}
	var coldBytesRetrieved uint64
	var coldDeliveredBytes uint64
	hasUnprovenColdDelivery := false

	type packRestoreInfo struct {
		retrievedBytes  uint64
		restoredUntilNS int64
	}
	restoredPacks := make(map[storage.PackRef]*packRestoreInfo)

	var targetClass storage.StorageClass
	switch polName {
	case PolicyWarmGlacierIR:
		targetClass = storage.GlacierIR
	case PolicyWarmGlacierFlex:
		targetClass = storage.Glacier
	}

	// Count distinct cold classes that had retrieval/restore usage in the entire trace.
	hasIRInTrace := false
	hasFlexInTrace := false
	for _, ev := range trace.Events {
		for _, u := range ev.Usage {
			switch {
			case u.RateID == "s3.retrieval_ir":
				if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
					hasIRInTrace = true
				}
			case u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb":
				if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
					hasFlexInTrace = true
				}
			}
		}
	}
	coldClassesInTrace := 0
	if hasIRInTrace {
		coldClassesInTrace++
	}
	if hasFlexInTrace {
		coldClassesInTrace++
	}

	for _, event := range trace.Events {
		hasColdUsageForPolicy := false
		hasIRInEvent := false
		hasFlexInEvent := false

		for _, u := range event.Usage {
			switch {
			case u.RateID == "s3.retrieval_ir":
				if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
					hasIRInEvent = true
					if polName == PolicyWarmGlacierIR {
						gbFloat, _ := q.Float64()
						b := uint64(gbFloat * float64(trace.BytesPerGB))
						coldBytesRetrieved += b
						hasColdUsageForPolicy = true
					}
				}
			case u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb":
				if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
					hasFlexInEvent = true
					if polName == PolicyWarmGlacierFlex {
						gbFloat, _ := q.Float64()
						b := uint64(gbFloat * float64(trace.BytesPerGB))
						if b > math.MaxUint64-coldBytesRetrieved {
							hasUnprovenColdDelivery = true
							continue
						}
						coldBytesRetrieved += b
						hasColdUsageForPolicy = true
						restoreDur, ok := trace.RestoreDuration(event)
						if !ok {
							hasUnprovenColdDelivery = true
							continue
						}
						durNS := restoreDur.Nanoseconds()
						if durNS <= 0 || durNS > math.MaxInt64-event.AtNS {
							hasUnprovenColdDelivery = true
							continue
						}
						expiry := event.AtNS + durNS

						var packRef storage.PackRef
						if event.Tiering != nil {
							packRef = event.Tiering.Pack.Ref
							if !event.Tiering.Pack.RestoredUntil.IsZero() && !trace.Start.IsZero() {
								expiry = event.Tiering.Pack.RestoredUntil.Sub(trace.Start).Nanoseconds()
							}
						}

						pInfo := restoredPacks[packRef]
						if pInfo == nil {
							pInfo = &packRestoreInfo{}
							restoredPacks[packRef] = pInfo
						}
						if b > math.MaxUint64-pInfo.retrievedBytes {
							hasUnprovenColdDelivery = true
							continue
						}
						pInfo.retrievedBytes += b
						if expiry > pInfo.restoredUntilNS {
							pInfo.restoredUntilNS = expiry
						}
					}
				}
			}
		}

		// If this event carries authoritative pack metadata with RestoredUntil for an already restored pack,
		// update that pack's active restore window or detect expiry.
		// Never allow an unrestored pack's metadata to extend or create a restore window (Finding 1 [P2]).
		if polName == PolicyWarmGlacierFlex && event.Tiering != nil && !event.Tiering.Pack.RestoredUntil.IsZero() && !trace.Start.IsZero() {
			packRef := event.Tiering.Pack.Ref
			pInfo := restoredPacks[packRef]
			if pInfo != nil && pInfo.retrievedBytes > 0 {
				eventTime := trace.Start.Add(time.Duration(event.AtNS))
				if !event.Tiering.Pack.RestoredUntil.After(eventTime) {
					pInfo.restoredUntilNS = event.AtNS
				} else {
					packExpiryNS := event.Tiering.Pack.RestoredUntil.Sub(trace.Start).Nanoseconds()
					if packExpiryNS > pInfo.restoredUntilNS {
						pInfo.restoredUntilNS = packExpiryNS
					}
				}
			}
		}

		coldClassesWithRetrievalInEvent := 0
		if hasIRInEvent {
			coldClassesWithRetrievalInEvent++
		}
		if hasFlexInEvent {
			coldClassesWithRetrievalInEvent++
		}

		// Check if this event claims cold delivery for targetClass.
		var delivered uint64
		hasDeliveryClaim := false

		if len(event.ColdLogicalDeliveredByClass) > 0 {
			if b, ok := event.ColdLogicalDeliveredByClass[targetClass]; ok && b > 0 {
				delivered = b
				hasDeliveryClaim = true
			}
			// When class-specific telemetry is present, NEVER fall back to scalar for missing class.
		} else if event.ColdLogicalDeliveredBytes > 0 {
			// Scalar telemetry fallback is only permitted when:
			// 1. In this event, targetClass is the only cold class retrieved (if retrieval occurred in this event), OR
			// 2. In this trace, targetClass is the only cold class retrieved anywhere (if delivery occurs after restore).
			if hasColdUsageForPolicy && coldClassesWithRetrievalInEvent == 1 {
				delivered = event.ColdLogicalDeliveredBytes
				hasDeliveryClaim = true
			} else if !hasColdUsageForPolicy && coldClassesInTrace == 1 {
				delivered = event.ColdLogicalDeliveredBytes
				hasDeliveryClaim = true
			}
		}

		// For candidate Glacier policies (WarmGlacierIR, WarmGlacierFlex), client reads
		// are served strictly through the Dedup Storage Gateway via gateway_to_client.
		// Direct route s3_to_client belongs to the Direct S3 baseline topology and is
		// never evidence of gateway-delivered cold reads.
		clientTraffic := event.Traffic[cost.GatewayClient]

		// If this event has a physical cold retrieval for this policy:
		if hasColdUsageForPolicy {
			// If the event has client traffic (gateway_to_client > 0) but lacks proven delivery telemetry:
			if clientTraffic > 0 && !hasDeliveryClaim {
				hasUnprovenColdDelivery = true
				continue
			}
		}

		if hasDeliveryClaim {
			// Provenance check: verify the delivery can be linked to an active retrieval or restore.
			if polName == PolicyWarmGlacierFlex {
				if coldBytesRetrieved == 0 {
					hasUnprovenColdDelivery = true
					continue
				}

				// If event metadata carries RestoredUntil that is already expired at or before event time, fail closed.
				if event.Tiering != nil && !event.Tiering.Pack.RestoredUntil.IsZero() && !trace.Start.IsZero() {
					eventTime := trace.Start.Add(time.Duration(event.AtNS))
					if !event.Tiering.Pack.RestoredUntil.After(eventTime) {
						hasUnprovenColdDelivery = true
						continue
					}
				}

				var packInfo *packRestoreInfo
				if event.Tiering != nil && event.Tiering.Pack.Ref.ID != "" {
					// Event explicitly references a specific pack by PackRef.ID + Generation.
					packInfo = restoredPacks[event.Tiering.Pack.Ref]
				} else {
					// Event does not identify a specific pack by Ref.ID.
					// Attribution is only sound when exactly one distinct pack was restored in the trace.
					numRestored := 0
					var solePack *packRestoreInfo
					for _, p := range restoredPacks {
						if p.retrievedBytes > 0 {
							numRestored++
							solePack = p
						}
					}
					if numRestored == 1 {
						packInfo = solePack
					} else {
						// Ambiguous (multiple packs restored) or no packs restored.
						hasUnprovenColdDelivery = true
						continue
					}
				}

				// Finding 1 [P2]: The pack being read must have been restored (proven in numerator).
				if packInfo == nil || packInfo.retrievedBytes == 0 {
					hasUnprovenColdDelivery = true
					continue
				}

				// Finding 2 [P3]: Check expiration at or after restoredUntilNS (event.AtNS >= restoredUntilNS).
				if event.AtNS >= packInfo.restoredUntilNS {
					hasUnprovenColdDelivery = true
					continue
				}
			} else if polName == PolicyWarmGlacierIR {
				// For Glacier IR, reads are instant retrievals; delivery must occur in an event with s3.retrieval_ir.
				if !hasColdUsageForPolicy {
					hasUnprovenColdDelivery = true
					continue
				}
			}

			// Consistency check 1: Measured gateway-to-client traffic must be positive.
			if clientTraffic == 0 {
				hasUnprovenColdDelivery = true
				continue
			}

			// Consistency check 2: The sum of cold deliveries across all classes in this event
			// must not exceed measured client traffic, and must not overflow uint64.
			totalColdDelivered, ok := event.TotalColdDeliveredBytes()
			if !ok || totalColdDelivered > clientTraffic {
				hasUnprovenColdDelivery = true
				continue
			}

			// Consistency check 3: Cold delivery for this class must not exceed measured client traffic.
			if delivered > clientTraffic {
				hasUnprovenColdDelivery = true
				continue
			}

			coldDeliveredBytes += delivered
		} else {
			// Even if this event does not claim delivery for targetClass, if it claims cold delivery
			// for other classes, verify that total claimed cold delivery does not exceed measured traffic.
			totalColdDelivered, ok := event.TotalColdDeliveredBytes()
			if !ok || (totalColdDelivered > 0 && totalColdDelivered > clientTraffic) {
				hasUnprovenColdDelivery = true
				continue
			}
		}
	}

	if hasUnprovenColdDelivery || coldBytesRetrieved == 0 || coldDeliveredBytes == 0 {
		return nil
	}
	return cost.RestoreAmplification(coldBytesRetrieved, coldDeliveredBytes)
}
