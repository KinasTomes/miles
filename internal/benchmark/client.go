package benchmark

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/KinasTomes/miles/internal/secretfile"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// Config is private run input. It is deliberately not copied into the artifact:
// endpoints, bucket names, key IDs and credential locations can identify accounts.
type Config struct {
	Target          string      `json:"target"` // direct-s3, miles-warm or local-s3-smoke
	Endpoint        string      `json:"endpoint"`
	Region          string      `json:"region"`
	Bucket          string      `json:"bucket"`
	CredentialsFile string      `json:"credentials_file"`
	KMSKeyID        string      `json:"kms_key_id"`
	Environment     Environment `json:"environment"`
}

// Environment records operator declarations, not measurements or readiness proof.
type Environment struct {
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

func newClient(c Config, concurrency int) (*awss3.Client, func(), error) {
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
	client := awss3.New(awss3.Options{
		Region: c.Region, BaseEndpoint: aws.String(c.Endpoint), UsePathStyle: true, RetryMaxAttempts: 1,
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: cred.AccessKeyID, SecretAccessKey: cred.SecretAccessKey, SessionToken: cred.SessionToken}, nil
		}),
		HTTPClient: &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	})
	return client, transport.CloseIdleConnections, nil
}

var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
var safeCode = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

// Do not persist SDK error strings: they can include URLs, request IDs or bodies.
func errorCode(err error) string {
	if err == nil {
		return ""
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
