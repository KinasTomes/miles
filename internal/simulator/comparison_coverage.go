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

// PolicyCompleteness holds the evaluation result and policy-local unproven reasons
// ensuring candidate policy completeness checks remain isolated and order-invariant.
type PolicyCompleteness struct {
	IsComplete      bool
	UnprovenReasons map[UnprovenColdReason]bool
}

// checkPolicyCompleteness evaluates whether a specific candidate policy meets all coverage,
// infrastructure, and cold read evidence obligations.
// Operates fail-closed and strictly policy-local: does not mutate baseReasons.
func checkPolicyCompleteness(polName PolicyName, r cost.Report, trace Trace, currentLogical uint64, hasStorageInconsistency bool, baseReasons map[UnprovenColdReason]bool) PolicyCompleteness {
	localReasons := make(map[UnprovenColdReason]bool)
	for k, v := range baseReasons {
		if v {
			localReasons[k] = true
		}
	}

	fail := func() PolicyCompleteness {
		return PolicyCompleteness{IsComplete: false, UnprovenReasons: localReasons}
	}
	failReason := func(reason UnprovenColdReason) PolicyCompleteness {
		localReasons[reason] = true
		return PolicyCompleteness{IsComplete: false, UnprovenReasons: localReasons}
	}

	if hasStorageInconsistency {
		return fail()
	}
	if len(localReasons) > 0 && (polName == PolicyWarmGlacierIR || polName == PolicyWarmGlacierFlex) {
		return fail()
	}
	if len(r.Unpriced) > 0 {
		return fail()
	}
	for _, om := range trace.Omissions {
		if !isProvenCompleteOmission(om) {
			return fail()
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
		return fail()
	}

	// S3 PUT requests required if writes or storage exist
	if currentLogical > 0 && !hasPutReq {
		return fail()
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
					return fail()
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
					return fail()
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
		return fail()
	}

	// Invariant: Per-policy cold coverage check.
	// If the workload exercised cold reads (retrieval usage, restore usage, or cold delivery telemetry),
	// candidate policies with cold storage obligations must match event-by-event physical delivery/restore.
	// We cannot assume cold coverage merely because an unrelated restore occurred in another event.
	if polName == PolicyWarmGlacierIR || polName == PolicyWarmGlacierFlex {
		tracker := NewRestoreTracker()
		for evIdx, event := range trace.Events {
			// Bounded checked sum of cold delivery telemetry in this event
			totalColdDelivered, validColdTotal := event.TotalColdDeliveredBytes()
			if !validColdTotal {
				return failReason(UnprovenColdIdentity)
			}

			// Restore tracking for Glacier Flexible
			if polName == PolicyWarmGlacierFlex {
				tracker.ExpirePastDeadlines(event.AtNS)
				restoredInThisEvent := make(map[storage.PackRef]bool)
				for _, u := range event.Usage {
					if u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb" {
						if q, ok := new(big.Rat).SetString(u.Quantity); ok && q.Sign() > 0 {
							if b, ok := ratGBToBytes(q, trace.BytesPerGB); ok && b > 0 {
								// Invariant (P1): S3 restore on a pack explicitly typed as Glacier IR (or non-Glacier)
								// cannot be treated as a valid Glacier Flexible restore.
								if event.Tiering != nil && event.Tiering.Pack.Class != "" && event.Tiering.Pack.Class != storage.Glacier {
									return failReason(UnprovenColdReadObligation)
								}

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
				if event.Tiering == nil || event.Tiering.Pack.Class == "" || event.Tiering.Pack.Class == storage.Glacier {
					tracker.ApplyAuthoritativeMetadata(event, trace, restoredInThisEvent)
				}
			}

			// Invariant (P3): Only positive bytes (b > 0) constitute a delivery claim.
			// Explicit measured 0 bytes (e.g. ColdLogicalDeliveredByClass={GLACIER_IR:0} or scalar 0)
			// is equivalent to no cold delivery and must not trigger an unproven read obligation.
			hasColdReadInEvent := totalColdDelivered > 0 || event.ColdLogicalDeliveredBytes > 0
			if !hasColdReadInEvent {
				if polName == PolicyWarmGlacierIR {
					for _, u := range event.Usage {
						if u.RateID == "s3.retrieval_ir" || u.RateID == "s3.get_ir" {
							hasColdReadInEvent = true
							break
						}
					}
				} else if polName == PolicyWarmGlacierFlex {
					for _, u := range event.Usage {
						if u.RateID == "s3.restore_standard_gb" || u.RateID == "s3.restore_bulk_gb" {
							hasColdReadInEvent = true
							break
						}
					}
				}
			}

			if hasColdReadInEvent {
				if polName == PolicyWarmGlacierIR {
					if !hasIRRetrieval {
						return failReason(UnprovenColdReadObligation)
					}
					// (P1): IR retrieval cannot match a pack explicitly typed as non-IR (e.g. Glacier Flexible)
					if event.Tiering != nil && event.Tiering.Pack.Class != "" && event.Tiering.Pack.Class != storage.GlacierIR {
						return failReason(UnprovenColdReadObligation)
					}
					// (P1): If cold delivery is exclusively for another class, IR cannot claim it
					if len(event.ColdLogicalDeliveredByClass) > 0 {
						if b, ok := event.ColdLogicalDeliveredByClass[storage.GlacierIR]; !ok || b == 0 {
							for otherCls, otherB := range event.ColdLogicalDeliveredByClass {
								if otherCls != storage.GlacierIR && otherB > 0 {
									return failReason(UnprovenColdReadObligation)
								}
							}
						}
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
						return failReason(UnprovenColdReadObligation)
					}
				} else if polName == PolicyWarmGlacierFlex {
					if !hasFlexRestore {
						return failReason(UnprovenColdReadObligation)
					}
					// (P1): S3 restore cannot match a pack explicitly typed as non-Glacier (e.g. Glacier IR)
					if event.Tiering != nil && event.Tiering.Pack.Class != "" && event.Tiering.Pack.Class != storage.Glacier {
						return failReason(UnprovenColdReadObligation)
					}
					// (P1): If cold delivery is exclusively for another class, Flex cannot claim it
					if len(event.ColdLogicalDeliveredByClass) > 0 {
						if b, ok := event.ColdLogicalDeliveredByClass[storage.Glacier]; !ok || b == 0 {
							for otherCls, otherB := range event.ColdLogicalDeliveredByClass {
								if otherCls != storage.Glacier && otherB > 0 {
									return failReason(UnprovenColdReadObligation)
								}
							}
						}
					}
					packInfo, resolved := tracker.ResolvePackForDelivery(event)
					if !resolved || packInfo == nil || packInfo.RetrievedBytes == 0 ||
						packInfo.Expired || event.AtNS >= packInfo.DeadlineNS {
						return failReason(UnprovenColdReadObligation)
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
				return fail()
			}
		} else {
			hasDSGRoutes := observedRoutes[cost.ClientGateway] && observedRoutes[cost.GatewayClient] &&
				observedRoutes[cost.GatewayS3] && observedRoutes[cost.S3Gateway] &&
				routeBytes[cost.ClientGateway] == 0 && routeBytes[cost.GatewayClient] == 0 &&
				routeBytes[cost.GatewayS3] == 0 && routeBytes[cost.S3Gateway] == 0
			if !hasDSGRoutes {
				return fail()
			}
		}
	}

	// Topology & Infrastructure requirements for candidate DSG policies
	if polName != PolicyDirectS3 {
		if trace.Profile == "cloud" {
			// Cloud DSG requires Compute (EC2), Disk (EBS), and Load Balancer (ALB hour + LCU)
			if !hasEC2 || !hasEBS || !hasALBHour || !hasALBLCU {
				return fail()
			}
			// Cloud metadata backend requires DynamoDB storage and write accounting
			if trace.Metadata == "dynamodb" || trace.Metadata == "dynamodb-local" {
				if !hasDDBStorage || !hasDDBWrite {
					return fail()
				}
			}
		} else if trace.Profile == "edge" {
			// Edge DSG requires Edge Compute and Edge Disk/Network
			if !hasEdgeCompute || !hasEdgeDisk {
				return fail()
			}
		}
	}

	return PolicyCompleteness{IsComplete: true, UnprovenReasons: localReasons}
}

// isPolicyComplete returns true if candidate policy polName is proven complete.
// Preserves caller's unprovenColdReasons without in-place mutation.
func isPolicyComplete(polName PolicyName, r cost.Report, trace Trace, currentLogical uint64, hasStorageInconsistency bool, unprovenColdReasons map[UnprovenColdReason]bool) bool {
	return checkPolicyCompleteness(polName, r, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons).IsComplete
}

// applyCompletenessTotals evaluates candidate policies independently with policy-local reasons,
// ensuring order-invariance. Diagnostics are merged into unprovenColdReasons only after all policies are evaluated.
func applyCompletenessTotals(reports *policyReports, trace Trace, currentLogical uint64, hasStorageInconsistency bool, unprovenColdReasons map[UnprovenColdReason]bool) {
	directComp := checkPolicyCompleteness(PolicyDirectS3, reports.directReport, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons)
	if directComp.IsComplete {
		sub := reports.directReport.Subtotal
		reports.directReport.CompleteTotalUSD = &sub
	}

	warmStdComp := checkPolicyCompleteness(PolicyWarmStandardOnly, reports.warmStdReport, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons)
	if warmStdComp.IsComplete {
		sub := reports.warmStdReport.Subtotal
		reports.warmStdReport.CompleteTotalUSD = &sub
	}

	glacierIRComp := checkPolicyCompleteness(PolicyWarmGlacierIR, reports.glacierIRReport, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons)
	if glacierIRComp.IsComplete {
		sub := reports.glacierIRReport.Subtotal
		reports.glacierIRReport.CompleteTotalUSD = &sub
	}

	glacierFlexComp := checkPolicyCompleteness(PolicyWarmGlacierFlex, reports.glacierFlexReport, trace, currentLogical, hasStorageInconsistency, unprovenColdReasons)
	if glacierFlexComp.IsComplete {
		sub := reports.glacierFlexReport.Subtotal
		reports.glacierFlexReport.CompleteTotalUSD = &sub
	}

	if unprovenColdReasons != nil {
		for r := range glacierIRComp.UnprovenReasons {
			unprovenColdReasons[r] = true
		}
		for r := range glacierFlexComp.UnprovenReasons {
			unprovenColdReasons[r] = true
		}
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
