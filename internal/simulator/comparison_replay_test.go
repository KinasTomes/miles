package simulator

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/cost"
	"github.com/KinasTomes/miles/internal/pricing"
	"github.com/KinasTomes/miles/internal/storage"
)

func TestComparePoliciesTimelineReplayMultiplePackTransitions(t *testing.T) {
	dayNS := int64(24 * time.Hour)

	baseTrace, catalog := fixture(t, "cloud-boundary")
	trace := baseTrace
	trace.Name = "multi-transition-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      2000 * 1024 * 1024,
				UniqueRaw:    1000 * 1024 * 1024,
				UniqueStored: 1000 * 1024 * 1024,
				Physical:     1000 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: 1000 * 1024 * 1024,
			},
		},
		{
			AtNS: 10 * dayNS,
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard:  600 * 1024 * 1024,
				storage.GlacierIR: 400 * 1024 * 1024,
			},
		},
		{
			AtNS: 20 * dayNS,
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard:  400 * 1024 * 1024,
				storage.GlacierIR: 600 * 1024 * 1024,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	// Verify WarmGlacierIR has both Standard and GlacierIR storage lines
	irPolicy := policies[PolicyWarmGlacierIR]
	var stdLine, irLine *cost.Line
	for i := range irPolicy.Report.Lines {
		line := &irPolicy.Report.Lines[i]
		if line.RateID == "s3.storage.standard" {
			stdLine = line
		}
		if line.RateID == "s3.storage.glacier_ir" {
			irLine = line
		}
	}

	if stdLine == nil || irLine == nil {
		t.Fatalf("expected both standard and glacier_ir storage lines, got: %+v", irPolicy.Report.Lines)
	}

	stdGBMo := stdLine.Quantity
	irGBMo := irLine.Quantity
	if stdGBMo != "125/192" || irGBMo != "125/384" {
		t.Fatalf("expected stdGBMo=125/192 and irGBMo=125/384, got std=%s, ir=%s", stdGBMo, irGBMo)
	}
}

func TestComparePoliciesEdgeRevivalNoSynthesizedTransition(t *testing.T) {
	trace, catalog := fixture(t, "edge-revival")
	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies edge revival failed: %v", err)
	}

	if len(res.Policies) != 4 {
		t.Fatalf("expected 4 policies, got %d", len(res.Policies))
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	flexPolicy := policies[PolicyWarmGlacierFlex]
	for _, line := range flexPolicy.Report.Lines {
		if line.RateID == "s3.transition_glacier" {
			t.Fatalf("unexpected synthesized s3.transition_glacier line in edge-revival: %+v", line)
		}
	}

	irPolicy := policies[PolicyWarmGlacierIR]
	for _, line := range irPolicy.Report.Lines {
		if line.RateID == "s3.transition_ir" {
			t.Fatalf("unexpected synthesized s3.transition_ir line in edge-revival: %+v", line)
		}
	}
}

func TestComparePoliciesStandardIAPreservedInWarmStandardOnly(t *testing.T) {
	dayNS := int64(24 * time.Hour)

	baseTrace, catalog := fixture(t, "cloud-boundary")
	trace := baseTrace
	trace.Name = "standard-ia-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1024 * 1024 * 1024,
				UniqueRaw:    1024 * 1024 * 1024,
				UniqueStored: 1024 * 1024 * 1024,
				Physical:     1024 * 1024 * 1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.StandardIA: 1024 * 1024 * 1024,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	warmStd := policies[PolicyWarmStandardOnly]
	foundStandardLine := false
	for _, line := range warmStd.Report.Lines {
		if line.RateID == "s3.storage.standard" {
			foundStandardLine = true
			if line.Quantity != "1" {
				t.Fatalf("expected 1 GB-Mo of s3.storage.standard from STANDARD_IA, got %s", line.Quantity)
			}
		}
	}
	if !foundStandardLine {
		t.Fatalf("missing s3.storage.standard line in warm_standard_only when inventory is STANDARD_IA: %+v", warmStd.Report.Lines)
	}
}

func TestComparePoliciesStorageRulesPaddingAndMetadataOverhead(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	// 1. Glacier IR with 1024 bytes (< 128 KiB min billable bytes)
	traceIR := baseTrace
	traceIR.Name = "ir-padding-test"
	traceIR.DurationNS = 30 * dayNS
	traceIR.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1024,
				UniqueRaw:    1024,
				UniqueStored: 1024,
				Physical:     1024,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: 1024,
			},
		},
	}

	resIR, err := ComparePolicies(context.Background(), traceIR, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies IR failed: %v", err)
	}
	irPol := resIR.Policies[2] // WarmGlacierIR
	var irLine *cost.Line
	for i := range irPol.Report.Lines {
		if irPol.Report.Lines[i].RateID == "s3.storage.glacier_ir" {
			irLine = &irPol.Report.Lines[i]
			break
		}
	}
	if irLine == nil {
		t.Fatalf("missing s3.storage.glacier_ir line: %+v", irPol.Report.Lines)
	}
	if irLine.Quantity != "1/8192" {
		t.Fatalf("expected padded quantity 1/8192 GB-Mo, got %s", irLine.Quantity)
	}

	// 2. Glacier Flexible with 1 MiB -> adds 8 KiB Standard metadata + 32 KiB Glacier metadata
	traceFlex := baseTrace
	traceFlex.Name = "flex-metadata-test"
	traceFlex.DurationNS = 30 * dayNS
	traceFlex.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1048576,
				UniqueRaw:    1048576,
				UniqueStored: 1048576,
				Physical:     1048576,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: 1048576,
			},
		},
	}

	resFlex, err := ComparePolicies(context.Background(), traceFlex, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies Flex failed: %v", err)
	}
	flexPol := resFlex.Policies[3] // WarmGlacierFlex
	var flexStdLine, flexGlacierLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			flexStdLine = &flexPol.Report.Lines[i]
		}
		if flexPol.Report.Lines[i].RateID == "s3.storage.glacier" {
			flexGlacierLine = &flexPol.Report.Lines[i]
		}
	}
	if flexStdLine == nil || flexGlacierLine == nil {
		t.Fatalf("expected both standard metadata and glacier lines in Flex: %+v", flexPol.Report.Lines)
	}
	if flexStdLine.Quantity != "1/131072" {
		t.Fatalf("expected standard metadata quantity 1/131072 GB-Mo, got %s", flexStdLine.Quantity)
	}
	if flexGlacierLine.Quantity != "33/32768" {
		t.Fatalf("expected glacier storage quantity 33/32768 GB-Mo, got %s", flexGlacierLine.Quantity)
	}
}

func TestComparePoliciesTemporaryRestoredStandard(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "temporary-restored-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      1073741824,
				UniqueRaw:    1073741824,
				UniqueStored: 1073741824,
				Physical:     1073741824,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: 1073741824,
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	var stdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			stdLine = &flexPol.Report.Lines[i]
			break
		}
	}
	if stdLine == nil {
		t.Fatalf("expected standard storage line from restore and metadata: %+v", flexPol.Report.Lines)
	}
	if stdLine.USD == "" {
		t.Fatal("empty standard storage USD")
	}
}

func TestComparePoliciesTemporaryRestoredStandardHonorsAutoRestoreDays(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "temp-restored-auto-restore-days-3"
	trace.DurationNS = 30 * dayNS
	oneGiB := uint64(1024 * 1024 * 1024)

	// Set policy auto_restore_days = 3
	trace.Policy = json.RawMessage(`{
		"tiering": {
			"enabled": true,
			"warm_storage_class": "STANDARD",
			"cold_storage_class": "GLACIER",
			"cold_after_days": 30,
			"min_pack_live_ratio": 0.8,
			"auto_restore": true,
			"auto_restore_days": 3,
			"auto_restore_tier": "Standard"
		},
		"compression": {
			"enabled": true,
			"codec": "zstd",
			"level": 3,
			"min_saving_ratio": 0.1
		},
		"chunking": {
			"min_bytes": 262144,
			"avg_bytes": 1048576,
			"max_bytes": 4194304
		}
	}`)

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      oneGiB,
				UniqueRaw:    oneGiB,
				UniqueStored: oneGiB,
				Physical:     oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	var stdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			stdLine = &flexPol.Report.Lines[i]
			break
		}
	}
	if stdLine == nil {
		t.Fatalf("expected standard storage line from restore and metadata: %+v", flexPol.Report.Lines)
	}

	// For 1 GiB for 3 days in a 30-day month: 1 * 3/30 = 0.1 GB-Mo = 1/10 GB-Mo
	// Plus standard storage metadata overhead 1/131072 GB-Mo = 65541/655360 GB-Mo
	qRat, ok := new(big.Rat).SetString(stdLine.Quantity)
	if !ok {
		t.Fatalf("invalid quantity: %s", stdLine.Quantity)
	}
	expectedRat := big.NewRat(65541, 655360)
	if qRat.Cmp(expectedRat) != 0 {
		t.Fatalf("expected 3-day standard storage quantity %s, got %s", expectedRat.RatString(), qRat.RatString())
	}
	if flexPol.TemporaryRestoredBytes != oneGiB {
		t.Fatalf("expected TemporaryRestoredBytes %d, got %d", oneGiB, flexPol.TemporaryRestoredBytes)
	}
}

func TestComparePoliciesRestoreCappedToRemainingTraceDuration(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "restore-time-cap-test"
	trace.DurationNS = 30 * dayNS
	restoreAtNS := trace.DurationNS - int64(2*time.Hour)
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: 1073741824,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: 1073741824,
			},
		},
		{
			AtNS: restoreAtNS,
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3] // WarmGlacierFlex
	var stdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			stdLine = &flexPol.Report.Lines[i]
			break
		}
	}
	if stdLine == nil {
		t.Fatal("expected s3.storage.standard line for temporary restored Standard copy")
	}

	expectedRat := new(big.Rat).SetFrac64(2, 720)
	actualRat, ok := new(big.Rat).SetString(stdLine.Quantity)
	if !ok {
		t.Fatalf("invalid quantity rational %q", stdLine.Quantity)
	}
	metadataRat := new(big.Rat).SetFrac64(8192, 1073741824)
	restoreRat := new(big.Rat).Sub(actualRat, metadataRat)
	if restoreRat.Cmp(expectedRat) != 0 {
		t.Fatalf("expected %s GB-Mo for 2 hours restore copy, got %s (actual total: %s)", expectedRat.RatString(), restoreRat.RatString(), actualRat.RatString())
	}
}

func TestComparePoliciesEarlyDeletionAccrued(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "early-deletion-test"
	trace.DurationNS = 30 * dayNS
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trace.Start = startTime

	packBytes := int64(100 * 1024 * 1024) // 100 MiB
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: uint64(packBytes),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(packBytes),
			},
		},
		{
			AtNS: 10 * dayNS, // Observed as Deleting at day 10 (GLACIER_IR has 90-day minimum retention)
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "same-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              packBytes,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: packBytes,
			},
		},
		{
			AtNS: 10 * dayNS, // Second observation as Retiring for the same pack identity and generation
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "same-pack", Generation: "g1"},
					State:             "Retiring",
					Size:              packBytes,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: packBytes,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2] // WarmGlacierIR
	if irPol.Breakdown == nil {
		t.Fatal("expected breakdown")
	}
	if irPol.Breakdown.EarlyDeletionUSD == "0" || irPol.Breakdown.EarlyDeletionUSD == "" {
		t.Fatalf("expected early deletion fees to be accrued, got %q", irPol.Breakdown.EarlyDeletionUSD)
	}

	// Verify deduplication: single charge for 100 MiB deleted at day 10 (80 days unexpired out of 90)
	minRetention := 90 * 24 * time.Hour
	monthDuration := 30 * 24 * time.Hour
	uSingle, err := cost.EarlyDeletionUsage(storage.GlacierIR, uint64(packBytes), 10*24*time.Hour, minRetention, trace.BytesPerGB, monthDuration)
	if err != nil {
		t.Fatal(err)
	}
	singleReport, err := cost.Evaluate(catalog, []cost.Usage{uSingle})
	if err != nil {
		t.Fatal(err)
	}

	if irPol.Breakdown.EarlyDeletionUSD != singleReport.Subtotal {
		t.Fatalf("expected single early deletion charge (%s), got %s (did it charge duplicate lifecycle observations?)", singleReport.Subtotal, irPol.Breakdown.EarlyDeletionUSD)
	}

	// Two distinct packs with identical size without pack Ref (ID == "")
	traceTwoPacks := baseTrace
	traceTwoPacks.Name = "two-packs-without-ref"
	traceTwoPacks.DurationNS = 30 * dayNS
	traceTwoPacks.Start = startTime
	traceTwoPacks.BytesPerGB = 1024 * 1024 * 1024

	oneGiB := int64(1024 * 1024 * 1024)
	traceTwoPacks.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  uint64(2 * oneGiB),
				Physical: uint64(2 * oneGiB),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(2 * oneGiB),
			},
		},
		{
			AtNS: 10 * dayNS, // Pack 1 deleted at day 10
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "pack-1", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 10 * dayNS, // Pack 2 deleted at day 10 - distinct pack identity
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "pack-2", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}

	resTwo, err := ComparePolicies(context.Background(), traceTwoPacks, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	irTwo := resTwo.Policies[2] // WarmGlacierIR
	if irTwo.Breakdown.EarlyDeletionUSD != "2/75" {
		t.Fatalf("expected early deletion fee '2/75' for two 1GiB packs, got %q", irTwo.Breakdown.EarlyDeletionUSD)
	}

	// Two observations (Deleting + Retiring) of the same pack must be charged exactly once (idempotent)
	traceDupObservation := traceTwoPacks
	traceDupObservation.Name = "dup-observation-early-deletion"
	traceDupObservation.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: uint64(oneGiB), Physical: uint64(oneGiB)},
			StorageBytes: map[storage.StorageClass]uint64{storage.GlacierIR: uint64(oneGiB)},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "single-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "single-pack", Generation: "g1"},
					State:             "Retiring",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}
	resDup, err := ComparePolicies(context.Background(), traceDupObservation, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resDup.Policies[2].Breakdown.EarlyDeletionUSD != "1/75" {
		t.Fatalf("expected single charge '1/75' for duplicate observations of same pack, got %q", resDup.Policies[2].Breakdown.EarlyDeletionUSD)
	}

	// Pack without Ref.ID cannot prove identity; must not be charged early deletion
	traceNoRef := traceTwoPacks
	traceNoRef.Name = "no-ref-early-deletion"
	traceNoRef.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: uint64(oneGiB), Physical: uint64(oneGiB)},
			StorageBytes: map[storage.StorageClass]uint64{storage.GlacierIR: uint64(oneGiB)},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}
	resNoRef, err := ComparePolicies(context.Background(), traceNoRef, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resNoRef.Policies[2].Breakdown.EarlyDeletionUSD != "0" {
		t.Fatalf("expected 0 early deletion for pack without Ref identity, got %q", resNoRef.Policies[2].Breakdown.EarlyDeletionUSD)
	}

	// A pack with the same Ref changing storage class
	traceClassChange := baseTrace
	traceClassChange.Name = "class-change-early-deletion"
	traceClassChange.DurationNS = 30 * dayNS
	traceClassChange.Start = startTime
	traceClassChange.BytesPerGB = 1024 * 1024 * 1024

	traceClassChange.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  uint64(oneGiB),
				Physical: uint64(oneGiB),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(oneGiB),
			},
		},
		{
			AtNS: 10 * dayNS, // Pack retiring from Glacier IR at day 10
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "shared-pack", Generation: "g1"},
					State:             "Retiring",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 11 * dayNS, // Same pack deleting from Glacier Flexible at day 11
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "shared-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.Glacier,
					MinRetentionUntil: startTime.Add(10 * 24 * time.Hour).Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}

	resChange, err := ComparePolicies(context.Background(), traceClassChange, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	irChange := resChange.Policies[2]   // WarmGlacierIR
	flexChange := resChange.Policies[3] // WarmGlacierFlex

	if irChange.Breakdown.EarlyDeletionUSD == "0" || irChange.Breakdown.EarlyDeletionUSD == "" {
		t.Fatalf("expected Glacier IR early deletion fee to be charged, got %q", irChange.Breakdown.EarlyDeletionUSD)
	}
	if flexChange.Breakdown.EarlyDeletionUSD == "0" || flexChange.Breakdown.EarlyDeletionUSD == "" {
		t.Fatalf("expected Glacier Flexible early deletion fee to be charged, got %q", flexChange.Breakdown.EarlyDeletionUSD)
	}
}

func TestComparePoliciesGlacierPerPackRules(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "two-cold-packs"
	trace.DurationNS = 30 * dayNS
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trace.Start = startTime

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2048,
				Physical: 2048,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: 2048,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-1", Generation: "g1"},
					Size:  1024,
					Class: storage.GlacierIR,
					State: "Cold",
				},
				LiveBytes: 1024,
			},
		},
		{
			AtNS: 0,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-2", Generation: "g1"},
					Size:  1024,
					Class: storage.GlacierIR,
					State: "Cold",
				},
				LiveBytes: 1024,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2] // WarmGlacierIR
	var irLine *cost.Line
	for i := range irPol.Report.Lines {
		if irPol.Report.Lines[i].RateID == "s3.storage.glacier_ir" {
			irLine = &irPol.Report.Lines[i]
			break
		}
	}
	if irLine == nil {
		t.Fatalf("missing s3.storage.glacier_ir line: %+v", irPol.Report.Lines)
	}

	if irLine.Quantity != "1/4096" {
		t.Fatalf("expected 2 packs padded to 256 KiB (1/4096 GB-Mo), got %s", irLine.Quantity)
	}

	flexPol := res.Policies[3] // WarmGlacierFlex
	var flexGlacierLine, flexStdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.glacier" {
			flexGlacierLine = &flexPol.Report.Lines[i]
		}
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			flexStdLine = &flexPol.Report.Lines[i]
		}
	}
	if flexGlacierLine == nil || flexStdLine == nil {
		t.Fatalf("missing flex lines: %+v", flexPol.Report.Lines)
	}

	if flexGlacierLine.Quantity != "33/524288" {
		t.Fatalf("expected 2 packs glacier metadata quantity 33/524288, got %s", flexGlacierLine.Quantity)
	}
	if flexStdLine.Quantity != "1/65536" {
		t.Fatalf("expected 2 packs standard metadata quantity 1/65536, got %s", flexStdLine.Quantity)
	}
}

func TestComparePoliciesEarlyDeletionExpiredRetentionNoRecharge(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	trace := baseTrace
	trace.Name = "expired-retention-test"
	trace.DurationNS = 100 * dayNS
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	trace.Start = startTime
	packBytes := int64(100 * 1024 * 1024)

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  1073741824,
				Physical: uint64(packBytes),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(packBytes),
			},
		},
		{
			AtNS: 95 * dayNS, // Deleted at day 95, where MinRetentionUntil was day 90 (expired!)
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "expired-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              packBytes,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour), // Retention expired 5 days ago!
					LastAccess:        startTime.Add(94 * 24 * time.Hour), // LastAccess was day 94
				},
				LiveBytes: packBytes,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2] // WarmGlacierIR
	if irPol.Breakdown == nil {
		t.Fatal("expected breakdown")
	}
	if irPol.Breakdown.EarlyDeletionUSD != "0" {
		t.Fatalf("expected EarlyDeletionUSD to be '0' for pack deleted after expired retention, got %q", irPol.Breakdown.EarlyDeletionUSD)
	}

	// Deadline set further than minimum retention (e.g. 120 days for 90-day rule) clamped to 90 days
	oneGiB := int64(1024 * 1024 * 1024)
	traceClamp := baseTrace
	traceClamp.Name = "clamp-retention-test"
	traceClamp.DurationNS = 30 * dayNS
	traceClamp.Start = startTime
	traceClamp.BytesPerGB = 1024 * 1024 * 1024
	traceClamp.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: uint64(oneGiB), Physical: uint64(oneGiB)},
			StorageBytes: map[storage.StorageClass]uint64{storage.GlacierIR: uint64(oneGiB)},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "clamp-pack", Generation: "g1"},
					State:             "Deleting",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(120 * 24 * time.Hour), // 120 days > 90 days!
				},
				LiveBytes: oneGiB,
			},
		},
	}
	resClamp, err := ComparePolicies(context.Background(), traceClamp, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resClamp.Policies[2].Breakdown.EarlyDeletionUSD != "3/200" {
		t.Fatalf("expected early deletion fee clamped to maximum 90 days '3/200', got %q", resClamp.Policies[2].Breakdown.EarlyDeletionUSD)
	}
}

func TestComparePoliciesStalePhysicalClassMapInconsistency(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	oneGiB := uint64(1024 * 1024 * 1024)
	trace := baseTrace
	trace.Name = "stale-inventory-test"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard: oneGiB,
			},
		},
		{
			AtNS: 15 * dayNS,
			Snapshot: &cost.Snapshot{
				Logical:  oneGiB,
				Physical: 2 * oneGiB, // Physical increased 1 -> 2 GiB
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	warmStd := res.Policies[1]
	if warmStd.PhysicalBytes != 2*oneGiB {
		t.Fatalf("expected summary PhysicalBytes to be updated to 2 GiB (%d), got %d", 2*oneGiB, warmStd.PhysicalBytes)
	}
	if warmStd.StorageBytesByClass["STANDARD"] != 2*oneGiB {
		t.Fatalf("expected warmStd class map STANDARD to be 2 GiB, got %d", warmStd.StorageBytesByClass["STANDARD"])
	}
	if warmStd.Report.CompleteTotalUSD != nil {
		t.Fatalf("expected CompleteTotalUSD to be nil on inconsistent inventory, got %v", *warmStd.Report.CompleteTotalUSD)
	}
	if warmStd.SavingsUSD != nil {
		t.Fatalf("expected SavingsUSD to be nil on inconsistent inventory, got %v", *warmStd.SavingsUSD)
	}

	foundInconsistentOmission := false
	for _, om := range res.Omissions {
		if strings.Contains(om, "Storage inventory inconsistent") {
			foundInconsistentOmission = true
			break
		}
	}
	if !foundInconsistentOmission {
		t.Fatalf("expected storage inventory inconsistent omission, got: %v", res.Omissions)
	}

	// Inconsistency at exact trace.DurationNS (end-of-trace)
	traceEndInconsistent := baseTrace
	traceEndInconsistent.Name = "end-of-trace-inconsistent"
	traceEndInconsistent.DurationNS = 30 * dayNS
	traceEndInconsistent.Omissions = []string{"Full verified observation"}
	for i := range catalog.Rates {
		switch catalog.Rates[i].ID {
		case "transfer.internet_egress":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.09"}}
		case "ec2.compute":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.0416"}}
		case "ebs.gp3":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.08"}}
		}
	}
	traceEndInconsistent.Events = []Event{
		{
			AtNS:         0,
			Snapshot:     &cost.Snapshot{Logical: oneGiB, Physical: oneGiB},
			StorageBytes: map[storage.StorageClass]uint64{storage.Standard: oneGiB},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 0, cost.GatewayClient: 0, cost.GatewayS3: 0, cost.S3Gateway: 0,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "10"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "720"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "20"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "720"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "720"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "10"},
			},
		},
		{
			AtNS:         30 * dayNS,
			Snapshot:     &cost.Snapshot{Logical: oneGiB, Physical: 2 * oneGiB},
			StorageBytes: map[storage.StorageClass]uint64{storage.Standard: oneGiB},
		},
	}
	resEnd, err := ComparePolicies(context.Background(), traceEndInconsistent, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resEnd.Policies[1].Report.CompleteTotalUSD != nil {
		t.Fatalf("expected CompleteTotalUSD to be nil on end-of-trace inconsistent inventory, got %v", *resEnd.Policies[1].Report.CompleteTotalUSD)
	}
	if resEnd.Policies[1].SavingsUSD != nil {
		t.Fatalf("expected SavingsUSD to be nil on end-of-trace inconsistent inventory, got %v", *resEnd.Policies[1].SavingsUSD)
	}
}

func TestComparePoliciesStandardPackDeletionWithoutRefDoesNotSuppressGlacierCompleteness(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	for i := range catalog.Rates {
		switch catalog.Rates[i].ID {
		case "transfer.internet_egress":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.09"}}
		case "ec2.compute":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.0416"}}
		case "ebs.gp3":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.08"}}
		case "s3.storage.glacier_ir":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.005"}}
		case "s3.storage.glacier":
			catalog.Rates[i].Status = "verified"
			catalog.Rates[i].Tiers = []pricing.Tier{{Begin: "0", End: nil, USD: "0.004"}}
		}
	}

	oneGiB := uint64(1024 * 1024 * 1024)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	trace := baseTrace
	trace.Name = "std-pack-deletion-without-ref"
	trace.DurationNS = 30 * dayNS
	trace.Start = startTime
	trace.Omissions = []string{"Full verified observation"}

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  2 * oneGiB,
				Physical: oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Standard:  512 * 1024 * 1024,
				storage.GlacierIR: 512 * 1024 * 1024,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "ir-pack-1", Generation: "g1"},
					State:             "Cold",
					Size:              int64(512 * 1024 * 1024),
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: int64(512 * 1024 * 1024),
			},
			Traffic: map[cost.Route]uint64{
				cost.ClientGateway: 0, cost.GatewayClient: 0, cost.GatewayS3: 0, cost.S3Gateway: 0,
			},
			Usage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
				{RateID: "ec2.compute", Unit: "Hrs", Quantity: "720"},
				{RateID: "ebs.gp3", Unit: "GB-Mo", Quantity: "20"},
				{RateID: "alb.hour", Unit: "Hrs", Quantity: "720"},
				{RateID: "alb.lcu", Unit: "LCU-Hrs", Quantity: "720"},
				{RateID: "ddb.storage", Unit: "GB-Mo", Quantity: "1"},
				{RateID: "ddb.write", Unit: "WriteRequestUnits", Quantity: "100"},
			},
			DirectUsage: []cost.Usage{
				{RateID: "s3.put_standard", Unit: "Requests", Quantity: "100"},
			},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					State: "Deleting",
					Size:  int64(64 * 1024 * 1024),
					Class: storage.Standard,
				},
				LiveBytes: int64(64 * 1024 * 1024),
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.Report.CompleteTotalUSD == nil {
		t.Fatal("expected CompleteTotalUSD to be non-nil for Glacier IR when deleted pack is Standard without Ref.ID")
	}
	if irPol.SavingsUSD == nil {
		t.Fatal("expected SavingsUSD to be non-nil for Glacier IR")
	}
}

func TestComparePoliciesEarlyDeletionAccruedAcrossLifecycleProgression(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")

	oneGiB := int64(1024 * 1024 * 1024)
	startTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	trace := baseTrace
	trace.Name = "lifecycle-progression-early-deletion"
	trace.DurationNS = 30 * dayNS
	trace.Start = startTime
	trace.Omissions = []string{"Full verified observation"}

	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  uint64(oneGiB),
				Physical: uint64(oneGiB),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(oneGiB),
			},
		},
		{
			AtNS: 5 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-progression", Generation: "g1"},
					State: "Retiring",
					Size:  oneGiB,
					Class: storage.GlacierIR,
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 10 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "pack-progression", Generation: "g1"},
					State:             "Deleted",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
		{
			AtNS: 12 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:               storage.PackRef{ID: "pack-progression", Generation: "g1"},
					State:             "Retired",
					Size:              oneGiB,
					Class:             storage.GlacierIR,
					MinRetentionUntil: startTime.Add(90 * 24 * time.Hour),
				},
				LiveBytes: oneGiB,
			},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	irPol := res.Policies[2]
	if irPol.Breakdown.EarlyDeletionUSD != "1/75" {
		t.Fatalf("expected early deletion fee to be '1/75', got %q", irPol.Breakdown.EarlyDeletionUSD)
	}

	traceUnresolved := trace
	traceUnresolved.Name = "unresolved-retention-deadline"
	traceUnresolved.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:  uint64(oneGiB),
				Physical: uint64(oneGiB),
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.GlacierIR: uint64(oneGiB),
			},
		},
		{
			AtNS: 5 * dayNS,
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-unresolved", Generation: "g1"},
					State: "Retiring",
					Size:  oneGiB,
					Class: storage.GlacierIR,
				},
				LiveBytes: oneGiB,
			},
		},
	}
	resUnresolved, err := ComparePolicies(context.Background(), traceUnresolved, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}
	if resUnresolved.Policies[2].Report.CompleteTotalUSD != nil {
		t.Fatalf("expected CompleteTotalUSD to be nil when cold pack retention deadline is never resolved, got %v", *resUnresolved.Policies[2].Report.CompleteTotalUSD)
	}
}

func TestComparePoliciesTemporaryRestoredStandardHonorsAuthoritativeShorterExpiry(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")
	oneGiB := uint64(1024 * 1024 * 1024)

	// Reviewer Finding [P2] (HEAD e834d29 audit):
	// Restore initial expiry: 4 hours.
	// Observation confirms: 2 hours.
	// Standard GB-Mo must reflect 2 hours (not 4 hours).
	trace := baseTrace
	trace.Name = "temp-restore-shorter-expiry"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      oneGiB,
				UniqueRaw:    oneGiB,
				UniqueStored: oneGiB,
				Physical:     oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-a", Generation: "gen-1"},
					State:         "Restored",
					Class:         storage.Glacier,
					RestoredUntil: trace.Start.Add(4 * time.Hour), // initial: 4 hours
				},
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(time.Hour), // hour 1: observation confirms shorter expiry of 2 hours
			Snapshot: &cost.Snapshot{
				Logical:      oneGiB,
				UniqueRaw:    oneGiB,
				UniqueStored: oneGiB,
				Physical:     oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-a", Generation: "gen-1"},
					State:         "Restored",
					Class:         storage.Glacier,
					RestoredUntil: trace.Start.Add(2 * time.Hour), // updated: 2 hours
				},
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	var stdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			stdLine = &flexPol.Report.Lines[i]
			break
		}
	}
	if stdLine == nil {
		t.Fatal("expected s3.storage.standard line in WarmGlacierFlex")
	}

	qRat, ok := new(big.Rat).SetString(stdLine.Quantity)
	if !ok {
		t.Fatalf("invalid quantity: %s", stdLine.Quantity)
	}

	// 2 hours of 1 GiB in 720 hours = 2/720 = 1/360 GB-Mo
	// Plus standard storage metadata overhead 1/131072 GB-Mo = 16429/5898240 GB-Mo
	expected2hRat := new(big.Rat).Add(big.NewRat(2, 720), big.NewRat(1, 131072))
	if qRat.Cmp(expected2hRat) != 0 {
		t.Fatalf("expected 2-hour standard storage quantity %s, got %s", expected2hRat.RatString(), qRat.RatString())
	}

	// Make sure it did NOT charge 4 hours (which would be 4/720 + 1/131072)
	wrong4hRat := new(big.Rat).Add(big.NewRat(4, 720), big.NewRat(1, 131072))
	if qRat.Cmp(wrong4hRat) == 0 {
		t.Fatalf("standard storage wrongly retained 4-hour duration %s", wrong4hRat.RatString())
	}
}

func TestComparePoliciesTemporaryRestoredStandardColdTransitionEndsStorage(t *testing.T) {
	dayNS := int64(24 * time.Hour)
	baseTrace, catalog := fixture(t, "cloud-boundary")
	oneGiB := uint64(1024 * 1024 * 1024)

	trace := baseTrace
	trace.Name = "temp-restore-cold-transition"
	trace.DurationNS = 30 * dayNS
	trace.Events = []Event{
		{
			AtNS: 0,
			Snapshot: &cost.Snapshot{
				Logical:      oneGiB,
				UniqueRaw:    oneGiB,
				UniqueStored: oneGiB,
				Physical:     oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:           storage.PackRef{ID: "pack-a", Generation: "gen-1"},
					State:         "Restored",
					Class:         storage.Glacier,
					RestoredUntil: trace.Start.Add(4 * time.Hour),
				},
			},
			Usage: []cost.Usage{
				{RateID: "s3.restore_standard_gb", Unit: "GB", Quantity: "1"},
			},
		},
		{
			AtNS: int64(time.Hour), // hour 1: observed in Cold state (restore ended)
			Snapshot: &cost.Snapshot{
				Logical:      oneGiB,
				UniqueRaw:    oneGiB,
				UniqueStored: oneGiB,
				Physical:     oneGiB,
			},
			StorageBytes: map[storage.StorageClass]uint64{
				storage.Glacier: oneGiB,
			},
			Tiering: &Tiering{
				Pack: storage.PackInfo{
					Ref:   storage.PackRef{ID: "pack-a", Generation: "gen-1"},
					State: "Cold",
					Class: storage.Glacier,
				},
			},
			Usage: []cost.Usage{},
		},
	}

	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	flexPol := res.Policies[3]
	var stdLine *cost.Line
	for i := range flexPol.Report.Lines {
		if flexPol.Report.Lines[i].RateID == "s3.storage.standard" {
			stdLine = &flexPol.Report.Lines[i]
			break
		}
	}
	if stdLine == nil {
		t.Fatal("expected s3.storage.standard line in WarmGlacierFlex")
	}

	qRat, ok := new(big.Rat).SetString(stdLine.Quantity)
	if !ok {
		t.Fatalf("invalid quantity: %s", stdLine.Quantity)
	}

	// 1 hour of 1 GiB in 720 hours = 1/720 GB-Mo
	// Plus standard storage metadata overhead 1/131072 GB-Mo
	expected1hRat := new(big.Rat).Add(big.NewRat(1, 720), big.NewRat(1, 131072))
	if qRat.Cmp(expected1hRat) != 0 {
		t.Fatalf("expected 1-hour standard storage quantity %s, got %s", expected1hRat.RatString(), qRat.RatString())
	}
}
