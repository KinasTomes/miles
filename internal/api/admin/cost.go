package admin

import (
	"fmt"
	"math/big"
	"net/http"
	"os"
	"time"

	pricingschema "github.com/KinasTomes/miles/configs/pricing"
	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
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
	LogicalBytes    uint64 `json:"logical_bytes"`
	StorageCostUSD  string `json:"storage_cost_usd"`
	RequestsCostUSD string `json:"requests_cost_usd"`
	NetworkCostUSD  string `json:"network_cost_usd"`
	TotalCostUSD    string `json:"total_cost_usd"`
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
	RevivedChunks              uint64 `json:"revived_chunks"`
	RevivedBytesStored         uint64 `json:"revived_bytes_stored"`
	CopySourceDownloadBytes    uint64 `json:"copy_source_download_bytes"`
	CopyDestinationUploadBytes uint64 `json:"copy_destination_upload_bytes"`
	Note                       string `json:"note"`
}

type CostReportResults struct {
	PricedSubtotalUSDExact   string   `json:"priced_subtotal_usd_exact"`
	CompleteTotalUSD         *string  `json:"complete_total_usd"`
	NetSavingUSDVsBaseline   *string  `json:"net_saving_usd_vs_baseline"`
	SavingsPercentVsBaseline *float64 `json:"savings_percent_vs_baseline"`
	BreakEven                *string  `json:"break_even"`
	RestoreAmplification     *float64 `json:"restore_amplification"`
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
	q, err := query(c.r, "region", "as_of", "scenario", "profile", "from", "to", "bucket", "domain")
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

	if s.Statistics == nil {
		return &apiError{http.StatusServiceUnavailable, "unavailable", "statistics provider is unavailable"}
	}
	global, err := s.Statistics.Global(c.r.Context())
	if err != nil {
		return &apiError{http.StatusServiceUnavailable, "unavailable", "statistics backend is unavailable"}
	}

	scenario := q.Get("scenario")
	if scenario == "" {
		scenario = "A"
	}
	profile := q.Get("profile")
	if profile == "" {
		profile = "cloud"
	}
	from := q.Get("from")
	to := q.Get("to")
	bucket := q.Get("bucket")
	domain := q.Get("domain")

	duration := 30 * 24 * time.Hour
	month := 30 * 24 * time.Hour
	bytesPerGB := uint64(1024 * 1024 * 1024)

	if from != "" && to != "" {
		tFrom, errFrom := time.Parse(time.RFC3339, from)
		tTo, errTo := time.Parse(time.RFC3339, to)
		if errFrom == nil && errTo == nil && tTo.After(tFrom) {
			duration = tTo.Sub(tFrom)
		}
	} else {
		to = s.now().Format(time.RFC3339)
		from = s.now().Add(-duration).Format(time.RFC3339)
	}

	// 1. DSG Storage Usage
	dsgUsage := []cost.Usage{}
	storageByClass := map[string]int64{}
	if len(global.PhysicalClasses) > 0 {
		for classStr, b := range global.PhysicalClasses {
			storageByClass[classStr] = b
			class := storage.StorageClass(classStr)
			if b > 0 && class.Validate() == nil {
				if u, e := cost.StorageUsage(class, uint64(b), duration, bytesPerGB, month); e == nil {
					dsgUsage = append(dsgUsage, u)
				}
			}
		}
	} else if global.BytesStored != nil && *global.BytesStored > 0 {
		storageByClass[string(storage.Standard)] = *global.BytesStored
		if u, e := cost.StorageUsage(storage.Standard, uint64(*global.BytesStored), duration, bytesPerGB, month); e == nil {
			dsgUsage = append(dsgUsage, u)
		}
	}

	dsgReport, err := cost.Evaluate(catalog, dsgUsage)
	if err != nil {
		return err
	}

	// 2. Direct S3 Baseline
	directUsage := []cost.Usage{}
	logicalBytes := uint64(0)
	if global.BytesLogical > 0 {
		logicalBytes = uint64(global.BytesLogical)
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

	// 3. Savings Calculation (only when both sides have zero unpriced items and logicalBytes > 0)
	var netSavingUSD *string
	var savingsPercent *float64
	if len(directReport.Unpriced) == 0 && len(dsgReport.Unpriced) == 0 && logicalBytes > 0 {
		dRat, okD := new(big.Rat).SetString(directReport.Subtotal)
		dsgRat, okDSG := new(big.Rat).SetString(dsgReport.Subtotal)
		if okD && okDSG && dRat.Sign() > 0 {
			diff := new(big.Rat).Sub(dRat, dsgRat)
			diffFloat, _ := diff.Float64()
			dFloat, _ := dRat.Float64()
			pct := (diffFloat / dFloat) * 100.0
			s := diff.RatString()
			netSavingUSD = &s
			savingsPercent = &pct
		}
	}

	deadRetired := int64(0)
	temporaryRestored := int64(0)
	if global.PhysicalLifecycle != nil {
		deadRetired = global.PhysicalLifecycle["Dead"] + global.PhysicalLifecycle["Retired"]
		temporaryRestored = global.PhysicalLifecycle["Restored"]
	}

	methodology := global.Methodology
	if methodology == "" {
		methodology = "global_inventory_snapshot"
	}

	response := CostReportResponse{
		ReportType:    "partial_prototype_cost_report",
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
				"Storage byte-time evaluated assuming piecewise constant inventory across interval.",
				"Baseline Direct S3 assumes 1:1 logical-to-physical ratio without dedup or compression.",
			},
		},
		Baseline: BaselineReport{
			DirectS3: DirectS3Breakdown{
				LogicalBytes:    logicalBytes,
				StorageCostUSD:  directReport.Subtotal,
				RequestsCostUSD: "0",
				NetworkCostUSD:  "0",
				TotalCostUSD:    directReport.Subtotal,
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
			RetrievalFees:     "0",
			TransitionFees:    "0",
			RestoreFees:       "0",
			EarlyDeletionFees: "0",
		},
		Infrastructure: InfrastructureReport{
			ComputeOrEdgeVM:       "omitted",
			DiskEBS:               "omitted",
			ALBLCU:                "omitted",
			DynamoDB:              "omitted",
			KMSCloudWatch:         "omitted",
			NATInterfaceEndpoints: "omitted",
		},
		RevivalCopy: RevivalCopyReport{
			RevivedChunks:              0,
			RevivedBytesStored:         0,
			CopySourceDownloadBytes:    0,
			CopyDestinationUploadBytes: 0,
			Note:                       "Revival and copy breakdowns are accounted in totals; not double charged.",
		},
		Results: CostReportResults{
			PricedSubtotalUSDExact:   dsgReport.Subtotal,
			CompleteTotalUSD:         dsgReport.CompleteTotalUSD,
			NetSavingUSDVsBaseline:   netSavingUSD,
			SavingsPercentVsBaseline: savingsPercent,
			BreakEven:                nil,
			RestoreAmplification:     nil,
		},
		Omissions: []string{
			"Infrastructure compute/disk/ALB/DynamoDB/KMS costs omitted in live snapshot",
			"Request, transition, retrieval, and network egress fees omitted unless explicitly metered",
			"Cloud-client Internet baseline path must be declared separately; topology not inferred",
		},
		Limitations: []string{
			"Modelled comparative accounting prototype; does not reflect AWS invoice, Free Tier or account credits.",
			"Live snapshot prices current inventory; historical time-warp requires simulated trace replay.",
		},
	}

	c.w.Header().Set("Cache-Control", "no-store")
	c.json(http.StatusOK, response)
	return nil
}
