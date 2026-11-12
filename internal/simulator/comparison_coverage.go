package simulator

import (
	"math/big"
	"strings"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/storage"
)

// isProvenCompleteOmission checks whether an omission entry is an explicit proof of completeness
// or a benign modeling assumption that does not represent unmeasured trace telemetry.
// Fail-closed rule: ANY omission that is not explicitly proven complete causes completeness to fail.
func isProvenCompleteOmission(om string) bool {
	lower := strings.ToLower(strings.TrimSpace(om))
	if lower == "" {
		return false
	}

	// Typed schema / explicit severity
	if strings.HasPrefix(lower, "severity:info") ||
		strings.HasPrefix(lower, "omission:info:") ||
		strings.HasPrefix(lower, "note:") ||
		strings.HasPrefix(lower, "modelling_assumption:") ||
		strings.HasPrefix(lower, "verified_complete:") {
		return true
	}

	// Explicit benign modeling notes & verified complete declarations
	provenCompletePhrases := []string{
		"storage month and gb divisor are explicit modelling assumptions",
		"full verified observation run across all topology dimensions",
		"full verified observation",
		"full observation run",
		"deterministic simulation note",
		"omissions resolved by full measurement",
		"trace inventory snapshot verified against manifest",
	}

	for _, phrase := range provenCompletePhrases {
		if lower == phrase || strings.HasPrefix(lower, phrase) {
			return true
		}
	}

	return false
}

// isIncompleteOmission identifies declarations in trace omissions indicating that the trace
// has partial infrastructure, missing network routes, unpriced components, or unproven observations.
// Operates fail-closed: anything not proven complete is treated as incomplete.
func isIncompleteOmission(om string) bool {
	return !isProvenCompleteOmission(om)
}

func isPolicyComplete(polName PolicyName, r cost.Report, trace Trace, currentLogical uint64, hasStorageInconsistency bool, unprovenColdReasons map[UnprovenColdReason]bool) bool {
	if hasStorageInconsistency {
		return false
	}
	if len(unprovenColdReasons) > 0 && (polName == PolicyWarmGlacierIR || polName == PolicyWarmGlacierFlex) {
		return false
	}
	if len(r.Unpriced) > 0 {
		return false
	}
	for _, om := range trace.Omissions {
		if !isProvenCompleteOmission(om) {
			return false
		}
	}

	hasStorage := false
	hasPutReq := false
	hasStdGetReq := false
	hasIRRetrieval := false
	hasFlexRestore := false
	hasPayloadTransfer := false
	hasEC2 := false
	hasEBS := false
	hasALBHour := false
	hasALBLCU := false
	hasDDBStorage := false
	hasDDBWrite := false
	hasEdgeCompute := false
	hasEdgeDisk := false

	for _, l := range r.Lines {
		switch {
		case strings.HasPrefix(l.RateID, "s3.storage."):
			hasStorage = true
		case strings.HasPrefix(l.RateID, "s3.put_"):
			hasPutReq = true
		case l.RateID == "s3.get_standard":
			hasStdGetReq = true
		case l.RateID == "s3.retrieval_ir" || l.RateID == "s3.get_ir":
			hasIRRetrieval = true
		case strings.HasPrefix(l.RateID, "s3.restore_"):
			hasFlexRestore = true
		case strings.HasPrefix(l.RateID, "transfer."):
			hasPayloadTransfer = true
		case strings.HasPrefix(l.RateID, "ec2."):
			hasEC2 = true
		case strings.HasPrefix(l.RateID, "ebs."):
			hasEBS = true
		case l.RateID == "alb.hour":
			hasALBHour = true
		case l.RateID == "alb.lcu":
			hasALBLCU = true
		case l.RateID == "ddb.storage":
			hasDDBStorage = true
		case l.RateID == "ddb.write":
			hasDDBWrite = true
		case strings.HasPrefix(l.RateID, "edge.compute"):
			hasEdgeCompute = true
		case strings.HasPrefix(l.RateID, "edge.disk"):
			hasEdgeDisk = true
		}
	}

	// Storage is always required
	if !hasStorage {
		return false
	}

	// S3 PUT requests required if writes or storage exist
	if currentLogical > 0 && !hasPutReq {
		return false
	}

	// Invariant: Direct S3 baseline requires event-scoped independent client request telemetry (DirectUsage).
	// Baseline must not conflate DSG backend pack + sidecar upload requests with client logical operations.
	// Every event exercising client writes or downloads must have explicit DirectUsage covering the operation.
	if polName == PolicyDirectS3 {
		for _, ev := range trace.Events {
			hasWrites := ev.Counters.ChunkOccurrences > 0 || ev.Counters.RevivedBytes > 0 ||
				ev.Traffic[cost.ClientGateway] > 0 || ev.Traffic[cost.ClientS3] > 0
			if !hasWrites {
				for _, u := range ev.Usage {
					if u.RateID == "s3.put_standard" {
						hasWrites = true
						break
					}
				}
			}
			if hasWrites {
				hasDirectPUT := false
				for _, du := range ev.DirectUsage {
					if du.RateID == "s3.put_standard" {
						if q, ok := new(big.Rat).SetString(du.Quantity); ok && q.Sign() > 0 {
							hasDirectPUT = true
							break
						}
					}
				}
				if !hasDirectPUT {
					return false
				}
			}

			hasDownloads := ev.Traffic[cost.GatewayClient] > 0 || ev.Traffic[cost.S3Client] > 0
			if hasDownloads {
				hasDirectGET := false
				for _, du := range ev.DirectUsage {
					if du.RateID == "s3.get_standard" {
						if q, ok := new(big.Rat).SetString(du.Quantity); ok && q.Sign() > 0 {
							hasDirectGET = true
							break
						}
					}
				}
				if !hasDirectGET {
					return false
				}
			}
		}
	}

	// Network coverage check:
	observedRoutes := make(map[cost.Route]bool)
	routeBytes := make(map[cost.Route]uint64)
	hasClientDownloads := false

	for _, event := range trace.Events {
		for route, b := range event.Traffic {
			observedRoutes[route] = true
			routeBytes[route] += b
		}
		if event.Traffic[cost.GatewayClient] > 0 || event.Traffic[cost.S3Gateway] > 0 ||
			event.Traffic[cost.S3Client] > 0 {
			hasClientDownloads = true
		}
	}

	if hasClientDownloads && !hasStdGetReq {
		return false
	}

	// Invariant: Per-policy cold coverage check.
	// If the workload exercised cold reads (retrieval usage, restore usage, or cold delivery telemetry),
	// candidate policies with cold storage obligations must match event-by-event physical delivery/restore.
	// We cannot assume cold coverage merely because an unrelated restore occurred in another event.
	if polName == PolicyWarmGlacierIR || polName == PolicyWarmGlacierFlex {
		tracker := NewRestoreTracker()
		for evIdx, event := range trace.Events {
			tracker.ExpirePastDeadlines(event.AtNS)
			restoredInThisEvent := make(map[storage.PackRef]bool)
			for _, u := range event.Usage {
				if u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb" {
					if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
						if b, ok := ratGBToBytes(q, trace.BytesPerGB); ok && b > 0 {
							var packBytes uint64
							if event.Tiering != nil {
								if event.Tiering.Pack.Size > 0 {
									packBytes = uint64(event.Tiering.Pack.Size)
								} else if event.Tiering.TotalBytes > 0 {
									packBytes = uint64(event.Tiering.TotalBytes)
								}
							}
							if packBytes == 0 {
								packBytes = b
							}
							pInfo, ok := tracker.RegisterRestore(evIdx, event, trace, b, packBytes)
							if ok && pInfo != nil {
								restoredInThisEvent[pInfo.PackRef] = true
							}
						}
					}
				}
			}
			tracker.ApplyAuthoritativeMetadata(event, trace, restoredInThisEvent)

			hasColdReadInEvent := event.ColdLogicalDeliveredBytes > 0 || len(event.ColdLogicalDeliveredByClass) > 0
			if !hasColdReadInEvent {
				for _, u := range event.Usage {
					if u.RateID == "s3.retrieval_ir" || u.RateID == "s3.get_ir" {
						hasColdReadInEvent = true
						break
					}
				}
			}

			if hasColdReadInEvent {
				if polName == PolicyWarmGlacierIR {
					if !hasIRRetrieval {
						return false
					}
					hasEventIR := false
					for _, u := range event.Usage {
						if u.RateID == "s3.retrieval_ir" || u.RateID == "s3.get_ir" {
							if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
								hasEventIR = true
								break
							}
						}
					}
					if !hasEventIR {
						unprovenColdReasons[UnprovenColdReadObligation] = true
						return false
					}
				} else if polName == PolicyWarmGlacierFlex {
					if !hasFlexRestore {
						return false
					}
					packInfo, resolved := tracker.ResolvePackForDelivery(event)
					if !resolved || packInfo == nil || packInfo.RetrievedBytes == 0 ||
						packInfo.Expired || event.AtNS >= packInfo.DeadlineNS {
						unprovenColdReasons[UnprovenColdReadObligation] = true
						return false
					}
				}
			}
		}
	}

	if !hasPayloadTransfer {
		if polName == PolicyDirectS3 {
			hasDirectRoutes := (observedRoutes[cost.ClientGateway] && observedRoutes[cost.GatewayClient] &&
				routeBytes[cost.ClientGateway] == 0 && routeBytes[cost.GatewayClient] == 0) ||
				(observedRoutes[cost.ClientS3] && observedRoutes[cost.S3Client] &&
					routeBytes[cost.ClientS3] == 0 && routeBytes[cost.S3Client] == 0)
			if !hasDirectRoutes {
				return false
			}
		} else {
			hasDSGRoutes := observedRoutes[cost.ClientGateway] && observedRoutes[cost.GatewayClient] &&
				observedRoutes[cost.GatewayS3] && observedRoutes[cost.S3Gateway] &&
				routeBytes[cost.ClientGateway] == 0 && routeBytes[cost.GatewayClient] == 0 &&
				routeBytes[cost.GatewayS3] == 0 && routeBytes[cost.S3Gateway] == 0
			if !hasDSGRoutes {
				return false
			}
		}
	}

	// Topology & Infrastructure requirements for candidate DSG policies
	if polName != PolicyDirectS3 {
		if trace.Profile == "cloud" {
			// Cloud DSG requires Compute (EC2), Disk (EBS), and Load Balancer (ALB hour + LCU)
			if !hasEC2 || !hasEBS || !hasALBHour || !hasALBLCU {
				return false
			}
			// Cloud metadata backend requires DynamoDB storage and write accounting
			if trace.Metadata == "dynamodb" || trace.Metadata == "dynamodb-local" {
				if !hasDDBStorage || !hasDDBWrite {
					return false
				}
			}
		} else if trace.Profile == "edge" {
			// Edge DSG requires Edge Compute and Edge Disk/Network
			if !hasEdgeCompute || !hasEdgeDisk {
				return false
			}
		}
	}

	return true
}

func applyCompletenessTotals(reports *policyReports, trace Trace, currentLogical uint64, hasStorageInconsistency bool, unprovenColdReasons map[UnprovenColdReason]bool) {
	if isPolicyComplete(PolicyDirectS3, reports.directReport, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons) {
		sub := reports.directReport.Subtotal
		reports.directReport.CompleteTotalUSD = &sub
	}
	if isPolicyComplete(PolicyWarmStandardOnly, reports.warmStdReport, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons) {
		sub := reports.warmStdReport.Subtotal
		reports.warmStdReport.CompleteTotalUSD = &sub
	}
	if isPolicyComplete(PolicyWarmGlacierIR, reports.glacierIRReport, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons) {
		sub := reports.glacierIRReport.Subtotal
		reports.glacierIRReport.CompleteTotalUSD = &sub
	}
	if isPolicyComplete(PolicyWarmGlacierFlex, reports.glacierFlexReport, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons) {
		sub := reports.glacierFlexReport.Subtotal
		reports.glacierFlexReport.CompleteTotalUSD = &sub
	}
}

func buildComparisonOmissions(trace Trace, hasObservedCold bool, hasStorageInconsistency bool, unprovenColdReasons map[UnprovenColdReason]bool, hasUnpricedOmission bool, hasIncompleteOmission bool) []string {
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
	if hasStorageInconsistency {
		omissions = append(omissions, "Storage inventory inconsistent: snapshot physical bytes do not match sum of storage bytes by class")
	}
	for reason := range unprovenColdReasons {
		omissions = append(omissions, reason.OmissionText())
	}
	if hasUnpricedOmission {
		omissions = append(omissions, "Net savings and savings percent vs baseline omitted: unpriced items present in baseline or candidate policy cost evaluation")
	}
	if hasIncompleteOmission {
		omissions = append(omissions, "Net savings vs baseline omitted: cost evaluation is partial prototype without complete matched totals (CompleteTotalUSD is null)")
	}

	return omissions
}
