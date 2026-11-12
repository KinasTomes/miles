package simulator

import (
	"strings"

	"github.com/KinasTomes/miles/internal/cost"
)

// isIncompleteOmission identifies declarations in trace omissions indicating that the trace
// has partial infrastructure, missing network routes, or unpriced components.
func isIncompleteOmission(om string) bool {
	knownIncompletePhrases := []string{
		"unpriced items present",
		"without complete matched totals",
		"partial prototype",
		"infrastructure compute/disk/alb/dynamodb/kms costs omitted",
		"infrastructure and account-wide tier aggregation omitted",
		"full ec2/ebs/alb/kms/cloudwatch infrastructure and account-wide tier aggregation",
		"minimum duration/size, archive metadata, restore and retries not exercised",
		"missing ec2, ebs",
		"missing network telemetry",
		"trace contains no observed cold inventory",
		"storage inventory inconsistent",
		"glacier accounting unproven",
		"glacier retention fee unproven",
		"glacier storage unproven",
		"glacier restore storage unproven",
		"glacier restore unproven",
		"baseline direct s3 client request telemetry not independently observed",
		"candidate policy cold read pricing unproven",
	}
	lower := strings.ToLower(strings.TrimSpace(om))
	for _, phrase := range knownIncompletePhrases {
		if strings.Contains(lower, phrase) {
			return true
		}
	}
	if strings.HasPrefix(lower, "omission:incomplete_") || strings.HasPrefix(lower, "omission:unproven_") {
		return true
	}
	return false
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
		if isIncompleteOmission(om) {
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

	// Invariant: Direct S3 baseline requires independent client request telemetry (DirectUsage).
	// Baseline must not conflate DSG backend pack + sidecar upload requests with client logical operations.
	if polName == PolicyDirectS3 {
		hasExplicitDirectUsage := false
		for _, ev := range trace.Events {
			if len(ev.DirectUsage) > 0 {
				hasExplicitDirectUsage = true
				break
			}
		}
		if !hasExplicitDirectUsage {
			return false
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
	// candidate policies with cold storage obligations must provide proven cold pricing lines.
	hasColdReadActivity := false
	for _, event := range trace.Events {
		for _, u := range event.Usage {
			if u.RateID == "s3.retrieval_ir" || strings.HasPrefix(u.RateID, "s3.restore_") {
				hasColdReadActivity = true
				break
			}
		}
		if event.ColdLogicalDeliveredBytes > 0 || len(event.ColdLogicalDeliveredByClass) > 0 {
			hasColdReadActivity = true
			break
		}
	}

	if hasColdReadActivity {
		if polName == PolicyWarmGlacierIR && !hasIRRetrieval {
			return false
		}
		if polName == PolicyWarmGlacierFlex && !hasFlexRestore {
			return false
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
