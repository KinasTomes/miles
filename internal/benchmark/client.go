package benchmark

import (
	"context"
	"errors"
	"github.com/KinasTomes/miles/internal/observability"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/KinasTomes/miles/internal/secretfile"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

type payloadHashContextKey struct{}

// WithPayloadHash instructs the S3 client stack to use a precomputed SHA-256 hex digest
// for SigV4 payload signing, allowing non-seekable streaming io.Reader bodies.
func WithPayloadHash(ctx context.Context, hash string) context.Context {
	return context.WithValue(ctx, payloadHashContextKey{}, hash)
}

// Config is private run input. It is deliberately not copied into the artifact:
// endpoints, bucket names, key IDs and credential locations can identify accounts.
type Config struct {
	Target          string      `json:"target"` // direct-s3, miles-warm or local-s3-smoke
	Endpoint        string      `json:"endpoint"`
	Region          string      `json:"region"`
	Bucket          string      `json:"bucket"`
	CredentialsFile string      `json:"credentials_file"`
	KMSKeyID        string      `json:"kms_key_id,omitempty"`
	StorageClass    string      `json:"storage_class,omitempty"`
	DedupDomain     string      `json:"dedup_domain,omitempty"`
	Environment     Environment `json:"environment"`
}

// Environment records operator declarations, not measurements or readiness proof.
type Environment struct {
	Profile        string `json:"profile,omitempty"`
	Topology       string `json:"topology"`
	Host           string `json:"host"`
	CPU            string `json:"cpu"`
	Disk           string `json:"disk"`
	Network        string `json:"network"`
	Metadata       string `json:"metadata"`
	GatewayCommit  string `json:"gateway_commit"`
	PolicyRevision string `json:"policy_revision"`
	Encryption     string `json:"encryption"`
	CacheState     string `json:"cache_state"`
	Isolation      string `json:"isolation"`
	AsyncWork      string `json:"async_work"`
}

type credentials struct {
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	SessionToken    string `json:"session_token,omitempty"`
	UserID          string `json:"user_id,omitempty"` // accepts the local runtime file
}

func newClient(c Config, concurrency int, traffic *bodyTraffic) (*awss3.Client, func(), error) {
	if c.Target != "direct-s3" && c.Target != "miles-warm" && c.Target != "local-s3-smoke" {
		return nil, nil, errors.New("target must be direct-s3, miles-warm or local-s3-smoke")
	}
	if c.Region == "" || !bucketName.MatchString(c.Bucket) || c.CredentialsFile == "" ||
		c.Environment.Topology == "" || c.Environment.Host == "" || c.Environment.CPU == "" ||
		c.Environment.Disk == "" || c.Environment.Network == "" || c.Environment.Metadata == "" ||
		c.Environment.Encryption == "" || c.Environment.CacheState == "" || c.Environment.Isolation == "" || c.Environment.AsyncWork == "" {
		return nil, nil, errors.New("region, bucket, credentials_file and declared environment fields are required")
	}
	if c.Target == "miles-warm" && ((c.Environment.Metadata != "badger" && c.Environment.Metadata != "dynamodb" && c.Environment.Metadata != "dynamodb-local") || c.Environment.GatewayCommit == "" || c.Environment.PolicyRevision == "") {
		return nil, nil, errors.New("Miles requires declared Badger/DynamoDB backend, gateway_commit and policy_revision")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, nil, errors.New("endpoint must be an explicit origin without credentials, path or query")
	}
	loopback := net.ParseIP(u.Hostname()).IsLoopback()
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return nil, nil, errors.New("endpoint requires HTTPS, or literal loopback HTTP for local smoke")
	}
	if c.Target == "local-s3-smoke" && !loopback {
		return nil, nil, errors.New("local-s3-smoke requires literal loopback")
	}
	if c.Target == "direct-s3" && u.Scheme != "https" {
		return nil, nil, errors.New("direct-s3 requires HTTPS; use local-s3-smoke for an emulator")
	}
	b, err := secretfile.Read(c.CredentialsFile, 16<<10)
	if err != nil {
		return nil, nil, err
	}
	var cred credentials
	if err = decode(b, &cred); err != nil || cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
		return nil, nil, errors.New("invalid credential JSON")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DisableCompression = true // never transparently decode gzip object bytes
	transport.MaxIdleConnsPerHost = concurrency
	var rt http.RoundTripper = transport
	if traffic != nil {
		rt = &interceptTransport{
			base: transport,
			onResp: func(resp *http.Response) {
				if resp == nil {
					return
				}
				if d := resp.Header.Get("X-Dsg-Dedup-Domain"); d != "" {
					traffic.observedDomain.Store(&d)
				}
				hasTelemetry := false
				if dupStr := resp.Header.Get("X-Dsg-Chunks-Dup"); dupStr != "" {
					if v, err := strconv.ParseUint(dupStr, 10, 64); err == nil {
						traffic.chunksDup.Add(v)
						hasTelemetry = true
					}
				}
				if storedStr := resp.Header.Get("X-Dsg-Bytes-Stored"); storedStr != "" {
					if v, err := strconv.ParseUint(storedStr, 10, 64); err == nil {
						traffic.bytesStored.Add(v)
						hasTelemetry = true
					}
				}
				if hasTelemetry {
					traffic.telemetryResponses.Add(1)
				}
			},
		}
	}
	client := awss3.New(awss3.Options{
		Region: c.Region, BaseEndpoint: aws.String(c.Endpoint), UsePathStyle: true, RetryMaxAttempts: 1,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: cred.AccessKeyID, SecretAccessKey: cred.SecretAccessKey, SessionToken: cred.SessionToken}, nil
		}),
		HTTPClient: observability.CountHTTPClient(&http.Client{Transport: rt, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, func(n int) { traffic.sent.Add(uint64(n)) }, func(n int) { traffic.received.Add(uint64(n)) }),
		APIOptions: []func(*middleware.Stack) error{
			func(stack *middleware.Stack) error {
				return stack.Build.Add(middleware.BuildMiddlewareFunc("MilesSignDebugHeader", func(ctx context.Context, in middleware.BuildInput, next middleware.BuildHandler) (middleware.BuildOutput, middleware.Metadata, error) {
					if req, ok := in.Request.(*smithyhttp.Request); ok {
						req.Header.Set("X-Dsg-Debug", "1")
					}
					if h, ok := ctx.Value(payloadHashContextKey{}).(string); ok && h != "" {
						ctx = v4.SetPayloadHash(ctx, h)
					}
					return next.HandleBuild(ctx, in)
				}), middleware.Before)
			},
		},
	})
	return client, transport.CloseIdleConnections, nil
}

type interceptTransport struct {
	base   http.RoundTripper
	onResp func(*http.Response)
}

func (it *interceptTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := it.base.RoundTrip(req)
	if err == nil && resp != nil && it.onResp != nil {
		it.onResp(resp)
	}
	return resp, err
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var safeCode = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// Only a bounded S3 request ID may leave the client; exclude host IDs,
// locations, raw error messages and arbitrary headers.
var safeRequestID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

func responseDiagnostics(err error) (status int, requestID string) {
	if err == nil {
		return 0, ""
	}
	var response *smithyhttp.ResponseError
	if errors.As(err, &response) && response.Response != nil && response.Response.Response != nil {
		status = response.HTTPStatusCode()
		// Keep only a valid HTTP status, not transport failure details.
		if status < 100 || status > 599 {
			status = 0
		}
		requestID = response.Response.Header.Get("X-Amz-Request-Id")
	}
	var s3response awss3.ResponseError
	if requestID == "" && errors.As(err, &s3response) {
		requestID = s3response.ServiceRequestID()
	}
	if !safeRequestID.MatchString(requestID) {
		requestID = ""
	}
	return status, requestID
}

// ErrInterrupted indicates the benchmark was interrupted by signal (e.g. SIGINT).
var ErrInterrupted = errors.New("interrupted")

// Do not persist SDK error strings: they can include URLs, request IDs or bodies.
func errorCode(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrInterrupted) {
		return "interrupted"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	var api smithy.APIError
	if errors.As(err, &api) && safeCode.MatchString(api.ErrorCode()) {
		return api.ErrorCode()
	}
	return "operation_failed"
}

const requestTimeout = 30 * time.Minute

type bodyTraffic struct {
	sent, received     atomic.Uint64
	observedDomain     atomic.Pointer[string]
	chunksDup          atomic.Uint64
	bytesStored        atomic.Uint64
	telemetryResponses atomic.Uint64
}

func (t *bodyTraffic) getObservedDomain() string {
	if t == nil {
		return ""
	}
	p := t.observedDomain.Load()
	if p == nil {
		return ""
	}
	return *p
}

func (t *bodyTraffic) snapshot(target string) map[string]uint64 {
	if target == "miles-warm" {
		return map[string]uint64{"client_to_gateway": t.sent.Load(), "gateway_to_client": t.received.Load()}
	}
	return map[string]uint64{"client_to_s3": t.sent.Load(), "s3_to_client": t.received.Load()}
}
