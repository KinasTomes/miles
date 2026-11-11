package simulator

import (
	"strings"

	"github.com/KinasTomes/miles/internal/cost"
)

func isPolicyComplete(polName PolicyName, r cost.Report, trace Trace, currentLogical uint64, hasStorageInconsistency bool, hasUnprovenColdPacks bool) bool {
	if hasStorageInconsistency {
		return false
	}
	if hasUnprovenColdPacks && (polName == PolicyWarmGlacierIR || polName == PolicyWarmGlacierFlex) {
		return false
	}
	if len(r.Unpriced) > 0 {
		return false
	}
	for _, om := range trace.Omissions {
		lower := strings.ToLower(om)
		if strings.Contains(lower, "omitted") || strings.Contains(lower, "not exercised") ||
			strings.Contains(lower, "not modeled") || strings.Contains(lower, "not modelled") ||
			strings.Contains(lower, "require additional observations") ||
			strings.Contains(lower, "unmeasured") || strings.Contains(lower, "unobserved") ||
			strings.Contains(lower, "incomplete") || strings.Contains(lower, "partial") ||
			strings.Contains(lower, "missing") || strings.Contains(lower, "not accounted") ||
			strings.Contains(lower, "not included") {
			return false
		}
	}

	hasStorage := false
	hasPutReq := false
	hasGetReq := false
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
		case strings.HasPrefix(l.RateID, "s3.get_") || strings.HasPrefix(l.RateID, "s3.retrieval_") ||
			strings.HasPrefix(l.RateID, "s3.restore_"):
			hasGetReq = true
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

	// Network coverage check:
	// A trace must provide explicit evidence of network metering according to topology and scope:
	// 1. Priced payload data transfer lines in report (hasPayloadTransfer == true), OR
	// 2. Explicit zero traffic metering across all required routes for the topology and policy.
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

	if hasClientDownloads && !hasGetReq {
		return false
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

func applyCompletenessTotals(reports *policyReports, trace Trace, currentLogical uint64, hasStorageInconsistency bool, hasUnprovenColdPacks bool) {
	if isPolicyComplete(PolicyDirectS3, reports.directReport, trace, currentLogical, hasStorageInconsistency, hasUnprovenColdPacks) {
		sub := reports.directReport.Subtotal
		reports.directReport.CompleteTotalUSD = &sub
	}
	if isPolicyComplete(PolicyWarmStandardOnly, reports.warmStdReport, trace, currentLogical, hasStorageInconsistency, hasUnprovenColdPacks) {
		sub := reports.warmStdReport.Subtotal
		reports.warmStdReport.CompleteTotalUSD = &sub
	}
	if isPolicyComplete(PolicyWarmGlacierIR, reports.glacierIRReport, trace, currentLogical, hasStorageInconsistency, hasUnprovenColdPacks) {
		sub := reports.glacierIRReport.Subtotal
		reports.glacierIRReport.CompleteTotalUSD = &sub
	}
	if isPolicyComplete(PolicyWarmGlacierFlex, reports.glacierFlexReport, trace, currentLogical, hasStorageInconsistency, hasUnprovenColdPacks) {
		sub := reports.glacierFlexReport.Subtotal
		reports.glacierFlexReport.CompleteTotalUSD = &sub
	}
}

func buildComparisonOmissions(trace Trace, hasObservedCold bool, hasStorageInconsistency bool, hasUnprovenColdPacks bool, hasUnpricedOmission bool, hasIncompleteOmission bool) []string {
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
	if hasUnprovenColdPacks {
		omissions = append(omissions, "Glacier per-pack pricing unproven: trace contains cold storage without per-pack tiering evidence")
	}
	if hasUnpricedOmission {
		omissions = append(omissions, "Net savings and savings percent vs baseline omitted: unpriced items present in baseline or candidate policy cost evaluation")
	}
	if hasIncompleteOmission {
		omissions = append(omissions, "Net savings vs baseline omitted: cost evaluation is partial prototype without complete matched totals (CompleteTotalUSD is null)")
	}

	return omissions
}
