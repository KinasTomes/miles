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
			wantSubstr: "partial_prototype_cost_report",
		},
		{
			name:       "default region and as_of returns 200",
			query:      "",
			wantStatus: http.StatusOK,
			wantSubstr: "partial_prototype_cost_report",
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
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}

	var r CostReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		t.Fatal(err)
	}

	if r.ReportType != "partial_prototype_cost_report" {
		t.Fatalf("expected partial_prototype_cost_report, got %q", r.ReportType)
	}
	if r.Inputs.Scenario != "B" || r.Inputs.Profile != "edge" {
		t.Fatalf("inputs not preserved: scenario=%s, profile=%s", r.Inputs.Scenario, r.Inputs.Profile)
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
