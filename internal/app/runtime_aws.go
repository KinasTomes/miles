package app

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"github.com/KinasTomes/miles/internal/config"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials/ec2rolecreds"
	"github.com/aws/aws-sdk-go-v2/credentials/processcreds"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	cw "github.com/aws/aws-sdk-go-v2/service/cloudwatch"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
	sm "github.com/aws/aws-sdk-go-v2/service/secretsmanager"
)

// AWS and gateway credentials are different identities. No static credentials,
// SDK endpoint overrides, developer login or fallback chain enter this wiring.
func runtimeCredentials(ctx context.Context, c config.Startup, h *http.Client) (*aws.CredentialsCache, error) {
	var provider aws.CredentialsProvider
	if c.Profile == "cloud" {
		provider = ec2rolecreds.New(func(o *ec2rolecreds.Options) {
			o.Client = imds.New(imds.Options{Endpoint: "http://169.254.169.254", EnableFallback: aws.FalseTernary, HTTPClient: h})
		})
	} else {
		profile, err := awsconfig.LoadSharedConfigProfile(ctx, "miles-runtime", func(o *awsconfig.LoadSharedConfigOptions) {
			o.ConfigFiles = []string{c.AWSConfigFile}
			o.CredentialsFiles = []string{}
		})
		if err != nil || profile.CredentialProcess == "" || profile.Credentials.AccessKeyID != "" || profile.RoleARN != "" || profile.SourceProfileName != "" || profile.CredentialSource != "" || profile.WebIdentityTokenFile != "" || profile.SSOSessionName != "" {
			return nil, errors.New("Edge requires only credential_process in miles-runtime profile")
		}
		provider = processcreds.NewProvider(profile.CredentialProcess, func(o *processcreds.Options) { o.Timeout = 10 * time.Second })
	}
	return aws.NewCredentialsCache(temporaryCredentials{provider}, func(o *aws.CredentialsCacheOptions) { o.ExpiryWindow = time.Minute }), nil
}

type temporaryCredentials struct{ aws.CredentialsProvider }

func (p temporaryCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	c, err := p.CredentialsProvider.Retrieve(ctx)
	if err != nil || !c.CanExpire || c.Expires.IsZero() || c.SessionToken == "" || c.AccessKeyID == "" || c.SecretAccessKey == "" || !time.Now().Before(c.Expires) {
		return aws.Credentials{}, errors.New("runtime temporary AWS credentials unavailable")
	}
	return c, nil
}

type runtimeClients struct {
	metadata  *ddb.Client
	metrics   *cw.Client
	packs     *s3.Client
	secrets   *sm.Client
	transport *http.Transport
}

func newRuntimeClients(ctx context.Context, c config.Startup) (runtimeClients, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	t.ResponseHeaderTimeout = 15 * time.Second
	h := &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	p, err := runtimeCredentials(ctx, c, h)
	if err != nil {
		t.CloseIdleConnections()
		return runtimeClients{}, err
	}
	return runtimeClients{metrics: cw.New(cw.Options{Region: c.Region, Credentials: p, HTTPClient: h, RetryMaxAttempts: 1}), metadata: ddb.New(ddb.Options{Region: c.Region, Credentials: p, HTTPClient: h, RetryMaxAttempts: 1}), packs: s3.New(s3.Options{Region: c.Region, Credentials: p, HTTPClient: h, RetryMaxAttempts: 1}), secrets: sm.New(sm.Options{Region: c.Region, Credentials: p, HTTPClient: h, RetryMaxAttempts: 1}), transport: t}, nil
}

type runtimeKeys struct {
	KeyID     string `json:"key_id"`
	SealKey   string `json:"seal_key"`
	CursorKey string `json:"cursor_key"`
}

func (c runtimeClients) keys(ctx context.Context, id string) (runtimeKeys, []byte, []byte, error) {
	out, err := c.secrets.GetSecretValue(ctx, &sm.GetSecretValueInput{SecretId: aws.String(id)})
	if err != nil || out == nil || out.SecretString == nil || len(*out.SecretString) > 4096 {
		return runtimeKeys{}, nil, nil, errors.New("shared runtime secret unavailable")
	}
	var k runtimeKeys
	if json.Unmarshal([]byte(*out.SecretString), &k) != nil || k.KeyID == "" {
		return k, nil, nil, errors.New("invalid runtime key document")
	}
	seal, e1 := base64.StdEncoding.Strict().DecodeString(k.SealKey)
	cursor, e2 := base64.StdEncoding.Strict().DecodeString(k.CursorKey)
	if e1 != nil || e2 != nil || len(seal) != 32 || len(cursor) != 32 || bytes.Equal(seal, cursor) {
		return k, nil, nil, errors.New("runtime keys must be independent 32-byte values")
	}
	return k, seal, cursor, nil
}

func runtimeDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("runtime local_data_path must be a non-symlink 0700 directory")
	}
	return nil
}
