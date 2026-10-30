package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/KinasTomes/miles/internal/api/admin"
	"github.com/KinasTomes/miles/internal/config"
	"github.com/KinasTomes/miles/internal/metadata/badger"
	ddbstore "github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type bootstrapOptions struct {
	user, tokenFile, dataPath, prefix, region, endpoint string
	ttl                                                 time.Duration
}

var tablePrefix = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,200}$`)

func parseBootstrap(args []string) (bootstrapOptions, error) {
	var o bootstrapOptions
	f := newFlags("admin-token bootstrap")
	f.StringVar(&o.user, "user", "", "system admin user_id")
	f.DurationVar(&o.ttl, "ttl", 24*time.Hour, "lifetime 5m..168h")
	f.StringVar(&o.tokenFile, "token-file", "", "new 0600 file for the token")
	f.StringVar(&o.dataPath, "local-data-path", "", "local/benchmark data directory")
	f.StringVar(&o.prefix, "ddb-table-prefix", "", "DynamoDB table name prefix")
	f.StringVar(&o.region, "region", "", "DynamoDB region")
	f.StringVar(&o.endpoint, "ddb-endpoint", "", "loopback HTTP DynamoDB endpoint (DynamoDB Local)")
	recover := f.Bool("recover", false, "R2 recovery")
	if _, ok := parse(f, args); !ok {
		return o, errors.New("invalid admin-token bootstrap flags; use -help")
	}
	if *recover {
		return o, errors.New("admin-token bootstrap --recover: R2 recovery gate not implemented")
	}
	if !idPattern.MatchString(o.user) || o.tokenFile == "" || !validTTL(o.ttl) {
		return o, errors.New("bootstrap needs --user, --token-file and --ttl 5m..168h in whole seconds")
	}
	ddb := o.prefix != "" || o.region != "" || o.endpoint != ""
	if (o.dataPath == "") == !ddb {
		return o, errors.New("bootstrap needs exactly one of --local-data-path or --ddb-table-prefix with --region")
	}
	if ddb {
		if !tablePrefix.MatchString(o.prefix) || o.region == "" || strings.ContainsAny(o.region, "/ \t\r\n") {
			return o, errors.New("invalid --ddb-table-prefix or --region")
		}
		if o.endpoint != "" {
			u, err := url.Parse(o.endpoint)
			if err != nil || u.Scheme != "http" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
				return o, errors.New("--ddb-endpoint must be a loopback HTTP origin")
			}
			a, err := netip.ParseAddrPort(u.Host)
			if err != nil || !a.Addr().Unmap().IsLoopback() {
				return o, errors.New("--ddb-endpoint must be a loopback HTTP origin")
			}
		}
	}
	return o, nil
}

// bootstrap provisions the first system-admin token offline (API v2 6.4). It
// never talks to the gateway; the token file is durable before the commit so
// a committed installation never loses its only token.
func bootstrap(ctx context.Context, args []string, stdout io.Writer) (err error) {
	o, err := parseBootstrap(args)
	if err != nil {
		return err
	}
	store, closeStore, err := openBootstrapStore(o)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeStore()) }()
	validator, err := config.NewPolicyValidator(config.DSGPolicyLimits())
	if err != nil {
		return err
	}
	global, err := validator.NewGlobalSnapshot(1, config.DefaultGlobalPolicy())
	if err != nil {
		return err
	}
	policy, err := global.StorageSnapshot()
	if err != nil {
		return err
	}
	policy.Revision = 0
	id, secret, token, err := admin.NewToken()
	if err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(secret))
	now := time.Now().UTC().Truncate(time.Second)
	record := storage.AdminToken{ID: id, UserID: o.user, Scope: storage.TokenScopeSystem, Hash: hash[:], Active: true, IssuedAt: now, ExpiresAt: now.Add(o.ttl)}
	file, err := createSecretFile(o.tokenFile)
	if err != nil {
		return err
	}
	if err := writeSecret(file, token); err != nil {
		_ = os.Remove(o.tokenFile)
		return errors.New("cannot persist token file")
	}
	err = store.BootstrapAdmin(ctx, storage.AdminBootstrap{Token: record, QuotaLimit: 1 << 40, Policy: policy})
	if err != nil {
		// Only conflict/invalid prove nothing committed; otherwise the token may be live.
		if !errors.Is(err, storage.ErrConflict) && !errors.Is(err, storage.ErrInvalid) {
			return fmt.Errorf("bootstrap outcome unknown (%w); token file %s kept; check the installation marker before retrying", err, o.tokenFile)
		}
		_ = os.Remove(o.tokenFile)
		if errors.Is(err, storage.ErrConflict) {
			return errors.New("installation already bootstrapped; mint more tokens with admin-token create under a system token")
		}
		return fmt.Errorf("bootstrap failed: %w", err)
	}
	_, err = fmt.Fprintf(stdout, "{\"token_id\":%q,\"expires_at\":%q}\n", id, record.ExpiresAt.Format(time.RFC3339))
	return err
}

var openBootstrapStore = func(o bootstrapOptions) (storage.IdentityStore, func() error, error) {
	if o.dataPath != "" {
		if err := os.MkdirAll(o.dataPath, 0700); err != nil {
			return nil, nil, err
		}
		// Badger's directory lock makes this fail while the gateway runs.
		m, err := badger.Open(badger.Options{Path: filepath.Join(o.dataPath, "metadata"), RequireProducers: true})
		if err != nil {
			return nil, nil, fmt.Errorf("cannot open local metadata (is the gateway running?): %w", err)
		}
		return m, m.Close, nil
	}
	cred := aws.Credentials{AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN")}
	if cred.AccessKeyID == "" || cred.SecretAccessKey == "" {
		return nil, nil, errors.New("DynamoDB bootstrap needs AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY of the provisioning role")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	options := awsddb.Options{Region: o.region, HTTPClient: &http.Client{Transport: transport},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return cred, nil })}
	if o.endpoint != "" {
		options.BaseEndpoint = aws.String(o.endpoint)
	}
	tables := map[string]string{}
	for name := range ddbstore.Tables() {
		tables[name] = o.prefix + name
	}
	m, err := ddbstore.New(ddbstore.Options{API: awsddb.New(options), Tables: tables, Authority: denyWriters{}})
	if err != nil {
		return nil, nil, err
	}
	return m, func() error { transport.CloseIdleConnections(); return nil }, nil
}

// denyWriters: bootstrap writes no fenced data, so no writer is ever admitted.
type denyWriters struct{}

func (denyWriters) Check(context.Context, storage.WriterFence, ddbstore.Observe) error {
	return storage.ErrAccessDenied
}
