package observability

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go/middleware"
)

func valueOf(t *testing.T, samples []Metric, name string) float64 {
	t.Helper()
	for _, s := range samples {
		if s.Name == name {
			return s.Value
		}
	}
	t.Fatalf("missing %s", name)
	return 0
}
func TestDynamoDBNativeCapacityAndLeaseAttempts(t *testing.T) {
	m := new(Runtime)
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if input["ReturnConsumedCapacity"] != "TOTAL" {
			t.Error("no capacity reporting")
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		switch calls {
		case 1:
			io.WriteString(w, `{"Item":{},"ConsumedCapacity":{"CapacityUnits":0.5,"ReadCapacityUnits":0.5}}`)
		case 2:
			io.WriteString(w, `{"ConsumedCapacity":[{"CapacityUnits":4,"WriteCapacityUnits":4}]}`)
		case 3:
			w.WriteHeader(400)
			io.WriteString(w, `{"__type":"TransactionCanceledException","CancellationReasons":[{"Code":"ThrottlingError"}]}`)
		}
	}))
	defer server.Close()
	client := ddb.New(ddb.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(server.URL), RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "fixture", SecretAccessKey: "fixture"}, nil
		}),
		HTTPClient: m.HTTPClient(server.Client(), true), APIOptions: []func(*middleware.Stack) error{m.DynamoDB("test-Leases")}})
	_, err := client.GetItem(t.Context(), &ddb.GetItemInput{TableName: aws.String("test-Objects"), Key: map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "private-id"}}})
	if err != nil {
		t.Fatal(err)
	}
	input := &ddb.TransactWriteItemsInput{ReturnConsumedCapacity: types.ReturnConsumedCapacityTotal, ClientRequestToken: aws.String("stable-token"), TransactItems: []types.TransactWriteItem{
		{Put: &types.Put{TableName: aws.String("test-Leases"), Item: map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "private-lease"}}}},
		{ConditionCheck: &types.ConditionCheck{TableName: aws.String("test-Leases"), Key: map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "guard"}}, ConditionExpression: aws.String("attribute_exists(id)")}},
	}}
	_, err = client.TransactWriteItems(t.Context(), input, func(o *ddb.Options) { o.Retryer = aws.NopRetryer{} })
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.TransactWriteItems(t.Context(), input, func(o *ddb.Options) { o.Retryer = aws.NopRetryer{} })
	if err == nil {
		t.Fatal("throttle swallowed")
	}
	samples := m.Snapshot(time.Now())
	for name, want := range map[string]float64{"miles_ddb_calls_total": 3, "miles_ddb_transactions_total": 2, "miles_ddb_lease_write_attempts_total": 2, "miles_ddb_consumed_units_total": 4.5, "miles_ddb_throttles_total": 1, "miles_ddb_capacity_responses_total": 2} {
		if got := valueOf(t, samples, name); got != want {
			t.Fatalf("%s=%v want %v", name, got, want)
		}
	}
	if calls != 3 || input.ClientRequestToken == nil || *input.ClientRequestToken != "stable-token" {
		t.Fatal("retry or idempotency changed")
	}
	if valueOf(t, samples, "miles_metadata_request_body_bytes_total") <= 0 || valueOf(t, samples, "miles_metadata_response_body_bytes_total") <= 0 {
		t.Fatal("metadata traffic missing")
	}
	for _, s := range samples {
		if !validMetric(s) || len(s.Labels) != 0 {
			t.Fatal("unbounded/invalid sample", s)
		}
	}
}

func TestHTTPBodyCountersMeasureConsumedBytesAndStreaming(t *testing.T) {
	m := new(Runtime)
	server := httptest.NewServer(m.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(200)
		io.WriteString(w, "abcd")
		http.NewResponseController(w).Flush()
	})))
	defer server.Close()
	var sent, received int
	client := CountHTTPClient(server.Client(), func(n int) { sent += n }, func(n int) { received += n })
	req, _ := http.NewRequest("POST", server.URL, strings.NewReader("xyz"))
	out, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 2)
	if _, err = io.ReadFull(out.Body, data); err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	if sent != 3 || received != 2 {
		t.Fatal(sent, received)
	}
	samples := m.Snapshot(time.Now())
	if valueOf(t, samples, "miles_client_gateway_body_bytes_total") != 3 || valueOf(t, samples, "miles_gateway_client_body_bytes_total") != 4 {
		t.Fatal(samples)
	}
}

func TestRuntimeStaleGaugesAndScrapeNoIO(t *testing.T) {
	m := new(Runtime)
	now := time.Now()
	m.Observe(now, 3700, 120, 70, 1)
	h := NewHealth(nil, nil, false)
	h.AttachRuntime(m)
	h.now = func() time.Time { return now }
	for range 2 {
		w := httptest.NewRecorder()
		h.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/metrics", nil))
		if !strings.Contains(w.Body.String(), "miles_stuck_completing_uploads 1") {
			t.Fatal(w.Body.String())
		}
	}
	samples := m.Snapshot(now)
	if valueOf(t, samples, "miles_journal_export_lag_seconds") != 70 {
		t.Fatal(samples)
	}
	for _, s := range m.Snapshot(now.Add(time.Minute)) {
		for _, name := range runtimeGauges {
			if s.Name == name {
				t.Fatal("stale gauge exported", s)
			}
		}
	}
}

func TestHTTPCountPreservesNoBodyFraming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 || len(r.TransferEncoding) != 0 {
			t.Error("empty request became chunked")
		}
		w.WriteHeader(204)
	}))
	defer server.Close()
	client := CountHTTPClient(server.Client(), func(int) { t.Error("unexpected request body") }, func(int) {})
	request, _ := http.NewRequest("GET", server.URL, http.NoBody)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}

func TestDynamoDBMetricsPreserveLegacyFrozenRequest(t *testing.T) {
	m := new(Runtime)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
		}
		if _, present := input["ReturnConsumedCapacity"]; present {
			t.Error("changed frozen idempotency parameters")
		}
		w.Header().Set("Content-Type", "application/x-amz-json-1.0")
		io.WriteString(w, `{}`)
	}))
	defer server.Close()
	client := ddb.New(ddb.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(server.URL), RetryMaxAttempts: 1,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "fixture", SecretAccessKey: "fixture"}, nil
		}),
		APIOptions: []func(*middleware.Stack) error{m.DynamoDB("test-Leases")}})
	_, err := client.TransactWriteItems(t.Context(), &ddb.TransactWriteItemsInput{ClientRequestToken: aws.String("legacy-frozen-token"), TransactItems: []types.TransactWriteItem{{Put: &types.Put{TableName: aws.String("test-Objects"), Item: map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "fixture"}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if valueOf(t, m.Snapshot(time.Now()), "miles_ddb_capacity_responses_total") != 0 {
		t.Fatal("invented capacity")
	}
}
