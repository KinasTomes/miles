// Package benchmarknative binds acceptance observers to an explicit AWS installation.
package benchmarknative

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/secretfile"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Config is private run input, never a declaration that evidence has passed.
type Config struct {
	Account       string            `json:"account"`
	RoleARN       string            `json:"role_arn"`
	Region        string            `json:"region"`
	Bucket        string            `json:"bucket"`
	LogicalBucket string            `json:"logical_bucket"`
	KMSKey        string            `json:"kms_key"`
	Tables        map[string]string `json:"tables"`
	Tags          map[string]string `json:"tags"`
	RunID         string            `json:"run_id"`
	Slot          string            `json:"slot"`
	Source        string            `json:"source"`
	History       string            `json:"history"`
	Incarnation   string            `json:"incarnation"`
	StopAt        time.Time         `json:"stop_at"`
}

func Read(path string) (Config, error) {
	var c Config
	b, err := secretfile.Read(path, 32<<10)
	if err != nil {
		return c, err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF {
		return c, errors.New("invalid native configuration")
	}
	return c, c.Validate()
}

func (c Config) Validate() error {
	if !regexp.MustCompile(`^[0-9]{12}$`).MatchString(c.Account) ||
		!strings.HasPrefix(c.RoleARN, "arn:aws:iam::"+c.Account+":role/") ||
		!regexp.MustCompile(`^[a-z]{2}-[a-z]+-[0-9]$`).MatchString(c.Region) ||
		!regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`).MatchString(c.Bucket) ||
		!regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`).MatchString(c.LogicalBucket) ||
		!strings.HasPrefix(c.KMSKey, "arn:aws:kms:"+c.Region+":"+c.Account+":key/") ||
		!regexp.MustCompile(`^[a-z0-9-]{1,100}$`).MatchString(c.RunID) ||
		!regexp.MustCompile(`^[a-z0-9-]+-rep[1-3]$`).MatchString(c.Slot) ||
		!regexp.MustCompile(`^[a-f0-9]{40}$`).MatchString(c.Source) ||
		!regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(c.History) ||
		!regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(c.Incarnation) ||
		c.Tags["Project"] != "Miles" || c.Tags["Environment"] == "" || c.StopAt.IsZero() {
		return errors.New("native configuration is not bound to a run/installation")
	}
	return nil
}

// Session ignores ambient profiles, endpoints, credentials and proxy redirects.
// The caller supplies only a short-lived exported deploy session, never DSG keys.
func Session(ctx context.Context, c Config, path string) (aws.Config, error) {
	var zero aws.Config
	if err := c.Validate(); err != nil {
		return zero, err
	}
	var key struct {
		Version                                    int
		AccessKeyId, SecretAccessKey, SessionToken string
		Expiration                                 time.Time
	}
	b, err := secretfile.Read(path, 32<<10)
	if err != nil {
		return zero, err
	}
	if json.Unmarshal(b, &key) != nil || key.Version != 1 || !strings.HasPrefix(key.AccessKeyId, "ASIA") ||
		key.SecretAccessKey == "" || key.SessionToken == "" || time.Until(key.Expiration) < 5*time.Minute ||
		!key.Expiration.After(c.StopAt) || !time.Now().Before(c.StopAt) {
		return zero, errors.New("native observer requires temporary AWS credentials valid through stop_at")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	cfg := aws.Config{Region: c.Region, RetryMaxAttempts: 1,
		HTTPClient: &http.Client{Transport: transport, Timeout: 30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: key.AccessKeyId, SecretAccessKey: key.SecretAccessKey,
				SessionToken: key.SessionToken, CanExpire: true, Expires: key.Expiration}, nil
		})}
	id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	role := c.RoleARN[strings.LastIndex(c.RoleARN, "/")+1:]
	if err != nil || aws.ToString(id.Account) != c.Account ||
		!strings.HasPrefix(aws.ToString(id.Arn), "arn:aws:sts::"+c.Account+":assumed-role/"+role+"/") {
		return zero, errors.New("native observer AWS account/role mismatch")
	}
	return cfg, nil
}
