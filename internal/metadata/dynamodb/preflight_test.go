package dynamodb_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/metadata/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)
type mockPreflightDDB struct {
	tables map[string]*ddbtypes.TableDescription
	tags   map[string][]ddbtypes.Tag
}

func (m *mockPreflightDDB) DescribeTable(_ context.Context, params *ddb.DescribeTableInput, _ ...func(*ddb.Options)) (*ddb.DescribeTableOutput, error) {
	name := aws.ToString(params.TableName)
	t, ok := m.tables[name]
	if !ok {
		return nil, &ddbtypes.ResourceNotFoundException{Message: aws.String("table not found")}
	}
	return &ddb.DescribeTableOutput{Table: t}, nil
}

func (m *mockPreflightDDB) ListTagsOfResource(_ context.Context, params *ddb.ListTagsOfResourceInput, _ ...func(*ddb.Options)) (*ddb.ListTagsOfResourceOutput, error) {
	arn := aws.ToString(params.ResourceArn)
	tags := m.tags[arn]
	return &ddb.ListTagsOfResourceOutput{Tags: tags}, nil
}

type mockPreflightS3 struct {
	status s3types.BucketVersioningStatus
	err    error
}

func (m *mockPreflightS3) GetBucketVersioning(_ context.Context, _ *s3.GetBucketVersioningInput, _ ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &s3.GetBucketVersioningOutput{Status: m.status}, nil
}

func valid20TablesMock(prefix, account, region string) *mockPreflightDDB {
	tables := make(map[string]*ddbtypes.TableDescription)
	tags := make(map[string][]ddbtypes.Tag)

	for logical, spec := range dynamodb.Tables() {
		physical := prefix + logical
		arn := "arn:aws:dynamodb:" + region + ":" + account + ":table/" + physical

		attrs := []ddbtypes.AttributeDefinition{
			{AttributeName: aws.String(spec.Partition), AttributeType: ddbtypes.ScalarAttributeTypeS},
		}
		keys := []ddbtypes.KeySchemaElement{
			{AttributeName: aws.String(spec.Partition), KeyType: ddbtypes.KeyTypeHash},
		}

		if spec.Sort != "" {
			attrs = append(attrs, ddbtypes.AttributeDefinition{
				AttributeName: aws.String(spec.Sort),
				AttributeType: ddbtypes.ScalarAttributeType(spec.SortType),
			})
			keys = append(keys, ddbtypes.KeySchemaElement{
				AttributeName: aws.String(spec.Sort),
				KeyType:       ddbtypes.KeyTypeRange,
			})
		}

		tables[physical] = &ddbtypes.TableDescription{
			TableName:            aws.String(physical),
			TableArn:             aws.String(arn),
			TableStatus:          ddbtypes.TableStatusActive,
			KeySchema:            keys,
			AttributeDefinitions: attrs,
		}

		tags[arn] = []ddbtypes.Tag{
			{Key: aws.String("Project"), Value: aws.String("Miles")},
			{Key: aws.String("Environment"), Value: aws.String("dev")},
		}
	}

	return &mockPreflightDDB{tables: tables, tags: tags}
}

func defaultPreflightConfig() dynamodb.PreflightInventoryConfig {
	return dynamodb.PreflightInventoryConfig{
		AccountID:   "499388132734",
		Region:      "ap-southeast-1",
		TablePrefix: "miles-dev-",
		BucketName:  "miles-dev-data-499388132734-ap-southeast-1",
		RequiredTags: map[string]string{
			"Project":     "Miles",
			"Environment": "dev",
		},
	}
}

func TestPreflightValidExactSchemaPasses(t *testing.T) {
	cfg := defaultPreflightConfig()
	ddbMock := valid20TablesMock(cfg.TablePrefix, cfg.AccountID, cfg.Region)
	s3Mock := &mockPreflightS3{status: s3types.BucketVersioningStatusEnabled}

	err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfg)
	if err != nil {
		t.Fatalf("expected valid preflight to pass, got: %v", err)
	}
}

func TestPreflightRejectsMalformedPartitionKey(t *testing.T) {
	cfg := defaultPreflightConfig()
	ddbMock := valid20TablesMock(cfg.TablePrefix, cfg.AccountID, cfg.Region)
	s3Mock := &mockPreflightS3{status: s3types.BucketVersioningStatusEnabled}

	// Corrupt Buckets table to have partition key "id" instead of "bucket" (reproducing CloudTrail bug)
	bucketsDesc := ddbMock.tables["miles-dev-Buckets"]
	bucketsDesc.KeySchema = []ddbtypes.KeySchemaElement{
		{AttributeName: aws.String("id"), KeyType: ddbtypes.KeyTypeHash},
	}
	bucketsDesc.AttributeDefinitions = []ddbtypes.AttributeDefinition{
		{AttributeName: aws.String("id"), AttributeType: ddbtypes.ScalarAttributeTypeS},
	}

	err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfg)
	if err == nil || !errors.Is(err, dynamodb.ErrPreflightStop) {
		t.Fatalf("expected ErrPreflightStop for malformed partition key, got: %v", err)
	}
	if !strings.Contains(err.Error(), "miles-dev-Buckets partition key mismatch") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPreflightRejectsMissingSortKey(t *testing.T) {
	cfg := defaultPreflightConfig()
	ddbMock := valid20TablesMock(cfg.TablePrefix, cfg.AccountID, cfg.Region)
	s3Mock := &mockPreflightS3{status: s3types.BucketVersioningStatusEnabled}

	// Corrupt Objects table to drop sort key "key"
	objectsDesc := ddbMock.tables["miles-dev-Objects"]
	objectsDesc.KeySchema = []ddbtypes.KeySchemaElement{
		{AttributeName: aws.String("bucket"), KeyType: ddbtypes.KeyTypeHash},
	}

	err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfg)
	if err == nil || !strings.Contains(err.Error(), "miles-dev-Objects sort key mismatch") {
		t.Fatalf("expected sort key mismatch error, got: %v", err)
	}
}

func TestPreflightRejectsWrongSortType(t *testing.T) {
	cfg := defaultPreflightConfig()
	ddbMock := valid20TablesMock(cfg.TablePrefix, cfg.AccountID, cfg.Region)
	s3Mock := &mockPreflightS3{status: s3types.BucketVersioningStatusEnabled}

	// Corrupt ManifestPages sort key "page_no" to string "S" instead of number "N"
	desc := ddbMock.tables["miles-dev-ManifestPages"]
	for i, attr := range desc.AttributeDefinitions {
		if aws.ToString(attr.AttributeName) == "page_no" {
			desc.AttributeDefinitions[i].AttributeType = ddbtypes.ScalarAttributeTypeS
		}
	}

	err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfg)
	if err == nil || !strings.Contains(err.Error(), "type mismatch") {
		t.Fatalf("expected type mismatch error, got: %v", err)
	}
}

func TestPreflightRejectsUnexpectedSortKey(t *testing.T) {
	cfg := defaultPreflightConfig()
	ddbMock := valid20TablesMock(cfg.TablePrefix, cfg.AccountID, cfg.Region)
	s3Mock := &mockPreflightS3{status: s3types.BucketVersioningStatusEnabled}

	// Add an illegal sort key to Users table
	usersDesc := ddbMock.tables["miles-dev-Users"]
	usersDesc.KeySchema = append(usersDesc.KeySchema, ddbtypes.KeySchemaElement{
		AttributeName: aws.String("extra_range"),
		KeyType:       ddbtypes.KeyTypeRange,
	})

	err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfg)
	if err == nil || !strings.Contains(err.Error(), "unexpected sort key") {
		t.Fatalf("expected unexpected sort key error, got: %v", err)
	}
}

func TestPreflightRejectsUnapprovedEnvironment(t *testing.T) {
	cfg := defaultPreflightConfig()
	ddbMock := valid20TablesMock(cfg.TablePrefix, cfg.AccountID, cfg.Region)
	s3Mock := &mockPreflightS3{status: s3types.BucketVersioningStatusEnabled}

	cfgBadRegion := cfg
	cfgBadRegion.Region = "us-east-1"
	if err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfgBadRegion); err == nil || !strings.Contains(err.Error(), "unapproved region") {
		t.Fatalf("expected unapproved region rejection, got: %v", err)
	}

	cfgBadPrefix := cfg
	cfgBadPrefix.TablePrefix = "runtime_test_"
	if err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfgBadPrefix); err == nil || !strings.Contains(err.Error(), "unapproved table prefix") {
		t.Fatalf("expected unapproved prefix rejection, got: %v", err)
	}

	cfgBadBucket := cfg
	cfgBadBucket.BucketName = "miles-dev-cold-unapproved"
	if err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfgBadBucket); err == nil || !strings.Contains(err.Error(), "unapproved bucket name") {
		t.Fatalf("expected unapproved bucket rejection, got: %v", err)
	}
}

func TestPreflightRejectsMissingOwnershipTags(t *testing.T) {
	cfg := defaultPreflightConfig()
	ddbMock := valid20TablesMock(cfg.TablePrefix, cfg.AccountID, cfg.Region)
	s3Mock := &mockPreflightS3{status: s3types.BucketVersioningStatusEnabled}

	// Remove tags from Chunks table
	chunksArn := "arn:aws:dynamodb:" + cfg.Region + ":" + cfg.AccountID + ":table/miles-dev-Chunks"
	ddbMock.tags[chunksArn] = nil

	err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfg)
	if err == nil || !strings.Contains(err.Error(), "missing or mismatched required tag") {
		t.Fatalf("expected missing required tag error, got: %v", err)
	}
}

func TestPreflightRejectsBucketVersioningDisabled(t *testing.T) {
	cfg := defaultPreflightConfig()
	ddbMock := valid20TablesMock(cfg.TablePrefix, cfg.AccountID, cfg.Region)
	s3Mock := &mockPreflightS3{status: s3types.BucketVersioningStatusSuspended}

	err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), ddbMock, s3Mock, cfg)
	if err == nil || !strings.Contains(err.Error(), "versioning is \"Suspended\"") {
		t.Fatalf("expected bucket versioning error, got: %v", err)
	}
}

func TestLivePreflightApprovedInventoryAndSchema(t *testing.T) {
	if os.Getenv("MILES_AWS_DEV_PREFLIGHT") != "1" {
		t.Skip("set MILES_AWS_DEV_PREFLIGHT=1 to run preflight check against live AWS")
	}
	cfg := defaultPreflightConfig()
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		t.Fatalf("cannot load AWS configuration: %v", err)
	}
	ddbClient := ddb.NewFromConfig(awsCfg)
	s3Client := s3.NewFromConfig(awsCfg)

	err = dynamodb.ValidateApprovedInventoryAndSchema(ctx, ddbClient, s3Client, cfg)
	if err != nil {
		t.Fatalf("live environment preflight rejected: %v", err)
	}
}
