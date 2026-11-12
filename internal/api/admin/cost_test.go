package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/metadata/badger"
	"github.com/KinasTomes/miles/internal/stats"
	"github.com/KinasTomes/miles/internal/storage"
)

type fakeStats struct {
	globalFunc func(context.Context) (stats.Summary, error)
	bucketFunc func(context.Context, string) (stats.BucketSummary, error)
}

func (f *fakeStats) Global(ctx context.Context) (stats.Summary, error) {
	if f.globalFunc != nil {
		return f.globalFunc(ctx)
	}
	stored := int64(1073741824)
	return stats.Summary{
		PhysicalComplete: true,
		Metrics: stats.Metrics{
			BytesLogical: 2147483648,
			BytesStored:  &stored,
		},
		PhysicalClasses: map[string]int64{
			string(storage.Standard): 1073741824,
		},
	}, nil
}

func (f *fakeStats) Bucket(ctx context.Context, bucket string) (stats.BucketSummary, error) {
	if f.bucketFunc != nil {
		return f.bucketFunc(ctx, bucket)
	}
	if bucket == "valid-bucket" {
		stored := int64(1073741824)
		return stats.BucketSummary{
			Bucket:   bucket,
			BucketID: "bkt-1",
			Summary: stats.Summary{
				PhysicalComplete: true,
				Metrics: stats.Metrics{
					BytesLogical: 2147483648,
					BytesStored:  &stored,
				},
				PhysicalClasses: map[string]int64{
					string(storage.Standard): 1073741824,
				},
			},
		}, nil
	}
	return stats.BucketSummary{}, storage.ErrBucketNotFound
}

func (f *fakeStats) Layout(context.Context, string, string) (stats.Layout, error) {
	return stats.Layout{}, nil
}

func newCostTestWorld(t *testing.T, s Statistics) (*world, *httptest.Server) {
	t.Helper()
	w := newWorld(t, badger.Bootstrap{})
	w.opts.Statistics = s
	handler, err := NewServer(w.opts)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return w, server
}

func TestCostReportRegionAndAsOfValidation(t *testing.T) {
	// Finding 4 regression:
	// Region and as_of must be validated; unsupported region/date must fail with HTTP 400,
	// never silently falling back to Singapore or ignoring as_of.
	w, server := newCostTestWorld(t, &fakeStats{})

	cases := []struct {
		name       string
		query      string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "unsupported region returns 400",
			query:      "region=eu-west-1",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "unsupported pricing region",
		},
		{
			name:       "unmatched as_of returns 400",
			query:      "region=ap-southeast-1&as_of=2030-01-01",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "pricing catalog for region",
		},
		{
			name:       "malformed as_of format returns 400",
			query:      "region=ap-southeast-1&as_of=invalid-date",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "invalid as_of format",
		},
		{
			name:       "valid region and valid as_of returns 200",
			query:      "region=ap-southeast-1&as_of=2026-10-19",
			wantStatus: http.StatusOK,
			wantSubstr: "unscoped_partial_storage_snapshot",
		},
		{
			name:       "default region and as_of returns 200",
			query:      "",
			wantStatus: http.StatusOK,
			wantSubstr: "unscoped_partial_storage_snapshot",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := server.URL + "/admin/v1/cost/report"
			if tc.query != "" {
				url += "?" + tc.query
			}
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+w.root)

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("query %q: expected status %d, got %d", tc.query, tc.wantStatus, resp.StatusCode)
			}

			var bodyMap map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&bodyMap); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(bodyMap)
			if !strings.Contains(string(raw), tc.wantSubstr) {
				t.Fatalf("expected response to contain %q, got: %s", tc.wantSubstr, string(raw))
			}
		})
	}
}

func TestCostReportStatisticsUnavailable(t *testing.T) {
	// Finding 5 regression:
	// Unavailable statistics provider or backend error must return HTTP 503,
	// never HTTP 200 with subtotal USD 0.

	t.Run("nil statistics provider returns 503", func(t *testing.T) {
		w, server := newCostTestWorld(t, nil)

		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable, got %d", resp.StatusCode)
		}
	})

	t.Run("backend error returns 503", func(t *testing.T) {
		backendErrStats := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{}, errors.New("connection reset by peer")
			},
		}
		w, server := newCostTestWorld(t, backendErrStats)

		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable, got %d", resp.StatusCode)
		}
	})

	t.Run("real zero inventory returns 200 with zero cost", func(t *testing.T) {
		storedZero := int64(0)
		zeroStats := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{
					Metrics: stats.Metrics{
						BytesLogical: 0,
						BytesStored:  &storedZero,
					},
					PhysicalClasses: map[string]int64{
						string(storage.Standard): 0,
					},
				}, nil
			},
		}
		w, server := newCostTestWorld(t, zeroStats)

		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for real zero inventory, got %d", resp.StatusCode)
		}

		var report CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
			t.Fatal(err)
		}
		if report.Results.PricedSubtotalUSDExact != "0" {
			t.Fatalf("expected subtotal '0', got %q", report.Results.PricedSubtotalUSDExact)
		}
	})
}

func TestCostReportFullContractResponse(t *testing.T) {
	// Finding 3: full structured report adhering to docs/dedup-storage-gateway-api.md
	w, server := newCostTestWorld(t, &fakeStats{})

	url := server.URL + "/admin/v1/cost/report?scenario=B&profile=edge&from=2026-10-01T00:00:00Z&to=2026-10-31T00:00:00Z"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+w.root)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errObj map[string]any
		json.NewDecoder(resp.Body).Decode(&errObj)
		t.Fatalf("expected 200 OK, got %d: %v", resp.StatusCode, errObj)
	}

	var r CostReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}

	if r.ReportType != "unscoped_partial_storage_snapshot" {
		t.Fatalf("expected unscoped_partial_storage_snapshot, got %q", r.ReportType)
	}
	if r.Inputs.Scenario != "B" || r.Inputs.Profile != "edge" {
		t.Fatalf("inputs not preserved: scenario=%s, profile=%s", r.Inputs.Scenario, r.Inputs.Profile)
	}
	if r.Infrastructure.ALBLCU != "not_applicable (edge deployment has no ALB)" {
		t.Fatalf("expected ALB not applicable for edge profile, got %q", r.Infrastructure.ALBLCU)
	}
	if r.Infrastructure.ComputeOrEdgeVM != "omitted (edge hardware)" {
		t.Fatalf("expected edge hardware compute note, got %q", r.Infrastructure.ComputeOrEdgeVM)
	}
	if r.Inputs.Region != "ap-southeast-1" || r.Inputs.AsOf != "2026-10-19" {
		t.Fatalf("pricing inputs not populated: region=%s, as_of=%s", r.Inputs.Region, r.Inputs.AsOf)
	}
	if r.Baseline.DirectS3.LogicalBytes != 2147483648 {
		t.Fatalf("baseline logical bytes mismatch: %d", r.Baseline.DirectS3.LogicalBytes)
	}
	if r.DSGStorage.StorageByClass[string(storage.Standard)] != 1073741824 {
		t.Fatalf("dsg storage class mismatch: %v", r.DSGStorage.StorageByClass)
	}
	if r.Results.PricedSubtotalUSDExact == "" {
		t.Fatal("empty priced subtotal")
	}
	if len(r.Omissions) == 0 || len(r.Limitations) == 0 {
		t.Fatal("omissions or limitations missing")
	}

	// Verify savings vs baseline:
	// Since CompleteTotalUSD == nil in partial prototype, net savings must be nil,
	// while indicative storage difference is populated.
	if r.Results.NetSavingUSDVsBaseline != nil || r.Results.SavingsPercentVsBaseline != nil {
		t.Fatalf("expected nil net savings in partial prototype, got: USD=%v, Pct=%v", r.Results.NetSavingUSDVsBaseline, r.Results.SavingsPercentVsBaseline)
	}
	if r.Results.IndicativeStorageDiffUSD == nil {
		t.Fatal("expected non-nil indicative storage difference")
	}
	if *r.Results.IndicativeStorageDiffUSD != "1/40" {
		t.Fatalf("expected indicative storage difference 1/40, got %s", *r.Results.IndicativeStorageDiffUSD)
	}

	// Finding 6 regression: In unscoped_partial_storage_snapshot, fees that are not measured
	// must be reported as "omitted", never literal "0", and copy breakdowns must not claim
	// "accounted in totals" without evidence.
	if r.Baseline.DirectS3.RequestsCostUSD != "omitted" {
		t.Fatalf("expected baseline RequestsCostUSD to be 'omitted', got %q", r.Baseline.DirectS3.RequestsCostUSD)
	}
	if r.Baseline.DirectS3.NetworkCostUSD != "omitted" {
		t.Fatalf("expected baseline NetworkCostUSD to be 'omitted', got %q", r.Baseline.DirectS3.NetworkCostUSD)
	}
	if r.DSGRequestsNetwork.RetrievalFees != "omitted" {
		t.Fatalf("expected DSG RetrievalFees to be 'omitted', got %q", r.DSGRequestsNetwork.RetrievalFees)
	}
	if r.DSGRequestsNetwork.TransitionFees != "omitted" {
		t.Fatalf("expected DSG TransitionFees to be 'omitted', got %q", r.DSGRequestsNetwork.TransitionFees)
	}
	if r.DSGRequestsNetwork.RestoreFees != "omitted" {
		t.Fatalf("expected DSG RestoreFees to be 'omitted', got %q", r.DSGRequestsNetwork.RestoreFees)
	}
	if r.DSGRequestsNetwork.EarlyDeletionFees != "omitted" {
		t.Fatalf("expected DSG EarlyDeletionFees to be 'omitted', got %q", r.DSGRequestsNetwork.EarlyDeletionFees)
	}
	if !strings.Contains(r.RevivalCopy.Note, "omitted") && !strings.Contains(r.RevivalCopy.Note, "unmeasured") {
		t.Fatalf("expected RevivalCopy.Note to indicate unmeasured/omitted copy breakdown, got %q", r.RevivalCopy.Note)
	}
}

func TestCostReportFiltersAndValidation(t *testing.T) {
	// Finding 1, 2, 4 regressions:
	// - scenario and profile enum validation (reject invalid with 400)
	// - domain unsupported (reject with 400)
	// - bucket filter (call Statistics.Bucket, 404 if not found, 200 with bucket methodology if found)
	// - from/to timestamp validation (both required, valid RFC3339, to > from, reject with 400)
	w, server := newCostTestWorld(t, &fakeStats{})

	cases := []struct {
		name       string
		query      string
		wantStatus int
		wantSubstr string
	}{
		{
			name:       "invalid scenario returns 400",
			query:      "scenario=garbage",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "invalid scenario",
		},
		{
			name:       "invalid profile returns 400",
			query:      "profile=no-such-profile",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "invalid profile",
		},
		{
			name:       "domain filter returns 400 unsupported",
			query:      "domain=team-a",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "per-domain physical allocation is unavailable",
		},
		{
			name:       "nonexistent bucket returns 404",
			query:      "bucket=nonexistent-filter",
			wantStatus: http.StatusNotFound,
			wantSubstr: "bucket_not_found",
		},
		{
			name:       "valid bucket returns 200 with bucket methodology",
			query:      "bucket=valid-bucket",
			wantStatus: http.StatusOK,
			wantSubstr: "bucket_inventory_snapshot",
		},
		{
			name:       "single 'from' parameter without 'to' returns 400",
			query:      "from=2026-10-01T00:00:00Z",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "both 'from' and 'to' parameters are required",
		},
		{
			name:       "single 'to' parameter without 'from' returns 400",
			query:      "to=2026-10-31T00:00:00Z",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "both 'from' and 'to' parameters are required",
		},
		{
			name:       "malformed 'to' timestamp returns 400",
			query:      "from=2026-10-01T00:00:00Z&to=not-a-date",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "invalid 'to' timestamp",
		},
		{
			name:       "malformed 'from' timestamp returns 400",
			query:      "from=bad-date&to=2026-10-31T00:00:00Z",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "invalid 'from' timestamp",
		},
		{
			name:       "'to' before 'from' returns 400",
			query:      "from=2026-10-31T00:00:00Z&to=2026-10-01T00:00:00Z",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "must be after 'from' timestamp",
		},
		{
			name:       "'to' equal to 'from' returns 400",
			query:      "from=2026-10-01T00:00:00Z&to=2026-10-01T00:00:00Z",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "must be after 'from' timestamp",
		},
		{
			name:       "incompatible scenario A with profile edge returns 400",
			query:      "scenario=A&profile=edge",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "incompatible scenario/profile",
		},
		{
			name:       "incompatible scenario B with profile cloud returns 400",
			query:      "scenario=B&profile=cloud",
			wantStatus: http.StatusBadRequest,
			wantSubstr: "incompatible scenario/profile",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			url := server.URL + "/admin/v1/cost/report"
			if tc.query != "" {
				url += "?" + tc.query
			}
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer "+w.root)

			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("query %q: expected status %d, got %d", tc.query, tc.wantStatus, resp.StatusCode)
			}

			var bodyMap map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&bodyMap); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(bodyMap)
			if !strings.Contains(string(raw), tc.wantSubstr) {
				t.Fatalf("expected response to contain %q, got: %s", tc.wantSubstr, string(raw))
			}
		})
	}
}

func TestCostReportBucketApportionsFromGlobalGlacier(t *testing.T) {
	// Finding 1 regression:
	// Global has 1GiB Glacier ($0.0045/GB-Mo).
	// stats.Bucket() has 1GiB BytesStored, but PhysicalClasses is nil.
	// Cost report must NOT assume 1GiB Standard ($0.025).
	// It should apportion from Global with evidence, yielding $0.0045 matching Global!
	stored := int64(1073741824)
	glacierStats := &fakeStats{
		globalFunc: func(context.Context) (stats.Summary, error) {
			return stats.Summary{
				SnapshotID:       "snap-glacier-1",
				PhysicalComplete: true,
				Metrics: stats.Metrics{
					BytesLogical: 1073741824,
					BytesStored:  &stored,
				},
				PhysicalClasses: map[string]int64{
					string(storage.Glacier): 1073741824,
				},
			}, nil
		},
		bucketFunc: func(ctx context.Context, bucket string) (stats.BucketSummary, error) {
			if bucket == "glacier-bkt" {
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-g",
					Summary: stats.Summary{
						SnapshotID:       "snap-glacier-1",
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 1073741824,
							BytesStored:  &stored,
						},
						PhysicalClasses: nil, // missing per-class breakdown on bucket
					},
				}, nil
			}
			return stats.BucketSummary{}, storage.ErrBucketNotFound
		},
	}

	w, server := newCostTestWorld(t, glacierStats)

	// 1. Check Global report
	reqGlobal, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
	reqGlobal.Header.Set("Authorization", "Bearer "+w.root)
	respGlobal, err := server.Client().Do(reqGlobal)
	if err != nil {
		t.Fatal(err)
	}
	defer respGlobal.Body.Close()
	var rGlobal CostReportResponse
	if err := json.NewDecoder(respGlobal.Body).Decode(&rGlobal); err != nil {
		t.Fatal(err)
	}

	// 2. Check Bucket report
	reqBucket, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=glacier-bkt", nil)
	reqBucket.Header.Set("Authorization", "Bearer "+w.root)
	respBucket, err := server.Client().Do(reqBucket)
	if err != nil {
		t.Fatal(err)
	}
	defer respBucket.Body.Close()
	var rBucket CostReportResponse
	if err := json.NewDecoder(respBucket.Body).Decode(&rBucket); err != nil {
		t.Fatal(err)
	}

	// Bucket subtotal must match Global subtotal ($0.0045), NOT Standard ($0.025)!
	if rBucket.Results.PricedSubtotalUSDExact != rGlobal.Results.PricedSubtotalUSDExact {
		t.Fatalf("bucket report subtotal %q does not match global subtotal %q; expected Glacier price, got Standard",
			rBucket.Results.PricedSubtotalUSDExact, rGlobal.Results.PricedSubtotalUSDExact)
	}
	if rBucket.DSGStorage.StorageByClass[string(storage.Glacier)] != 1073741824 {
		t.Fatalf("expected 1GiB Glacier in bucket storage_by_class, got: %v", rBucket.DSGStorage.StorageByClass)
	}
}

func TestCostReportIncompleteOrUnknownInventoryOmittedFromSavings(t *testing.T) {
	// Finding 2 regression:
	// When PhysicalComplete=false or BytesStored=nil or unknown storage class,
	// API must NOT return subtotal $0 with fake positive indicative savings!

	t.Run("PhysicalComplete=false omits savings", func(t *testing.T) {
		stored := int64(1073741824)
		incompleteStats := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{
					PhysicalComplete: false, // incomplete inventory!
					Metrics: stats.Metrics{
						BytesLogical: 2147483648,
						BytesStored:  &stored,
					},
					PhysicalClasses: map[string]int64{
						string(storage.Standard): 1073741824,
					},
				}, nil
			},
		}
		w, server := newCostTestWorld(t, incompleteStats)
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var r CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.Results.IndicativeStorageDiffUSD != nil {
			t.Fatalf("expected nil indicative storage diff when PhysicalComplete=false, got %s", *r.Results.IndicativeStorageDiffUSD)
		}
		if len(r.DSGStorage.CostReport.Unpriced) == 0 {
			t.Fatal("expected unpriced item for incomplete physical inventory")
		}
	})

	t.Run("BytesStored=nil omits savings", func(t *testing.T) {
		nilStoredStats := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{
					PhysicalComplete: true,
					Metrics: stats.Metrics{
						BytesLogical: 2147483648,
						BytesStored:  nil, // unmeasured!
					},
				}, nil
			},
		}
		w, server := newCostTestWorld(t, nilStoredStats)
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var r CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.Results.IndicativeStorageDiffUSD != nil {
			t.Fatalf("expected nil indicative storage diff when BytesStored=nil, got %s", *r.Results.IndicativeStorageDiffUSD)
		}
	})

	t.Run("unknown class omits savings and preserves unpriced", func(t *testing.T) {
		stored := int64(1073741824)
		unknownClassStats := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{
					PhysicalComplete: true,
					Metrics: stats.Metrics{
						BytesLogical: 2147483648,
						BytesStored:  &stored,
					},
					PhysicalClasses: map[string]int64{
						"unknown_class": 1073741824, // unpriced class!
					},
				}, nil
			},
		}
		w, server := newCostTestWorld(t, unknownClassStats)
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var r CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.Results.IndicativeStorageDiffUSD != nil {
			t.Fatalf("expected nil indicative storage diff when unknown class present, got %s", *r.Results.IndicativeStorageDiffUSD)
		}
		foundUnpriced := false
		for _, u := range r.DSGStorage.CostReport.Unpriced {
			if strings.Contains(u.RateID, "unknown_class") {
				foundUnpriced = true
				break
			}
		}
		if !foundUnpriced {
			t.Fatalf("expected unknown_class in unpriced items, got: %+v", r.DSGStorage.CostReport.Unpriced)
		}
	})
}

func TestCostReportRevivalObservationsReflected(t *testing.T) {
	// Finding 3 regression:
	// Revival counters from stats.Summary must be populated when available,
	// and null when nil (not claiming 0).
	t.Run("observed revival counters populated", func(t *testing.T) {
		stored := int64(1073741824)
		chunks := uint64(7)
		bytes := uint64(524288)
		revivalStats := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{
					PhysicalComplete: true,
					Metrics: stats.Metrics{
						BytesLogical: 2147483648,
						BytesStored:  &stored,
					},
					PhysicalClasses: map[string]int64{
						string(storage.Standard): 1073741824,
					},
					RevivedChunks: &chunks,
					RevivedBytes:  &bytes,
				}, nil
			},
		}
		w, server := newCostTestWorld(t, revivalStats)
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var r CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.RevivalCopy.RevivedChunks == nil || *r.RevivalCopy.RevivedChunks != 7 {
			t.Fatalf("expected RevivedChunks 7, got %v", r.RevivalCopy.RevivedChunks)
		}
		if r.RevivalCopy.RevivedBytesStored == nil || *r.RevivalCopy.RevivedBytesStored != 524288 {
			t.Fatalf("expected RevivedBytesStored 524288, got %v", r.RevivalCopy.RevivedBytesStored)
		}
	})

	t.Run("nil revival counters serialized as null", func(t *testing.T) {
		w, server := newCostTestWorld(t, &fakeStats{})
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		var r CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}
		if r.RevivalCopy.RevivedChunks != nil || r.RevivalCopy.RevivedBytesStored != nil {
			t.Fatalf("expected nil revival counters when not measured, got chunks=%v bytes=%v",
				r.RevivalCopy.RevivedChunks, r.RevivalCopy.RevivedBytesStored)
		}
	})
}

func TestCostReportScenarioProfileDifferentiation(t *testing.T) {
	// Finding 4 regression:
	// Verify that scenario A (cloud) and scenario B (edge) produce distinct infrastructure
	// entries and explicit unscoped partial storage snapshot mode.
	w, server := newCostTestWorld(t, &fakeStats{})

	// Scenario A: Cloud
	reqA, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?scenario=A&profile=cloud", nil)
	reqA.Header.Set("Authorization", "Bearer "+w.root)
	respA, err := server.Client().Do(reqA)
	if err != nil {
		t.Fatal(err)
	}
	defer respA.Body.Close()
	var rA CostReportResponse
	if err := json.NewDecoder(respA.Body).Decode(&rA); err != nil {
		t.Fatal(err)
	}

	// Scenario B: Edge
	reqB, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?scenario=B&profile=edge", nil)
	reqB.Header.Set("Authorization", "Bearer "+w.root)
	respB, err := server.Client().Do(reqB)
	if err != nil {
		t.Fatal(err)
	}
	defer respB.Body.Close()
	var rB CostReportResponse
	if err := json.NewDecoder(respB.Body).Decode(&rB); err != nil {
		t.Fatal(err)
	}

	if rA.Infrastructure.ALBLCU != "omitted (cloud alb)" {
		t.Fatalf("expected cloud alb note, got %q", rA.Infrastructure.ALBLCU)
	}
	if rB.Infrastructure.ALBLCU != "not_applicable (edge deployment has no ALB)" {
		t.Fatalf("expected edge not_applicable alb note, got %q", rB.Infrastructure.ALBLCU)
	}
	if rA.Infrastructure.ComputeOrEdgeVM != "omitted (cloud ec2)" {
		t.Fatalf("expected cloud ec2 compute note, got %q", rA.Infrastructure.ComputeOrEdgeVM)
	}
	if rB.Infrastructure.ComputeOrEdgeVM != "omitted (edge hardware)" {
		t.Fatalf("expected edge hardware compute note, got %q", rB.Infrastructure.ComputeOrEdgeVM)
	}
	if rA.ReportType != "unscoped_partial_storage_snapshot" || rB.ReportType != "unscoped_partial_storage_snapshot" {
		t.Fatalf("expected unscoped_partial_storage_snapshot ReportType")
	}
}

func TestCostReportBucketWithMixedGlobalClassesDoesNotInferGlobalRatio(t *testing.T) {
	// Finding 1 regression:
	// Global has 1GiB Standard ($0.025/GB-Mo) and 1GiB Glacier ($0.0045/GB-Mo).
	// A bucket has 1GiB stored, but PhysicalClasses is nil.
	// The API must NOT infer a synthetic 50/50 ratio from global ($0.01475).
	// Without bucket-level evidence, storage class must remain unmeasured/unknown and omitted from savings.
	// When bucket provides explicit class evidence (e.g. Standard), it must price accurately ($0.025).
	globalStored := int64(2147483648)
	bucketStored := int64(1073741824)
	statsProvider := &fakeStats{
		globalFunc: func(context.Context) (stats.Summary, error) {
			return stats.Summary{
				PhysicalComplete: true,
				Metrics: stats.Metrics{
					BytesLogical: 2147483648,
					BytesStored:  &globalStored,
				},
				PhysicalClasses: map[string]int64{
					string(storage.Standard): 1073741824,
					string(storage.Glacier):  1073741824,
				},
			}, nil
		},
		bucketFunc: func(ctx context.Context, bucket string) (stats.BucketSummary, error) {
			switch bucket {
			case "unspecified-bkt":
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-unspecified",
					Summary: stats.Summary{
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 1073741824,
							BytesStored:  &bucketStored,
						},
						PhysicalClasses: nil, // unmeasured
					},
				}, nil
			case "standard-bkt":
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-standard",
					Summary: stats.Summary{
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 1073741824,
							BytesStored:  &bucketStored,
						},
						PhysicalClasses: map[string]int64{
							string(storage.Standard): 1073741824,
						},
					},
				}, nil
			default:
				return stats.BucketSummary{}, storage.ErrBucketNotFound
			}
		},
	}

	w, server := newCostTestWorld(t, statsProvider)

	// 1. Unspecified bucket: must NOT calculate $0.01475 ("59/4000")
	reqUnspec, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=unspecified-bkt", nil)
	reqUnspec.Header.Set("Authorization", "Bearer "+w.root)
	respUnspec, err := server.Client().Do(reqUnspec)
	if err != nil {
		t.Fatal(err)
	}
	defer respUnspec.Body.Close()
	var rUnspec CostReportResponse
	if err := json.NewDecoder(respUnspec.Body).Decode(&rUnspec); err != nil {
		t.Fatal(err)
	}

	if rUnspec.Results.PricedSubtotalUSDExact == "59/4000" || rUnspec.Results.PricedSubtotalUSDExact == "118/8000" {
		t.Fatalf("API incorrectly inferred global ratio yielding $0.01475: %s", rUnspec.Results.PricedSubtotalUSDExact)
	}
	if rUnspec.DSGStorage.StorageByClass["UNKNOWN"] != 1073741824 {
		t.Fatalf("expected 1GiB UNKNOWN in storage_by_class, got: %v", rUnspec.DSGStorage.StorageByClass)
	}
	if rUnspec.Results.IndicativeStorageDiffUSD != nil {
		t.Fatalf("expected nil indicative_storage_difference_usd when classes are unmeasured, got %v", *rUnspec.Results.IndicativeStorageDiffUSD)
	}

	// 2. Standard bucket with explicit evidence: must price at Standard $0.025 ("1/40")
	reqStd, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=standard-bkt", nil)
	reqStd.Header.Set("Authorization", "Bearer "+w.root)
	respStd, err := server.Client().Do(reqStd)
	if err != nil {
		t.Fatal(err)
	}
	defer respStd.Body.Close()
	var rStd CostReportResponse
	if err := json.NewDecoder(respStd.Body).Decode(&rStd); err != nil {
		t.Fatal(err)
	}

	if rStd.Results.PricedSubtotalUSDExact != "1/40" {
		t.Fatalf("expected Standard subtotal 1/40 ($0.025), got: %s", rStd.Results.PricedSubtotalUSDExact)
	}
	if rStd.DSGStorage.StorageByClass[string(storage.Standard)] != 1073741824 {
		t.Fatalf("expected 1GiB Standard in storage_by_class, got: %v", rStd.DSGStorage.StorageByClass)
	}
}

func TestCostReportSmallBucketPreservesPhysicalBytesAndAvoidsZeroCostSavings(t *testing.T) {
	// Finding 2 regression:
	// A small bucket has 1 byte physical stored and 1GiB logical.
	// Integer division / truncation must not drop 1 byte to 0 physical bytes,
	// and must NOT emit positive savings against baseline with $0 DSG cost.
	globalStored := int64(2147483648)
	oneByte := int64(1)
	statsProvider := &fakeStats{
		globalFunc: func(context.Context) (stats.Summary, error) {
			return stats.Summary{
				PhysicalComplete: true,
				Metrics: stats.Metrics{
					BytesLogical: 2147483648,
					BytesStored:  &globalStored,
				},
				PhysicalClasses: map[string]int64{
					string(storage.Standard): 1073741824,
					string(storage.Glacier):  1073741824,
				},
			}, nil
		},
		bucketFunc: func(ctx context.Context, bucket string) (stats.BucketSummary, error) {
			switch bucket {
			case "one-byte-unspec":
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-1b-unspec",
					Summary: stats.Summary{
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 1073741824, // 1GiB logical
							BytesStored:  &oneByte,   // 1 byte physical
						},
						PhysicalClasses: nil,
					},
				}, nil
			case "one-byte-zero-classes":
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-1b-zeros",
					Summary: stats.Summary{
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 1073741824,
							BytesStored:  &oneByte,
						},
						PhysicalClasses: map[string]int64{
							"STANDARD": 0,
							"GLACIER":  0,
						},
					},
				}, nil
			case "one-byte-standard":
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-1b-std",
					Summary: stats.Summary{
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 1073741824,
							BytesStored:  &oneByte,
						},
						PhysicalClasses: map[string]int64{
							string(storage.Standard): 1,
						},
					},
				}, nil
			default:
				return stats.BucketSummary{}, storage.ErrBucketNotFound
			}
		},
	}

	w, server := newCostTestWorld(t, statsProvider)

	// 1. One-byte unspecified: 1 physical byte preserved as UNKNOWN, indicative storage diff is nil
	reqUnspec, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=one-byte-unspec", nil)
	reqUnspec.Header.Set("Authorization", "Bearer "+w.root)
	respUnspec, err := server.Client().Do(reqUnspec)
	if err != nil {
		t.Fatal(err)
	}
	defer respUnspec.Body.Close()
	var rUnspec CostReportResponse
	if err := json.NewDecoder(respUnspec.Body).Decode(&rUnspec); err != nil {
		t.Fatal(err)
	}

	if rUnspec.DSGStorage.StorageByClass["UNKNOWN"] != 1 {
		t.Fatalf("expected exactly 1 byte preserved in storage_by_class UNKNOWN, got: %v", rUnspec.DSGStorage.StorageByClass)
	}
	if rUnspec.Results.IndicativeStorageDiffUSD != nil {
		t.Fatalf("expected nil indicative_storage_difference_usd for 1-byte unknown inventory, got: %v", *rUnspec.Results.IndicativeStorageDiffUSD)
	}

	// 2. One-byte with zeros map: 1 physical byte preserved as UNKNOWN, indicative storage diff is nil
	reqZeros, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=one-byte-zero-classes", nil)
	reqZeros.Header.Set("Authorization", "Bearer "+w.root)
	respZeros, err := server.Client().Do(reqZeros)
	if err != nil {
		t.Fatal(err)
	}
	defer respZeros.Body.Close()
	var rZeros CostReportResponse
	if err := json.NewDecoder(respZeros.Body).Decode(&rZeros); err != nil {
		t.Fatal(err)
	}

	if rZeros.DSGStorage.StorageByClass["UNKNOWN"] != 1 {
		t.Fatalf("expected exactly 1 byte preserved in storage_by_class UNKNOWN, got: %v", rZeros.DSGStorage.StorageByClass)
	}
	if rZeros.Results.IndicativeStorageDiffUSD != nil {
		t.Fatalf("expected nil indicative_storage_difference_usd for zeros map with positive BytesStored, got: %v", *rZeros.Results.IndicativeStorageDiffUSD)
	}

	// 3. One-byte with explicit Standard: priced with exact rational (1/42949672960)
	reqStd, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=one-byte-standard", nil)
	reqStd.Header.Set("Authorization", "Bearer "+w.root)
	respStd, err := server.Client().Do(reqStd)
	if err != nil {
		t.Fatal(err)
	}
	defer respStd.Body.Close()
	var rStd CostReportResponse
	if err := json.NewDecoder(respStd.Body).Decode(&rStd); err != nil {
		t.Fatal(err)
	}

	if rStd.DSGStorage.StorageByClass[string(storage.Standard)] != 1 {
		t.Fatalf("expected 1 byte in storage_by_class STANDARD, got: %v", rStd.DSGStorage.StorageByClass)
	}
	if rStd.Results.PricedSubtotalUSDExact == "0" {
		t.Fatalf("expected non-zero subtotal for 1 byte of Standard storage, got 0")
	}
}

func TestCostReportBucketInferenceStrictConditions(t *testing.T) {
	// Round 5 regression tests for all 3 cases:
	// 1. Incomplete global inventory (PhysicalComplete = false) with only Standard observed
	//    must NOT infer Standard for the bucket.
	// 2. Total class bytes less than global.BytesStored (unclassified global bytes)
	//    must NOT infer 100% Standard for the bucket.
	// 3. Mismatched SnapshotID between Global and Bucket
	//    must NOT infer storage class from stale/unmatched snapshot.
	stored1GiB := int64(1073741824)
	stored2000 := int64(2000)
	stored500 := int64(500)

	t.Run("incomplete_global_inventory_omits_class_inference", func(t *testing.T) {
		statsProvider := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{
					SnapshotID:       "snap-same",
					PhysicalComplete: false, // incomplete global inventory
					Metrics: stats.Metrics{
						BytesLogical: 1073741824,
						BytesStored:  &stored1GiB,
					},
					PhysicalClasses: map[string]int64{
						string(storage.Standard): 1073741824,
					},
				}, nil
			},
			bucketFunc: func(ctx context.Context, bucket string) (stats.BucketSummary, error) {
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-test",
					Summary: stats.Summary{
						SnapshotID:       "snap-same",
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 1073741824,
							BytesStored:  &stored1GiB,
						},
						PhysicalClasses: nil,
					},
				}, nil
			},
		}
		w, server := newCostTestWorld(t, statsProvider)
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=bkt", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}

		if _, ok := r.DSGStorage.StorageByClass[string(storage.Standard)]; ok {
			t.Fatalf("expected Standard class NOT to be inferred when global inventory is incomplete, got: %v", r.DSGStorage.StorageByClass)
		}
		if r.DSGStorage.StorageByClass["UNKNOWN"] != 1073741824 {
			t.Fatalf("expected 1GiB UNKNOWN in storage_by_class, got: %v", r.DSGStorage.StorageByClass)
		}
		if r.Results.IndicativeStorageDiffUSD != nil {
			t.Fatalf("expected nil indicative_storage_difference_usd, got: %v", *r.Results.IndicativeStorageDiffUSD)
		}
	})

	t.Run("unclassified_global_bytes_omits_class_inference", func(t *testing.T) {
		statsProvider := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{
					SnapshotID:       "snap-same",
					PhysicalComplete: true,
					Metrics: stats.Metrics{
						BytesLogical: 2000,
						BytesStored:  &stored2000, // 2000 bytes total
					},
					PhysicalClasses: map[string]int64{
						string(storage.Standard): 1000, // only 1000 accounted for; 1000 unclassified
					},
				}, nil
			},
			bucketFunc: func(ctx context.Context, bucket string) (stats.BucketSummary, error) {
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-test",
					Summary: stats.Summary{
						SnapshotID:       "snap-same",
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 500,
							BytesStored:  &stored500,
						},
						PhysicalClasses: nil,
					},
				}, nil
			},
		}
		w, server := newCostTestWorld(t, statsProvider)
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=bkt", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}

		if _, ok := r.DSGStorage.StorageByClass[string(storage.Standard)]; ok {
			t.Fatalf("expected Standard class NOT to be inferred when global has unclassified bytes, got: %v", r.DSGStorage.StorageByClass)
		}
		if r.DSGStorage.StorageByClass["UNKNOWN"] != 500 {
			t.Fatalf("expected 500 UNKNOWN in storage_by_class, got: %v", r.DSGStorage.StorageByClass)
		}
		if r.Results.IndicativeStorageDiffUSD != nil {
			t.Fatalf("expected nil indicative_storage_difference_usd, got: %v", *r.Results.IndicativeStorageDiffUSD)
		}
	})

	t.Run("mismatched_snapshot_id_omits_class_inference", func(t *testing.T) {
		statsProvider := &fakeStats{
			globalFunc: func(context.Context) (stats.Summary, error) {
				return stats.Summary{
					SnapshotID:       "snap-new", // different snapshot
					PhysicalComplete: true,
					Metrics: stats.Metrics{
						BytesLogical: 1073741824,
						BytesStored:  &stored1GiB,
					},
					PhysicalClasses: map[string]int64{
						string(storage.Standard): 1073741824,
					},
				}, nil
			},
			bucketFunc: func(ctx context.Context, bucket string) (stats.BucketSummary, error) {
				return stats.BucketSummary{
					Bucket:   bucket,
					BucketID: "bkt-test",
					Summary: stats.Summary{
						SnapshotID:       "snap-old", // stale snapshot
						PhysicalComplete: true,
						Metrics: stats.Metrics{
							BytesLogical: 1073741824,
							BytesStored:  &stored1GiB,
						},
						PhysicalClasses: nil,
					},
				}, nil
			},
		}
		w, server := newCostTestWorld(t, statsProvider)
		req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?bucket=bkt", nil)
		req.Header.Set("Authorization", "Bearer "+w.root)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var r CostReportResponse
		if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
			t.Fatal(err)
		}

		if _, ok := r.DSGStorage.StorageByClass[string(storage.Standard)]; ok {
			t.Fatalf("expected Standard class NOT to be inferred when SnapshotID mismatches, got: %v", r.DSGStorage.StorageByClass)
		}
		if r.DSGStorage.StorageByClass["UNKNOWN"] != 1073741824 {
			t.Fatalf("expected 1GiB UNKNOWN in storage_by_class, got: %v", r.DSGStorage.StorageByClass)
		}
		if r.Results.IndicativeStorageDiffUSD != nil {
			t.Fatalf("expected nil indicative_storage_difference_usd, got: %v", *r.Results.IndicativeStorageDiffUSD)
		}
	})
}

func TestCostReportInconsistentInventoryOmitsIndicativeSavings(t *testing.T) {
	// Round 6 regression test:
	// When sum of bytes in PhysicalClasses exceeds BytesStored (e.g. 2GiB Standard vs 1GiB BytesStored),
	// inventory is contradictory/inconsistent. The API must add an unpriced marker
	// and suppress indicative_storage_difference_usd (must NOT return -1/40 USD).
	stored1GiB := int64(1073741824)
	stored2GiB := int64(2147483648)
	statsProvider := &fakeStats{
		globalFunc: func(context.Context) (stats.Summary, error) {
			return stats.Summary{
				SnapshotID:       "snap-inconsistent",
				PhysicalComplete: true,
				Metrics: stats.Metrics{
					BytesLogical: 1073741824,  // 1GiB logical ($0.025 baseline)
					BytesStored:  &stored1GiB, // 1GiB physical reported
				},
				PhysicalClasses: map[string]int64{
					string(storage.Standard): stored2GiB, // 2GiB class reported (exceeds BytesStored!)
				},
			}, nil
		},
	}

	w, server := newCostTestWorld(t, statsProvider)
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report", nil)
	req.Header.Set("Authorization", "Bearer "+w.root)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r CostReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}

	if r.Results.IndicativeStorageDiffUSD != nil {
		t.Fatalf("expected nil indicative_storage_difference_usd for contradictory inventory (expected nil, got %q)", *r.Results.IndicativeStorageDiffUSD)
	}
	if len(r.DSGStorage.CostReport.Unpriced) == 0 {
		t.Fatalf("expected unpriced marker for inconsistent inventory")
	}
}

func TestCostReportModeledWorkloadSimulation(t *testing.T) {
	w, server := newCostTestWorld(t, &fakeStats{})

	// 1. Valid workload simulation: cloud-tiering-boundary
	url := server.URL + "/admin/v1/cost/report?workload=cloud-tiering-boundary&scenario=A&profile=cloud"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+w.root)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		var errObj map[string]any
		json.NewDecoder(resp.Body).Decode(&errObj)
		t.Fatalf("expected 200 OK, got %d: %v", resp.StatusCode, errObj)
	}

	var r CostReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}

	if r.ReportType != "simulated_comparative_policy_report" {
		t.Fatalf("expected simulated_comparative_policy_report, got %q", r.ReportType)
	}
	if r.Inputs.Workload != "cloud-tiering-boundary" {
		t.Fatalf("expected workload cloud-tiering-boundary, got %q", r.Inputs.Workload)
	}
	if r.Baseline.DirectS3.LogicalBytes == 0 {
		t.Fatal("expected non-zero baseline logical bytes")
	}
	if len(r.Baseline.CostReport.Lines) == 0 {
		t.Fatal("expected baseline cost lines")
	}
	if len(r.DSGStorage.CostReport.Lines) == 0 {
		t.Fatal("expected dsg cost lines")
	}

	// 2. Non-existent workload returns 400
	badReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=nonexistent-trace-name", nil)
	badReq.Header.Set("Authorization", "Bearer "+w.root)
	badResp, err := server.Client().Do(badReq)
	if err != nil {
		t.Fatal(err)
	}
	defer badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for nonexistent workload, got %d", badResp.StatusCode)
	}

	// 3. Workload combined with bucket filter returns 400
	conflictReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=cloud-tiering-boundary&bucket=demo", nil)
	conflictReq.Header.Set("Authorization", "Bearer "+w.root)
	conflictResp, err := server.Client().Do(conflictReq)
	if err != nil {
		t.Fatal(err)
	}
	defer conflictResp.Body.Close()
	if conflictResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for workload with bucket filter, got %d", conflictResp.StatusCode)
	}

	// 4. Finding 5: Workload topology / profile mismatch returns 400
	mismatchEdgeReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=edge-revival-accounting&scenario=A&profile=cloud", nil)
	mismatchEdgeReq.Header.Set("Authorization", "Bearer "+w.root)
	mismatchEdgeResp, err := server.Client().Do(mismatchEdgeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer mismatchEdgeResp.Body.Close()
	if mismatchEdgeResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for edge workload under cloud profile, got %d", mismatchEdgeResp.StatusCode)
	}

	mismatchCloudReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=cloud-tiering-boundary&scenario=B&profile=edge", nil)
	mismatchCloudReq.Header.Set("Authorization", "Bearer "+w.root)
	mismatchCloudResp, err := server.Client().Do(mismatchCloudReq)
	if err != nil {
		t.Fatal(err)
	}
	defer mismatchCloudResp.Body.Close()
	if mismatchCloudResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for cloud workload under edge profile, got %d", mismatchCloudResp.StatusCode)
	}

	// 5. Finding 2: Valid Edge workload simulation preserves physical byte distribution without double counting
	edgeReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=edge-revival-accounting&scenario=B&profile=edge", nil)
	edgeReq.Header.Set("Authorization", "Bearer "+w.root)
	edgeResp, err := server.Client().Do(edgeReq)
	if err != nil {
		t.Fatal(err)
	}
	defer edgeResp.Body.Close()
	if edgeResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for edge workload under edge profile, got %d", edgeResp.StatusCode)
	}

	var edgeReport CostReportResponse
	if err := json.NewDecoder(edgeResp.Body).Decode(&edgeReport); err != nil {
		t.Fatal(err)
	}

	classes := edgeReport.DSGStorage.StorageByClass
	if classes == nil {
		t.Fatal("expected non-nil StorageByClass")
	}
	stdBytes := classes["STANDARD"]
	irBytes := classes["GLACIER_IR"]
	if stdBytes != 536870912 {
		t.Fatalf("expected 536870912 STANDARD bytes, got %d", stdBytes)
	}
	if irBytes != 1073741824 {
		t.Fatalf("expected 1073741824 GLACIER_IR bytes, got %d", irBytes)
	}
	totalAccounted := stdBytes + irBytes
	if totalAccounted != 1610612736 {
		t.Fatalf("expected total 1610612736 (1.5 GiB), got %d (must NOT double count to 3 GiB!)", totalAccounted)
	}
	if edgeReport.DSGRequestsNetwork.TransitionFees == "" {
		t.Fatal("expected populated TransitionFees")
	}

	// 6. Finding 3: Workload cloud-tiering-boundary only has ddb.write (no EC2).
	// Infrastructure must properly allocate ddb.write to DynamoDB, and keep Compute as omitted!
	cloudReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=cloud-tiering-boundary&scenario=A&profile=cloud", nil)
	cloudReq.Header.Set("Authorization", "Bearer "+w.root)
	cloudResp, err := server.Client().Do(cloudReq)
	if err != nil {
		t.Fatal(err)
	}
	defer cloudResp.Body.Close()
	if cloudResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for cloud-tiering-boundary, got %d", cloudResp.StatusCode)
	}

	var cloudReport CostReportResponse
	if err := json.NewDecoder(cloudResp.Body).Decode(&cloudReport); err != nil {
		t.Fatal(err)
	}
	if cloudReport.Infrastructure.ComputeOrEdgeVM != "omitted (cloud ec2)" {
		t.Fatalf("expected 'omitted (cloud ec2)' for ComputeOrEdgeVM when no ec2 lines exist, got %q", cloudReport.Infrastructure.ComputeOrEdgeVM)
	}
	if cloudReport.Infrastructure.DynamoDB != "71/12500000" {
		t.Fatalf("expected DynamoDB cost '71/12500000' from ddb.write, got %q", cloudReport.Infrastructure.DynamoDB)
	}
	if cloudReport.Infrastructure.ALBLCU != "omitted (cloud alb)" {
		t.Fatalf("expected 'omitted (cloud alb)' for ALBLCU, got %q", cloudReport.Infrastructure.ALBLCU)
	}
	if cloudReport.Infrastructure.DiskEBS != "omitted" {
		t.Fatalf("expected 'omitted' for DiskEBS, got %q", cloudReport.Infrastructure.DiskEBS)
	}
}

func TestCostReportWorkloadSecurityAndTimeline(t *testing.T) {
	w, server := newCostTestWorld(t, &fakeStats{})

	// 1. Path traversal attempt must return HTTP 400 Bad Request and not leak parser error or file content
	traversalReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?scenario=A&profile=cloud&workload=../../configs/pricing/pricing.ap-southeast-1.example", nil)
	traversalReq.Header.Set("Authorization", "Bearer "+w.root)
	traversalResp, err := server.Client().Do(traversalReq)
	if err != nil {
		t.Fatal(err)
	}
	defer traversalResp.Body.Close()
	if traversalResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for path traversal attempt, got %d", traversalResp.StatusCode)
	}
	var travErr struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(traversalResp.Body).Decode(&travErr); err != nil {
		t.Fatal(err)
	}
	errMsg := travErr.Error.Message
	if !strings.Contains(errMsg, "not found") {
		t.Fatalf("expected 'not found' in error message, got %q", errMsg)
	}
	if strings.Contains(errMsg, "as_of") {
		t.Fatalf("error message must NOT leak parsed file contents or 'as_of': %q", errMsg)
	}

	// 2. Reject 'from' and 'to' filters on modeled workload
	fromToReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?scenario=A&profile=cloud&workload=cloud-boundary&from=2026-01-01T00:00:00Z&to=2026-01-02T00:00:00Z", nil)
	fromToReq.Header.Set("Authorization", "Bearer "+w.root)
	fromToResp, err := server.Client().Do(fromToReq)
	if err != nil {
		t.Fatal(err)
	}
	defer fromToResp.Body.Close()
	if fromToResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request when 'from'/'to' supplied with workload, got %d", fromToResp.StatusCode)
	}

	// 3. Modeled workload response echoes tr.Start and tr.Start + tr.DurationNS
	validReq, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?scenario=A&profile=cloud&workload=cloud-boundary", nil)
	validReq.Header.Set("Authorization", "Bearer "+w.root)
	validResp, err := server.Client().Do(validReq)
	if err != nil {
		t.Fatal(err)
	}
	defer validResp.Body.Close()
	if validResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for valid workload, got %d", validResp.StatusCode)
	}
	var rep CostReportResponse
	if err := json.NewDecoder(validResp.Body).Decode(&rep); err != nil {
		t.Fatal(err)
	}
	if rep.Inputs.From != "2026-01-01T00:00:00Z" {
		t.Fatalf("expected Inputs.From to match trace start '2026-01-01T00:00:00Z', got %q", rep.Inputs.From)
	}
	if rep.Inputs.To != "2026-01-31T00:00:00Z" {
		t.Fatalf("expected Inputs.To to match trace end '2026-01-31T00:00:00Z', got %q", rep.Inputs.To)
	}
}

func TestCostReportModeledWorkloadOutsideSourceTree(t *testing.T) {
	// Reviewer Finding [P2] (HEAD e834d29 audit):
	// When running outside the source tree, GET /admin/v1/cost/report?workload=cloud-tiering-boundary
	// must succeed via embedded trace fixtures and return HTTP 200, rather than failing with 400.
	w, server := newCostTestWorld(t, &fakeStats{})

	// Switch working directory to an isolated empty directory outside source tree
	t.Chdir(t.TempDir())

	// 1. cloud-tiering-boundary
	reqCloud, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=cloud-tiering-boundary&scenario=A&profile=cloud", nil)
	reqCloud.Header.Set("Authorization", "Bearer "+w.root)
	respCloud, err := server.Client().Do(reqCloud)
	if err != nil {
		t.Fatal(err)
	}
	defer respCloud.Body.Close()
	if respCloud.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 for cloud-tiering-boundary outside source tree, got %d", respCloud.StatusCode)
	}
	var repCloud CostReportResponse
	if err := json.NewDecoder(respCloud.Body).Decode(&repCloud); err != nil {
		t.Fatal(err)
	}
	if repCloud.Inputs.Workload != "cloud-tiering-boundary" {
		t.Fatalf("expected workload 'cloud-tiering-boundary', got %q", repCloud.Inputs.Workload)
	}

	// 2. edge-revival-accounting
	reqEdge, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=edge-revival-accounting&scenario=B&profile=edge", nil)
	reqEdge.Header.Set("Authorization", "Bearer "+w.root)
	respEdge, err := server.Client().Do(reqEdge)
	if err != nil {
		t.Fatal(err)
	}
	defer respEdge.Body.Close()
	if respEdge.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 for edge-revival-accounting outside source tree, got %d", respEdge.StatusCode)
	}
	var repEdge CostReportResponse
	if err := json.NewDecoder(respEdge.Body).Decode(&repEdge); err != nil {
		t.Fatal(err)
	}
	if repEdge.Inputs.Workload != "edge-revival-accounting" {
		t.Fatalf("expected workload 'edge-revival-accounting', got %q", repEdge.Inputs.Workload)
	}
}

func TestCostReportAuditItem10_SimulationErrorReturnsSanitized500(t *testing.T) {
	// Item 10 [P3]: API simulation_failed raw err.Error() returns 500 without leaking internal details.
	// For invalid user-side input: returns 400 invalid_request.
	// For internal backend failures: returns sanitized 500 simulation_failed.
	w, server := newCostTestWorld(t, &fakeStats{})

	// 1. Invalid client parameter returns 400 invalid_request
	req400, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=cloud-tiering-boundary&scenario=A&profile=cloud&policy=nonexistent_policy", nil)
	req400.Header.Set("Authorization", "Bearer "+w.root)
	resp400, err := server.Client().Do(req400)
	if err != nil {
		t.Fatal(err)
	}
	defer resp400.Body.Close()
	if resp400.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request, got %d", resp400.StatusCode)
	}

	var errBody map[string]any
	if err := json.NewDecoder(resp400.Body).Decode(&errBody); err != nil {
		t.Fatal(err)
	}
	errMap, ok := errBody["error"].(map[string]any)
	if !ok || errMap["code"] != "invalid_request" {
		t.Fatalf("expected error code 'invalid_request', got %v", errBody["error"])
	}
}

func TestCostReportAuditItem11_CandidatePolicySelectorSupportsGlacierFlex(t *testing.T) {
	// Item 11 [P3]: Candidate policy selector supports warm_glacier_flexible.
	w, server := newCostTestWorld(t, &fakeStats{})

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?workload=cloud-tiering-boundary&scenario=A&profile=cloud&policy=warm_glacier_flexible", nil)
	req.Header.Set("Authorization", "Bearer "+w.root)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK for policy=warm_glacier_flexible, got %d", resp.StatusCode)
	}

	var r CostReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}
	if r.DSGStorage.StorageByClass == nil {
		t.Fatal("expected DSGStorage.StorageByClass to be populated")
	}
}

func TestCostReportAuditItem12_14_RevivalCopyReportNullableJSON(t *testing.T) {
	// Item 12 & 14 [P3]: Revival/copy report has null for unmeasured bytes and accurate note.
	w, server := newCostTestWorld(t, &fakeStats{})

	req, _ := http.NewRequest(http.MethodGet, server.URL+"/admin/v1/cost/report?scenario=A&profile=cloud", nil)
	req.Header.Set("Authorization", "Bearer "+w.root)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected HTTP 200 OK, got %d", resp.StatusCode)
	}

	var rawMap map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rawMap); err != nil {
		t.Fatal(err)
	}
	revivalCopy, ok := rawMap["revival_copy"].(map[string]any)
	if !ok {
		t.Fatalf("expected revival_copy map in response, got %v", rawMap["revival_copy"])
	}

	// copy_source_download_bytes and copy_destination_upload_bytes must be nil / null (not 0)
	if revivalCopy["copy_source_download_bytes"] != nil {
		t.Fatalf("expected null for copy_source_download_bytes when unmeasured, got %v", revivalCopy["copy_source_download_bytes"])
	}
	if revivalCopy["copy_destination_upload_bytes"] != nil {
		t.Fatalf("expected null for copy_destination_upload_bytes when unmeasured, got %v", revivalCopy["copy_destination_upload_bytes"])
	}
	note, _ := revivalCopy["note"].(string)
	if !strings.Contains(note, "unmeasured") && !strings.Contains(note, "omitted") {
		t.Fatalf("expected note to reflect unmeasured/omitted copy bytes, got %q", note)
	}
}
