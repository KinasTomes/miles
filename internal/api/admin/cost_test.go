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

func (f *fakeStats) Bucket(context.Context, string) (stats.BucketSummary, error) {
	return stats.BucketSummary{}, nil
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

	// Verify savings vs baseline computed
	if r.Results.NetSavingUSDVsBaseline == nil || r.Results.SavingsPercentVsBaseline == nil {
		t.Fatal("expected non-nil savings vs baseline")
	}
	if *r.Results.SavingsPercentVsBaseline <= 0 {
		t.Fatalf("expected positive savings percent, got %f", *r.Results.SavingsPercentVsBaseline)
	}
}
