package simulator

import (
	"context"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestComparePoliciesFourPoliciesDeterminismAndUnpricedOmission(t *testing.T) {
	trace, catalog := fixture(t, "cloud-boundary")
	res, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("ComparePolicies failed: %v", err)
	}

	if res.Baseline != PolicyDirectS3 {
		t.Fatalf("expected baseline %v, got %v", PolicyDirectS3, res.Baseline)
	}

	if len(res.Policies) != 4 {
		t.Fatalf("expected 4 policies, got %d", len(res.Policies))
	}

	policies := map[PolicyName]PolicySummary{}
	for _, p := range res.Policies {
		policies[p.Name] = p
	}

	for _, name := range []PolicyName{PolicyDirectS3, PolicyWarmStandardOnly, PolicyWarmGlacierIR, PolicyWarmGlacierFlex} {
		p, ok := policies[name]
		if !ok {
			t.Fatalf("missing policy %s", name)
		}
		if p.TotalCostUSD == "" {
			t.Fatalf("policy %s has empty total cost", name)
		}
	}

	warmStd := policies[PolicyWarmStandardOnly]
	// Finding 1 regression: cloud-boundary has unpriced items (transfer.internet_egress).
	// Savings must NOT be concluded from partial priced subtotals!
	if warmStd.SavingsUSD != nil || warmStd.SavingsPercent != nil {
		t.Fatalf("expected savings to be nil when unpriced items exist, got USD=%v, Pct=%v", warmStd.SavingsUSD, warmStd.SavingsPercent)
	}

	// Verify omission recorded
	hasOmission := false
	for _, om := range res.Omissions {
		if strings.Contains(om, "Net savings and savings percent vs baseline omitted") {
			hasOmission = true
			break
		}
	}
	if !hasOmission {
		t.Fatalf("expected unpriced omission in res.Omissions, got: %v", res.Omissions)
	}

	// Finding 2 regression: cloud-boundary has eligible decision at end of month,
	// but NO cold transition or cold inventory was observed.
	// Cold byte-time must NOT be fabricated (no 50% split).
	glacierIR := policies[PolicyWarmGlacierIR]
	for _, line := range glacierIR.Report.Lines {
		if line.RateID == "s3.storage_glacier_ir" || line.RateID == "s3.transition_ir" {
			t.Fatalf("unexpected cold rate in glacierIR policy when no transition observed: %+v", line)
		}
	}

	// Repeat for determinism
	res2, err := ComparePolicies(context.Background(), trace, catalog)
	if err != nil {
		t.Fatalf("second run failed: %v", err)
	}
	if res.Policies[0].TotalCostUSD != res2.Policies[0].TotalCostUSD ||
		res.Policies[1].TotalCostUSD != res2.Policies[1].TotalCostUSD {
		t.Fatalf("ComparePolicies is non-deterministic")
	}
}

func TestComparePoliciesRejectsRegionMismatch(t *testing.T) {
	trace, catalog := fixture(t, "cloud-boundary")
	mismatchedCatalog := *catalog
	mismatchedCatalog.Region = "us-east-1"

	_, err := ComparePolicies(context.Background(), trace, &mismatchedCatalog)
	if err == nil {
		t.Fatal("expected error on region mismatch, got nil")
	}
}

func TestTraceJSONDecodingColdDeliveredBytesFormats(t *testing.T) {
	traceJSONScalar := `{
		"schema_version": 1,
		"name": "test-scalar-cold-delivered",
		"kind": "synthetic",
		"region": "ap-southeast-1",
		"profile": "cloud",
		"metadata_backend": "dynamodb",
		"topology": "Test topology",
		"start": "2026-01-01T00:00:00Z",
		"duration_ns": 1000,
		"bytes_per_gb": 1073741824,
		"storage_month_seconds": 2592000,
		"policy_revision": 1,
		"policy": {
			"tiering": {"enabled": true, "warm_storage_class": "STANDARD", "cold_storage_class": "GLACIER_IR", "cold_after_days": 30, "min_pack_live_ratio": 0.8, "auto_restore": true, "auto_restore_days": 1, "auto_restore_tier": "Standard"},
			"compression": {"enabled": true, "codec": "zstd", "level": 3, "min_saving_ratio": 0.1},
			"chunking": {"min_bytes": 262144, "avg_bytes": 1048576, "max_bytes": 4194304}
		},
		"omissions": ["test"],
		"events": [
			{
				"at_ns": 0,
				"cold_logical_delivered_bytes": 67108864,
				"traffic_bytes": {},
				"write_counters": {"chunk_occurrences": 0, "dup_occurrences": 0, "revived_distinct_chunks": 0, "revived_bytes_stored": 0},
				"usage": [],
				"billable_storage_bytes": {}
			}
		]
	}`

	trScalar, err := Read(strings.NewReader(traceJSONScalar))
	if err != nil {
		t.Fatalf("failed to read trace with scalar cold_logical_delivered_bytes: %v", err)
	}
	if trScalar.Events[0].ColdLogicalDeliveredBytes != 67108864 {
		t.Fatalf("expected 67108864 scalar cold delivered bytes, got %d", trScalar.Events[0].ColdLogicalDeliveredBytes)
	}

	traceJSONMap := `{
		"schema_version": 1,
		"name": "test-map-cold-delivered",
		"kind": "synthetic",
		"region": "ap-southeast-1",
		"profile": "cloud",
		"metadata_backend": "dynamodb",
		"topology": "Test topology",
		"start": "2026-01-01T00:00:00Z",
		"duration_ns": 1000,
		"bytes_per_gb": 1073741824,
		"storage_month_seconds": 2592000,
		"policy_revision": 1,
		"policy": {
			"tiering": {"enabled": true, "warm_storage_class": "STANDARD", "cold_storage_class": "GLACIER_IR", "cold_after_days": 30, "min_pack_live_ratio": 0.8, "auto_restore": true, "auto_restore_days": 1, "auto_restore_tier": "Standard"},
			"compression": {"enabled": true, "codec": "zstd", "level": 3, "min_saving_ratio": 0.1},
			"chunking": {"min_bytes": 262144, "avg_bytes": 1048576, "max_bytes": 4194304}
		},
		"omissions": ["test"],
		"events": [
			{
				"at_ns": 0,
				"cold_logical_delivered_bytes": {"GLACIER_IR": 67108864},
				"traffic_bytes": {},
				"write_counters": {"chunk_occurrences": 0, "dup_occurrences": 0, "revived_distinct_chunks": 0, "revived_bytes_stored": 0},
				"usage": [],
				"billable_storage_bytes": {}
			}
		]
	}`

	trMap, err := Read(strings.NewReader(traceJSONMap))
	if err != nil {
		t.Fatalf("failed to read trace with map cold_logical_delivered_bytes: %v", err)
	}
	bytesIR, ok := trMap.Events[0].ColdDeliveredForClass(storage.GlacierIR)
	if !ok || bytesIR != 67108864 {
		t.Fatalf("expected 67108864 for GLACIER_IR, got ok=%v bytes=%d", ok, bytesIR)
	}
}
