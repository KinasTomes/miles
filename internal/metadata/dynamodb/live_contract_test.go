package dynamodb

import (
	"context"
	"net/http"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage/contracttest"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

func liveClients(t *testing.T) (*ddb.Client, *s3.Client, string) {
	t.Helper()
	if os.Getenv("MILES_DDB_DEV_ACCEPTANCE") != "1" {
		t.Skip("AWS dev acceptance not enabled")
	}
	region, account := os.Getenv("AWS_REGION"), os.Getenv("MILES_AWS_DEV_ACCOUNT")
	credentials := aws.Credentials{
		AccessKeyID: os.Getenv("AWS_ACCESS_KEY_ID"), SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken: os.Getenv("AWS_SESSION_TOKEN"), Source: "explicit dev acceptance session",
	}
	if region == "" || !regexp.MustCompile("^[0-9]{12}$").MatchString(account) ||
		credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" || credentials.SessionToken == "" {
		t.Fatal("explicit AWS region, expected dev account and temporary session credentials required")
	}
	provider := aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) { return credentials, nil })
	httpClient := &http.Client{Timeout: 25 * time.Second}
	stsClient := sts.New(sts.Options{Region: region, Credentials: provider, HTTPClient: httpClient})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	identity, err := stsClient.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		t.Fatal("dev identity preflight", err)
	}
	if aws.ToString(identity.Account) != account {
		t.Fatal("AWS identity does not match expected dev account")
	}
	return ddb.New(ddb.Options{Region: region, Credentials: provider, HTTPClient: httpClient}),
		s3.New(s3.Options{Region: region, Credentials: provider, HTTPClient: httpClient}), account
}

func TestAWSDevBatchContract(t *testing.T) {
	client, _, _ := liveClients(t)
	f := contracttest.NewFixture()
	m, _ := fixtureStoreWithClient(t, f, client, "miles_hai_contract_", true)
	runBatchContract(t, f, m)
}

type trackedDevPacks struct {
	*s3.Client
	t       *testing.T
	account string
}

// Track exact version IDs from successful writes. Never delete by key alone.
func (p trackedDevPacks) PutObject(ctx context.Context, in *s3.PutObjectInput, options ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	in.ExpectedBucketOwner = aws.String(p.account)
	out, err := p.Client.PutObject(ctx, in, options...)
	if err != nil {
		return nil, err
	}
	if aws.ToString(out.VersionId) == "" || aws.ToString(out.VersionId) == "null" {
		p.t.Errorf("dev pack write returned no version ID for %s", aws.ToString(in.Key))
		return out, nil
	}
	bucket, key, version := *in.Bucket, *in.Key, *out.VersionId
	p.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := p.Client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &bucket, Key: &key, VersionId: &version, ExpectedBucketOwner: aws.String(p.account)}); err != nil {
			p.t.Error("cleanup dev object version", key, version, err)
			return
		}
		p.t.Log("removed test S3 object version")
	})
	return out, nil
}

func TestAWSDevWriteCommitWarmReadHandoff(t *testing.T) {
	metadata, physical, account := liveClients(t)
	bucket := os.Getenv("MILES_S3_DEV_BUCKET")
	if bucket == "" {
		t.Fatal("explicit S3 dev bucket required")
	}
	versioning, err := physical.GetBucketVersioning(t.Context(), &s3.GetBucketVersioningInput{Bucket: &bucket, ExpectedBucketOwner: &account})
	if err != nil {
		t.Fatal("S3 bucket preflight", err)
	}
	if versioning.Status != s3types.BucketVersioningStatusEnabled {
		t.Fatal("S3 dev bucket versioning must be enabled")
	}
	f := contracttest.NewFixture()
	f.Quota.Limit = 1 << 20
	m, _ := fixtureStoreWithClient(t, f, metadata, "miles_hai_contract_", true)
	runWriteCommitWarmReadHandoff(t, f, m, trackedDevPacks{Client: physical, t: t, account: account}, bucket, os.Getenv("MILES_S3_DEV_KMS_KEY"))
}
