package admin

import (
	"bytes"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/KinasTomes/miles/benchmarks/traces"
	pricingschema "github.com/KinasTomes/miles/configs/pricing"
	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/simulator"
	"github.com/KinasTomes/miles/internal/stats"
	"github.com/KinasTomes/miles/internal/storage"
)

type CostReportInputs struct {
	Region                string                    `json:"region"`
	AsOf                  string                    `json:"as_of"`
	PricingSource         map[string]pricing.Source `json:"pricing_sources"`
	Profile               string                    `json:"profile"`
	Scenario              string                    `json:"scenario"`
	From                  string                    `json:"from"`
	To                    string                    `json:"to"`
	Workload              string                    `json:"workload"`
	Bucket                string                    `json:"bucket,omitempty"`
	Domain                string                    `json:"domain,omitempty"`
	AllocationMethodology string                    `json:"allocation_methodology"`
	Assumptions           []string                  `json:"assumptions"`
}

type DirectS3Breakdown struct {
	LogicalBytes     uint64 `json:"logical_bytes"`
	StorageCostUSD   string `json:"storage_cost_usd"`
	RequestsCostUSD  string `json:"requests_cost_usd"`
	RetrievalCostUSD string `json:"retrieval_cost_usd,omitempty"`
	RestoreCostUSD   string `json:"restore_cost_usd,omitempty"`
	NetworkCostUSD   string `json:"network_cost_usd"`
	TotalCostUSD     string `json:"total_cost_usd"`
}

type BaselineReport struct {
	DirectS3   DirectS3Breakdown `json:"direct_s3"`
	CostReport cost.Report       `json:"cost_report"`
}

type DSGStorageReport struct {
	StorageByClass                       map[string]int64 `json:"storage_by_class"`
	DeadRetiredStorageBytes              int64            `json:"dead_retired_storage_bytes"`
	SidecarJournalCheckpointStorageBytes int64            `json:"sidecar_journal_checkpoint_storage_bytes"`
	TemporaryRestoredStorageBytes        int64            `json:"temporary_restored_storage_bytes"`
	CostReport                           cost.Report      `json:"cost_report"`
}

type DSGRequestsNetworkReport struct {
	RequestsByType    map[string]uint64 `json:"requests_by_type"`
	RetrievalFees     string            `json:"retrieval_fees"`
	TransitionFees    string            `json:"transition_fees"`
	RestoreFees       string            `json:"restore_fees"`
	EarlyDeletionFees string            `json:"early_deletion_fees"`
	TrafficBytes      map[string]uint64 `json:"traffic_bytes,omitempty"`
	TrafficUSD        map[string]string `json:"traffic_usd,omitempty"`
}

type InfrastructureReport struct {
	ComputeOrEdgeVM       string `json:"compute_or_edge_vm"`
	DiskEBS               string `json:"disk_ebs"`
	ALBLCU                string `json:"alb_lcu"`
	DynamoDB              string `json:"dynamodb"`
	KMSCloudWatch         string `json:"kms_cloudwatch"`
	NATInterfaceEndpoints string `json:"nat_interface_endpoints"`
}

type RevivalCopyReport struct {
	RevivedChunks              *uint64 `json:"revived_chunks"`
	RevivedBytesStored         *uint64 `json:"revived_bytes_stored"`
	CopySourceDownloadBytes    uint64  `json:"copy_source_download_bytes"`
	CopyDestinationUploadBytes uint64  `json:"copy_destination_upload_bytes"`
	Note                       string  `json:"note"`
}

type CostReportResults struct {
	PricedSubtotalUSDExact   string   `json:"priced_subtotal_usd_exact"`
	CompleteTotalUSD         *string  `json:"complete_total_usd"`
	NetSavingUSDVsBaseline   *string  `json:"net_saving_usd_vs_baseline"`
	SavingsPercentVsBaseline *float64 `json:"savings_percent_vs_baseline"`
	IndicativeStorageDiffUSD *string  `json:"indicative_storage_difference_usd,omitempty"`
	BreakEven                *string  `json:"break_even"`
	RestoreAmplification     *float64 `json:"restore_amplification"`
	JournalWatermark         *uint64  `json:"journal_watermark,omitempty"`
	ExportLagSeconds         *float64 `json:"export_lag_seconds,omitempty"`
}

type CostReportResponse struct {
	ReportType         string                   `json:"report_type"`
	SchemaVersion      int                      `json:"schema_version"`
	Inputs             CostReportInputs         `json:"inputs"`
	Baseline           BaselineReport           `json:"baseline"`
	DSGStorage         DSGStorageReport         `json:"dsg_storage"`
	DSGRequestsNetwork DSGRequestsNetworkReport `json:"dsg_requests_network"`
	Infrastructure     InfrastructureReport     `json:"infrastructure"`
	RevivalCopy        RevivalCopyReport        `json:"revival_copy"`
	Results            CostReportResults        `json:"results"`
	Omissions          []string                 `json:"omissions"`
	Limitations        []string                 `json:"limitations"`
}

func (s *server) costReport(c *call) error {
	if err := c.admin(); err != nil {
		return err
	}
	q, err := query(c.r, "region", "as_of", "scenario", "profile", "from", "to", "bucket", "domain", "workload")
	if err != nil {
		return err
	}

	region := q.Get("region")
	if region == "" {
		region = "ap-southeast-1"
	}

	var catalog *pricing.Catalog
	if region == "ap-southeast-1" {
		cat, parseErr := pricing.Parse(pricingschema.DefaultCatalog())
		if parseErr != nil {
			return invalid("failed to parse default catalog: %v", parseErr)
		}
		catalog = cat
	} else {
		for _, p := range []string{
			fmt.Sprintf("configs/pricing/pricing.%s.example.json", region),
			fmt.Sprintf("../../configs/pricing/pricing.%s.example.json", region),
		} {
			if b, readErr := os.ReadFile(p); readErr == nil {
				if cat, parseErr := pricing.Parse(b); parseErr == nil {
					catalog = cat
					break
				}
			}
		}
	}
	if catalog == nil {
		return invalid("unsupported pricing region: %q", region)
	}

	asOf := q.Get("as_of")
	if asOf != "" {
		if _, parseErr := time.Parse("2006-01-02", asOf); parseErr != nil {
			return invalid("invalid as_of format %q, expected YYYY-MM-DD", asOf)
		}
		if asOf != catalog.AsOf {
			return invalid("pricing catalog for region %q as of %q is unavailable (available: %s)", region, asOf, catalog.AsOf)
		}
	} else {
		asOf = catalog.AsOf
	}

	scenario := q.Get("scenario")
	if scenario == "" {
		scenario = "A"
	}
	if scenario != "A" && scenario != "B" {
		return invalid("invalid scenario %q; expected A or B", scenario)
	}

	profile := q.Get("profile")
	if profile == "" {
		if scenario == "A" {
			profile = "cloud"
		} else {
			profile = "edge"
		}
	}
	if profile != "cloud" && profile != "edge" {
		return invalid("invalid profile %q; expected cloud or edge", profile)
	}

	if scenario == "A" && profile != "cloud" {
		return invalid("incompatible scenario/profile: scenario A requires profile cloud (got %q)", profile)
	}
	if scenario == "B" && profile != "edge" {
		return invalid("incompatible scenario/profile: scenario B requires profile edge (got %q)", profile)
	}

	domain := q.Get("domain")
	if domain != "" {
		return invalid("domain filter %q is not supported: per-domain physical allocation is unavailable", domain)
	}

	from := q.Get("from")
	to := q.Get("to")
	duration := 30 * 24 * time.Hour
	month := 30 * 24 * time.Hour
	bytesPerGB := uint64(1024 * 1024 * 1024)

	if from != "" || to != "" {
		if from == "" || to == "" {
			return invalid("both 'from' and 'to' parameters are required when specifying a time range")
		}
		tFrom, errFrom := time.Parse(time.RFC3339, from)
		if errFrom != nil {
			return invalid("invalid 'from' timestamp %q; expected RFC3339", from)
		}
		tTo, errTo := time.Parse(time.RFC3339, to)
		if errTo != nil {
			return invalid("invalid 'to' timestamp %q; expected RFC3339", to)
		}
		if !tTo.After(tFrom) {
			return invalid("'to' timestamp (%s) must be after 'from' timestamp (%s)", to, from)
		}
		duration = tTo.Sub(tFrom)
	} else {
		to = s.now().Format(time.RFC3339)
		from = s.now().Add(-duration).Format(time.RFC3339)
	}

	bucket := q.Get("bucket")
	workload := q.Get("workload")
	if workload != "" && workload != "live_gateway_snapshot" {
		if bucket != "" {
			return invalid("bucket filter %q is not supported for modeled workload simulation", bucket)
		}
		if q.Get("from") != "" || q.Get("to") != "" {
			return invalid("time range filters 'from' and 'to' are not supported for modeled workload simulation; workload traces define their own fixed timeline")
		}

		// Security: prevent path traversal and arbitrary filesystem reads.
		// Only explicit allowed workload identifiers are supported.
		var traceFileName string
		switch workload {
		case "cloud-tiering-boundary", "cloud-boundary":
			traceFileName = "cloud-boundary.json"
		case "edge-revival-accounting", "edge-revival":
			traceFileName = "edge-revival.json"
		default:
			return invalid("workload trace %q not found", workload)
		}

		var traceData []byte
		if embedded := traces.Get(traceFileName); len(embedded) > 0 {
			traceData = embedded
		} else {
			for _, dir := range []string{"benchmarks/traces", "../../benchmarks/traces", "../../../benchmarks/traces"} {
				p := filepath.Join(dir, traceFileName)
				if b, readErr := os.ReadFile(p); readErr == nil {
					traceData = b
					break
				}
			}
		}
		if traceData == nil {
			return invalid("workload trace %q not found", workload)
		}
		tr, parseErr := simulator.Read(bytes.NewReader(traceData))
		if parseErr != nil {
			return invalid("workload trace %q is invalid", workload)
		}
		from = tr.Start.UTC().Format(time.RFC3339)
		to = tr.Start.Add(time.Duration(tr.DurationNS)).UTC().Format(time.RFC3339)
		if tr.Profile != "" && tr.Profile != profile {
			return invalid("workload trace %q has profile %q, which is incompatible with requested profile %q (scenario %s)", workload, tr.Profile, profile, scenario)
		}
		comp, compErr := simulator.ComparePolicies(c.r.Context(), tr, catalog)
		if compErr != nil {
			return &apiError{http.StatusInternalServerError, "simulation_failed", compErr.Error()}
		}

		var directPol, candidatePol *simulator.PolicySummary
		for i := range comp.Policies {
			p := &comp.Policies[i]
			if p.Name == simulator.PolicyDirectS3 {
				directPol = p
			}
			if candidatePol == nil && p.Name != simulator.PolicyDirectS3 {
				if scenario == "A" && p.Name == simulator.PolicyWarmStandardOnly {
					candidatePol = p
				} else if scenario == "B" && p.Name == simulator.PolicyWarmGlacierIR {
					candidatePol = p
				}
			}
		}
		if candidatePol == nil && len(comp.Policies) > 1 {
			candidatePol = &comp.Policies[1]
		}
		if directPol == nil {
			return &apiError{http.StatusInternalServerError, "missing_baseline", "baseline direct_s3 policy missing in simulation"}
		}

		directStorage := "0"
		directReq := "0"
		directNet := "0"
		directRet := "0"
		directRest := "0"
		if directPol.Breakdown != nil {
			directStorage = directPol.Breakdown.StorageUSD
			directReq = directPol.Breakdown.RequestsUSD
			directNet = directPol.Breakdown.NetworkUSD
			directRet = directPol.Breakdown.RetrievalUSD
			directRest = directPol.Breakdown.RestoreUSD
		}

		candStorageMap := map[string]int64{}
		if len(candidatePol.StorageBytesByClass) > 0 {
			for class, b := range candidatePol.StorageBytesByClass {
				if b > 0 {
					candStorageMap[class] = int64(b)
				}
			}
		} else {
			for _, l := range candidatePol.Report.Lines {
				if strings.HasPrefix(l.RateID, "s3.storage.") {
					class := strings.ToUpper(strings.TrimPrefix(l.RateID, "s3.storage."))
					candStorageMap[class] = int64(candidatePol.PhysicalBytes)
				}
			}
		}

		reqByType := map[string]uint64{}
		retFees := "0"
		transFees := "0"
		restFees := "0"
		earlyFees := "0"
		var trafficBytes map[string]uint64
		if candidatePol.Breakdown != nil {
			reqByType = candidatePol.Breakdown.RequestsByType
			retFees = candidatePol.Breakdown.RetrievalUSD
			transFees = candidatePol.Breakdown.TransitionUSD
			earlyFees = candidatePol.Breakdown.EarlyDeletionUSD
			restFees = candidatePol.Breakdown.RestoreUSD
			trafficBytes = candidatePol.Breakdown.TrafficBytes
		}

		var ec2Rat, ebsRat, albRat, ddbRat, kmsRat, natRat big.Rat
		var hasEC2, hasEBS, hasALB, hasDDB, hasKMS, hasNAT bool

		for _, l := range candidatePol.Report.Lines {
			val, _ := new(big.Rat).SetString(l.USD)
			if val == nil {
				continue
			}
			switch {
			case strings.HasPrefix(l.RateID, "ec2.") || strings.HasPrefix(l.RateID, "edge.compute"):
				ec2Rat.Add(&ec2Rat, val)
				hasEC2 = true
			case strings.HasPrefix(l.RateID, "ebs.") || strings.HasPrefix(l.RateID, "edge.disk"):
				ebsRat.Add(&ebsRat, val)
				hasEBS = true
			case strings.HasPrefix(l.RateID, "alb."):
				albRat.Add(&albRat, val)
				hasALB = true
			case strings.HasPrefix(l.RateID, "ddb."):
				ddbRat.Add(&ddbRat, val)
				hasDDB = true
			case strings.HasPrefix(l.RateID, "kms.") || strings.HasPrefix(l.RateID, "cw.") ||
				strings.HasPrefix(l.RateID, "secret.") || strings.HasPrefix(l.RateID, "ssm."):
				kmsRat.Add(&kmsRat, val)
				hasKMS = true
			case strings.HasPrefix(l.RateID, "network.nat_") || strings.HasPrefix(l.RateID, "network.endpoint_") ||
				strings.HasPrefix(l.RateID, "network.public_ipv4"):
				natRat.Add(&natRat, val)
				hasNAT = true
			}
		}

		computeCost := "omitted"
		if hasEC2 {
			computeCost = ec2Rat.RatString()
		} else if scenario == "A" {
			computeCost = "omitted (cloud ec2)"
		} else {
			computeCost = "omitted (edge hardware)"
		}

		diskCost := "omitted"
		if hasEBS {
			diskCost = ebsRat.RatString()
		}

		albCost := "omitted"
		if hasALB {
			albCost = albRat.RatString()
		} else if scenario == "A" {
			albCost = "omitted (cloud alb)"
		} else {
			albCost = "not_applicable (edge deployment has no ALB)"
		}

		ddbCost := "omitted"
		if hasDDB {
			ddbCost = ddbRat.RatString()
		}

		kmsCost := "omitted"
		if hasKMS {
			kmsCost = kmsRat.RatString()
		}

		natCost := "omitted"
		if hasNAT {
			natCost = natRat.RatString()
		}

		response := CostReportResponse{
			ReportType:    "simulated_comparative_policy_report",
			SchemaVersion: 1,
			Inputs: CostReportInputs{
				Region:                catalog.Region,
				AsOf:                  catalog.AsOf,
				PricingSource:         catalog.Sources,
				Profile:               profile,
				Scenario:              scenario,
				From:                  from,
				To:                    to,
				Workload:              workload,
				AllocationMethodology: "modeled_trace_replay",
				Assumptions: []string{
					"This report is produced from deterministic offline trace simulation replay.",
					"Comparative accounting across candidates: direct_s3, warm_standard_only, warm_glacier_ir, warm_glacier_flexible.",
				},
			},
			Baseline: BaselineReport{
				DirectS3: DirectS3Breakdown{
					LogicalBytes:     directPol.LogicalBytes,
					StorageCostUSD:   directStorage,
					RequestsCostUSD:  directReq,
					RetrievalCostUSD: directRet,
					RestoreCostUSD:   directRest,
					NetworkCostUSD:   directNet,
					TotalCostUSD:     directPol.TotalCostUSD,
				},
				CostReport: directPol.Report,
			},
			DSGStorage: DSGStorageReport{
				StorageByClass:                       candStorageMap,
				DeadRetiredStorageBytes:              0,
				SidecarJournalCheckpointStorageBytes: 0,
				TemporaryRestoredStorageBytes:        int64(candidatePol.TemporaryRestoredBytes),
				CostReport:                           candidatePol.Report,
			},
			DSGRequestsNetwork: DSGRequestsNetworkReport{
				RequestsByType:    reqByType,
				RetrievalFees:     retFees,
				TransitionFees:    transFees,
				RestoreFees:       restFees,
				EarlyDeletionFees: earlyFees,
				TrafficBytes:      trafficBytes,
			},
			Infrastructure: InfrastructureReport{
				ComputeOrEdgeVM:       computeCost,
				DiskEBS:               diskCost,
				ALBLCU:                albCost,
				DynamoDB:              ddbCost,
				KMSCloudWatch:         kmsCost,
				NATInterfaceEndpoints: natCost,
			},
			RevivalCopy: RevivalCopyReport{
				RevivedChunks:              nil,
				RevivedBytesStored:         nil,
				CopySourceDownloadBytes:    0,
				CopyDestinationUploadBytes: 0,
				Note:                       "Revival and copy breakdowns are accounted in totals; not double charged.",
			},
			Results: CostReportResults{
				PricedSubtotalUSDExact:   candidatePol.Report.Subtotal,
				CompleteTotalUSD:         candidatePol.Report.CompleteTotalUSD,
				NetSavingUSDVsBaseline:   candidatePol.SavingsUSD,
				SavingsPercentVsBaseline: candidatePol.SavingsPercent,
				IndicativeStorageDiffUSD: candidatePol.IndicativeStorageDiffUSD,
				BreakEven:                candidatePol.BreakEven,
				RestoreAmplification:     candidatePol.RestoreAmplification,
			},
			Omissions:   comp.Omissions,
			Limitations: comp.Limitations,
		}

		c.w.Header().Set("Cache-Control", "no-store")
		c.json(http.StatusOK, response)
		return nil
	}

	if s.Statistics == nil {
		return &apiError{http.StatusServiceUnavailable, "unavailable", "statistics provider is unavailable"}
	}
	global, err := s.Statistics.Global(c.r.Context())
	if err != nil {
		return &apiError{http.StatusServiceUnavailable, "unavailable", "statistics backend is unavailable"}
	}

	var summary stats.Summary
	methodology := "global_inventory_snapshot"
	if bucket != "" {
		bs, err := s.Statistics.Bucket(c.r.Context(), bucket)
		if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrBucketNotFound) {
			return errBucketNotFound
		}
		if err != nil {
			return &apiError{http.StatusServiceUnavailable, "unavailable", "statistics backend is unavailable"}
		}
		summary = bs.Summary
		methodology = "bucket_inventory_snapshot"

		// Finding 1: If bucket summary has no per-class breakdown (PhysicalClasses is empty),
		// we only infer storage class from global when ALL conditions are strictly met:
		// 1. Global inventory is complete (global.PhysicalComplete is true).
		// 2. Both Global and Bucket share the exact same snapshot generation (non-empty SnapshotID match).
		// 3. Exactly one storage class exists in global.PhysicalClasses with positive bytes.
		// 4. The total bytes accounted in that single class exactly matches global.BytesStored (no unclassified bytes).
		// If ANY of these conditions cannot be proven, we do NOT infer a class;
		// the bucket's physical storage class remains UNKNOWN, unpriced, and omitted from indicative savings.
		if len(summary.PhysicalClasses) == 0 && summary.BytesStored != nil && *summary.BytesStored > 0 {
			if global.PhysicalComplete &&
				global.SnapshotID != "" &&
				global.SnapshotID == summary.SnapshotID &&
				global.BytesStored != nil &&
				*global.BytesStored > 0 &&
				len(global.PhysicalClasses) > 0 {
				var singleClass string
				var globalClassTotal int64
				singleClassCount := 0
				for cStr, gBytes := range global.PhysicalClasses {
					if gBytes > 0 {
						singleClass = cStr
						globalClassTotal += gBytes
						singleClassCount++
					}
				}
				if singleClassCount == 1 && globalClassTotal == *global.BytesStored {
					summary.PhysicalClasses = map[string]int64{
						singleClass: *summary.BytesStored,
					}
				}
			}
		}
	} else {
		summary = global
	}
	if summary.Methodology != "" {
		methodology = summary.Methodology
	}

	// 1. DSG Storage Usage
	dsgUsage := []cost.Usage{}
	storageByClass := map[string]int64{}
	hasUnpricedPhysicalInventory := false

	// Finding 2: Incomplete or unmeasured physical inventory must not be priced as $0.
	if !summary.PhysicalComplete {
		hasUnpricedPhysicalInventory = true
		dsgUsage = append(dsgUsage, cost.Usage{
			RateID:   "s3.storage.incomplete_inventory",
			Unit:     "GB-Mo",
			Quantity: "1",
		})
	}

	if summary.BytesStored == nil {
		hasUnpricedPhysicalInventory = true
		dsgUsage = append(dsgUsage, cost.Usage{
			RateID:   "s3.storage.unmeasured_inventory",
			Unit:     "GB-Mo",
			Quantity: "1",
		})
	}

	var totalAccountedPhysical int64
	if len(summary.PhysicalClasses) > 0 {
		for classStr, b := range summary.PhysicalClasses {
			if b <= 0 {
				continue
			}
			storageByClass[classStr] = b
			totalAccountedPhysical += b
			class := storage.StorageClass(classStr)
			if class.Validate() == nil {
				if u, e := cost.StorageUsage(class, uint64(b), duration, bytesPerGB, month); e == nil {
					dsgUsage = append(dsgUsage, u)
				}
			} else {
				// Unknown physical storage class!
				// Must NOT be ignored as $0, which creates fake storage savings.
				hasUnpricedPhysicalInventory = true
				qtyRat := new(big.Rat).SetFrac64(b, int64(bytesPerGB))
				durRat := new(big.Rat).SetFrac64(int64(duration), int64(month))
				gbMo := new(big.Rat).Mul(qtyRat, durRat)
				rateID := "s3.storage.unknown"
				if strings.TrimSpace(classStr) != "" {
					rateID = fmt.Sprintf("s3.storage.%s", strings.ToLower(classStr))
				}
				dsgUsage = append(dsgUsage, cost.Usage{
					RateID:   rateID,
					Unit:     "GB-Mo",
					Quantity: gbMo.RatString(),
				})
			}
		}
	}

	// Finding 2: Preserve physical bytes and handle unallocated / unaccounted bytes.
	// If the sum of physical bytes across classes is less than BytesStored
	// (e.g. unmeasured class distribution, rounding, or empty classes map),
	// the difference must NOT vanish into $0. It is recorded as UNKNOWN class and unpriced usage.
	if summary.BytesStored != nil && *summary.BytesStored > totalAccountedPhysical {
		unaccounted := *summary.BytesStored - totalAccountedPhysical
		hasUnpricedPhysicalInventory = true
		storageByClass["UNKNOWN"] += unaccounted
		qtyRat := new(big.Rat).SetFrac64(unaccounted, int64(bytesPerGB))
		durRat := new(big.Rat).SetFrac64(int64(duration), int64(month))
		gbMo := new(big.Rat).Mul(qtyRat, durRat)
		dsgUsage = append(dsgUsage, cost.Usage{
			RateID:   "s3.storage.unknown",
			Unit:     "GB-Mo",
			Quantity: gbMo.RatString(),
		})
	} else if summary.BytesStored != nil && totalAccountedPhysical > *summary.BytesStored {
		hasUnpricedPhysicalInventory = true
		dsgUsage = append(dsgUsage, cost.Usage{
			RateID:   "s3.storage.inconsistent_inventory",
			Unit:     "GB-Mo",
			Quantity: "1",
		})
	}

	dsgReport, err := cost.Evaluate(catalog, dsgUsage)
	if err != nil {
		return err
	}

	// 2. Direct S3 Baseline
	directUsage := []cost.Usage{}
	logicalBytes := uint64(0)
	if summary.BytesLogical > 0 {
		logicalBytes = uint64(summary.BytesLogical)
	}
	if logicalBytes > 0 {
		if u, e := cost.StorageUsage(storage.Standard, logicalBytes, duration, bytesPerGB, month); e == nil {
			directUsage = append(directUsage, u)
		}
	}
	directReport, err := cost.Evaluate(catalog, directUsage)
	if err != nil {
		return err
	}

	// 3. Savings Calculation:
	// Only conclude net savings if complete totals exist on both sides.
	// In partial prototype models, net savings remain null, and indicative storage difference is reported.
	var netSavingUSD *string
	var savingsPercent *float64
	var indicativeStorageDiffUSD *string
	if !hasUnpricedPhysicalInventory && len(directReport.Unpriced) == 0 && len(dsgReport.Unpriced) == 0 && logicalBytes > 0 {
		dRat, okD := new(big.Rat).SetString(directReport.Subtotal)
		dsgRat, okDSG := new(big.Rat).SetString(dsgReport.Subtotal)
		if okD && okDSG {
			diff := new(big.Rat).Sub(dRat, dsgRat)
			s := diff.RatString()
			indicativeStorageDiffUSD = &s

			if directReport.CompleteTotalUSD != nil && dsgReport.CompleteTotalUSD != nil && dRat.Sign() > 0 {
				diffFloat, _ := diff.Float64()
				dFloat, _ := dRat.Float64()
				pct := (diffFloat / dFloat) * 100.0
				netSavingUSD = &s
				savingsPercent = &pct
			}
		}
	}

	deadRetired := int64(0)
	temporaryRestored := int64(0)
	if summary.PhysicalLifecycle != nil {
		deadRetired = summary.PhysicalLifecycle["Dead"] + summary.PhysicalLifecycle["Retired"]
		temporaryRestored = summary.PhysicalLifecycle["Restored"]
	}

	computeCost := "omitted"
	albCost := "omitted"
	scenarioAssumption := ""
	if scenario == "A" {
		computeCost = "omitted (cloud ec2)"
		albCost = "omitted (cloud alb)"
		scenarioAssumption = "Scenario A (Cloud DSG): evaluates S3 storage snapshot; cloud compute/ALB/NAT infrastructure omitted."
	} else {
		computeCost = "omitted (edge hardware)"
		albCost = "not_applicable (edge deployment has no ALB)"
		scenarioAssumption = "Scenario B (Edge DSG): evaluates S3 storage snapshot; edge compute/WAN egress omitted, ALB not applicable."
	}

	omissions := []string{
		"Infrastructure compute/disk/ALB/DynamoDB/KMS costs omitted in live snapshot",
		"Request, transition, retrieval, and network egress fees omitted unless explicitly metered",
		"Cloud-client Internet baseline path must be declared separately; topology not inferred",
		"Net savings vs baseline omitted: cost evaluation is partial prototype without complete matched totals (CompleteTotalUSD is null)",
	}
	if hasUnpricedPhysicalInventory {
		omissions = append(omissions, "Indicative storage difference omitted: physical inventory is incomplete, inconsistent, unmeasured, or contains unknown storage classes")
	}

	response := CostReportResponse{
		ReportType:    "unscoped_partial_storage_snapshot",
		SchemaVersion: 1,
		Inputs: CostReportInputs{
			Region:                catalog.Region,
			AsOf:                  catalog.AsOf,
			PricingSource:         catalog.Sources,
			Profile:               profile,
			Scenario:              scenario,
			From:                  from,
			To:                    to,
			Workload:              "live_gateway_snapshot",
			Bucket:                bucket,
			Domain:                domain,
			AllocationMethodology: methodology,
			Assumptions: []string{
				"This endpoint operates in unscoped_partial_storage_snapshot mode: prices current gateway physical inventory piecewise constant across the requested range.",
				"Scenario and profile parameters indicate deployment context; live endpoint does not perform comparative multi-scenario network/compute cost simulation.",
				scenarioAssumption,
				"Storage byte-time evaluated assuming piecewise constant inventory across interval; current gateway inventory snapshot held constant across the requested range without replaying historical mutations.",
				"Baseline Direct S3 assumes 1:1 logical-to-physical ratio without dedup or compression.",
			},
		},
		Baseline: BaselineReport{
			DirectS3: DirectS3Breakdown{
				LogicalBytes:     logicalBytes,
				StorageCostUSD:   directReport.Subtotal,
				RequestsCostUSD:  "omitted",
				RetrievalCostUSD: "omitted",
				RestoreCostUSD:   "omitted",
				NetworkCostUSD:   "omitted",
				TotalCostUSD:     directReport.Subtotal,
			},
			CostReport: directReport,
		},
		DSGStorage: DSGStorageReport{
			StorageByClass:                       storageByClass,
			DeadRetiredStorageBytes:              deadRetired,
			SidecarJournalCheckpointStorageBytes: 0,
			TemporaryRestoredStorageBytes:        temporaryRestored,
			CostReport:                           dsgReport,
		},
		DSGRequestsNetwork: DSGRequestsNetworkReport{
			RequestsByType:    map[string]uint64{},
			RetrievalFees:     "omitted",
			TransitionFees:    "omitted",
			RestoreFees:       "omitted",
			EarlyDeletionFees: "omitted",
		},
		Infrastructure: InfrastructureReport{
			ComputeOrEdgeVM:       computeCost,
			DiskEBS:               "omitted",
			ALBLCU:                albCost,
			DynamoDB:              "omitted",
			KMSCloudWatch:         "omitted",
			NATInterfaceEndpoints: "omitted",
		},
		RevivalCopy: RevivalCopyReport{
			RevivedChunks:              summary.RevivedChunks,
			RevivedBytesStored:         summary.RevivedBytes,
			CopySourceDownloadBytes:    0,
			CopyDestinationUploadBytes: 0,
			Note:                       "Revival and copy breakdowns are omitted in live snapshot; unmeasured.",
		},
		Results: CostReportResults{
			PricedSubtotalUSDExact:   dsgReport.Subtotal,
			CompleteTotalUSD:         dsgReport.CompleteTotalUSD,
			NetSavingUSDVsBaseline:   netSavingUSD,
			SavingsPercentVsBaseline: savingsPercent,
			IndicativeStorageDiffUSD: indicativeStorageDiffUSD,
			BreakEven:                nil,
			RestoreAmplification:     nil,
		},
		Omissions: omissions,
		Limitations: []string{
			"Unscoped partial storage snapshot reflects live inventory only; comparative cross-scenario analysis requires simulated trace replay with explicit route models.",
			"Modelled comparative accounting prototype; does not reflect AWS invoice, Free Tier or account credits.",
			"Live snapshot prices current inventory held piecewise constant across range; historical inventory mutations require simulated trace replay.",
		},
	}

	c.w.Header().Set("Cache-Control", "no-store")
	c.json(http.StatusOK, response)
	return nil
}
