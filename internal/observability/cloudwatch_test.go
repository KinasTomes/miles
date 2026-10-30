package observability

import (
	"compress/gzip"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/jobs"
	"github.com/aws/aws-sdk-go-v2/aws"
	cw "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
)

type captureCloudWatch struct {
	calls []*cw.PutMetricDataInput
	fail  bool
}

func (c *captureCloudWatch) PutMetricData(_ context.Context, in *cw.PutMetricDataInput, options ...func(*cw.Options)) (*cw.PutMetricDataOutput, error) {
	o := cw.Options{}
	for _, f := range options {
		f(&o)
	}
	if o.RetryMaxAttempts != 1 {
		return nil, errors.New("hidden retries enabled")
	}
	c.calls = append(c.calls, in)
	if c.fail {
		return nil, errors.New("secret-from-server")
	}
	return &cw.PutMetricDataOutput{}, nil
}
func TestCloudWatchBatchDeltasAndFailures(t *testing.T) {
	api := new(captureCloudWatch)
	export, e := NewCloudWatch(api, "cloud")
	if e != nil {
		t.Fatal(e)
	}
	h := NewHealth(func(context.Context) error { return nil }, nil, true)
	o := new(Operations)
	h.AttachOperations(o, func() int64 { return 3 }, nil)
	h.refresh(context.Background())
	o.JobUpdate("gc", jobs.Running)
	o.JobUpdate("gc", jobs.Running)
	flush := func() {
		t.Helper()
		if e := export.Flush(context.Background(), time.Now(), h.Snapshot()); e != nil {
			t.Fatal(e)
		}
	}
	flush()
	if len(api.calls) != 1 || len(api.calls[0].MetricData) != 7 || aws.ToString(api.calls[0].Namespace) != "Miles/DSG" {
		t.Fatal("not one bounded batch", api.calls)
	}
	value := func(in *cw.PutMetricDataInput) float64 {
		for _, d := range in.MetricData {
			if aws.ToString(d.MetricName) == "miles_job_updates_total" {
				return aws.ToFloat64(d.Value)
			}
		}
		return -1
	}
	if value(api.calls[0]) != 2 {
		t.Fatal("initial counter delta")
	}
	flush()
	if value(api.calls[1]) != -1 {
		t.Fatal("counter was re-summed")
	}
	o.JobUpdate("gc", jobs.Running)
	api.fail = true
	if e := export.Flush(context.Background(), time.Now(), h.Snapshot()); e == nil || strings.Contains(e.Error(), "secret") {
		t.Fatal("missing/redacted failure", e)
	}
	if export.Failures() != 1 {
		t.Fatal("failure not visible")
	}
	api.fail = false
	flush()
	if value(api.calls[len(api.calls)-1]) != 1 {
		t.Fatal("failed export lost delta")
	}
	for _, samples := range [][]Metric{
		{{Name: "miles_active_requests", Value: math.NaN()}},
		{{Name: "miles_active_requests", Value: 1, Labels: map[string]string{"node_id": "private-node"}}},
		{{Name: "miles_job_updates_total", Counter: true, Value: 1, Labels: map[string]string{"job_type": "secret-job-id", "state": "running"}}},
		{{Name: "miles_active_requests", Value: 1}, {Name: "miles_active_requests", Value: 2}},
	} {
		before := len(api.calls)
		if export.Flush(context.Background(), time.Now(), samples) == nil || len(api.calls) != before {
			t.Fatal("invalid series reached AWS")
		}
	}
}
func TestCloudWatchNativeSDKLoopbackBatch(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var reader io.Reader = r.Body
		if r.Header.Get("Content-Encoding") == "gzip" {
			compressed, e := gzip.NewReader(r.Body)
			if e != nil {
				t.Error(e)
				return
			}
			defer compressed.Close()
			reader = compressed
		}
		body, e := io.ReadAll(reader)
		if e != nil || len(body) > 1<<20 || !strings.Contains(string(body), "Miles") || !strings.Contains(string(body), "miles_ready") {
			t.Error("invalid native batch", e)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			t.Error("SDK did not sign")
		}
		w.Header().Set("Content-Type", "application/cbor")
		w.Header().Set("Smithy-Protocol", "rpc-v2-cbor")
		w.Write([]byte{0xa0}) // Native SDK's CBOR empty response structure.
	}))
	defer server.Close()
	client := cw.New(cw.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(server.URL), Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
		return aws.Credentials{AccessKeyID: "fixture", SecretAccessKey: "fixture"}, nil
	})})
	export, e := NewCloudWatch(client, "edge")
	if e != nil {
		t.Fatal(e)
	}
	h := NewHealth(nil, nil, false)
	if e = export.Flush(context.Background(), time.Now(), h.Snapshot()); e != nil {
		t.Fatal(e)
	}
	if calls != 1 {
		t.Fatal(calls)
	}
}

func TestCloudWatchFullMetricInventoryFitsOneBatch(t *testing.T) {
	h := NewHealth(func(context.Context) error { return errors.New("dependency unavailable") }, nil, false)
	o := new(Operations)
	h.AttachOperations(o, func() int64 { return 2 }, nil)
	h.refresh(context.Background()) // Give the dependency-failure counter a nonzero delta too.
	for _, kind := range jobs.Types() {
		for _, state := range jobs.States() {
			o.JobUpdate(kind, state)
		}
	}
	api := new(captureCloudWatch)
	export, e := NewCloudWatch(api, "cloud")
	if e != nil {
		t.Fatal(e)
	}
	samples := h.Snapshot()
	if e = export.Flush(context.Background(), time.Now(), samples); e != nil {
		t.Fatalf("full metric inventory (%d series) cannot be exported: %v", len(samples), e)
	}
	if len(api.calls) != 1 || len(api.calls[0].MetricData) != len(samples) {
		t.Fatal("full inventory was dropped or split")
	}
}

func TestOperationsCardinalityAndNoBackendFromScrape(t *testing.T) {
	probes := 0
	h := NewHealth(func(context.Context) error { probes++; return nil }, nil, true)
	o := new(Operations)
	h.AttachOperations(o, func() int64 { return 2 }, nil)
	for _, kind := range jobs.Types() {
		for _, state := range jobs.States() {
			o.JobUpdate(kind, state)
		}
	}
	o.JobUpdate("object-key", jobs.Running)
	for range 3 {
		r := httptest.NewRecorder()
		h.Handler().ServeHTTP(r, httptest.NewRequest("GET", "/metrics", nil))
		if strings.Contains(r.Body.String(), "object-key") || !strings.Contains(r.Body.String(), "miles_active_requests 2") {
			t.Fatal(r.Body.String())
		}
	}
	if probes != 0 || len(o.Snapshot()) != len(jobs.Types())*len(jobs.States()) {
		t.Fatal("unbounded labels or scrape did I/O")
	}
}
