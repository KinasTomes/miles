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
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type mockPreflightSTS struct{ account, arn string }

func (m *mockPreflightSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	return &sts.GetCallerIdentityOutput{Account: aws.String(m.account), Arn: aws.String(m.arn)}, nil
}

type mockPreflightDDB struct {
	tables map[string]*ddbtypes.TableDescription
	tags   map[string][]ddbtypes.Tag
}

func (m *mockPreflightDDB) DescribeTable(_ context.Context, params *ddb.DescribeTableInput, _ ...func(*ddb.Options)) (*ddb.DescribeTableOutput, error) {
	t, ok := m.tables[aws.ToString(params.TableName)]
	if !ok {
		return nil, &ddbtypes.ResourceNotFoundException{Message: aws.String("table not found")}
	}
	return &ddb.DescribeTableOutput{Table: t}, nil
}

func (m *mockPreflightDDB) ListTagsOfResource(_ context.Context, params *ddb.ListTagsOfResourceInput, _ ...func(*ddb.Options)) (*ddb.ListTagsOfResourceOutput, error) {
	return &ddb.ListTagsOfResourceOutput{Tags: m.tags[aws.ToString(params.ResourceArn)]}, nil
}

type mockPreflightS3 struct {
	status        s3types.BucketVersioningStatus
	bucket, owner string
}

func (m *mockPreflightS3) GetBucketVersioning(_ context.Context, in *s3.GetBucketVersioningInput, _ ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	if aws.ToString(in.Bucket) != m.bucket || aws.ToString(in.ExpectedBucketOwner) != m.owner {
		return nil, errors.New("NoSuchBucket or AccessDenied")
	}
	return &s3.GetBucketVersioningOutput{Status: m.status}, nil
}

// testInventory mirrors the tracked installation.example.json values.
func testInventory(t *testing.T) dynamodb.PreflightInventory {
	t.Helper()
	inv, err := dynamodb.LoadPreflightInventory("../../../infra/terraform/cloud/environments/dev/installation.example.json")
	if err != nil {
		t.Fatalf("tracked installation example must load: %v", err)
	}
	return inv
}

type preflightFixture struct {
	inv dynamodb.PreflightInventory
	sts *mockPreflightSTS
	ddb *mockPreflightDDB
	s3  *mockPreflightS3
}

func newPreflightFixture(t *testing.T) *preflightFixture {
	inv := testInventory(t)
	f := &preflightFixture{
		inv: inv,
		sts: &mockPreflightSTS{account: inv.AccountID, arn: "arn:aws:sts::" + inv.AccountID + ":assumed-role/miles-member-deploy/member-deploy"},
		ddb: &mockPreflightDDB{tables: map[string]*ddbtypes.TableDescription{}, tags: map[string][]ddbtypes.Tag{}},
		s3:  &mockPreflightS3{status: s3types.BucketVersioningStatusEnabled, bucket: inv.DataBucket(), owner: inv.AccountID},
	}
	for logical, spec := range dynamodb.Tables() {
		physical := inv.TablePrefix() + logical
		arn := "arn:aws:dynamodb:" + inv.Region + ":" + inv.AccountID + ":table/" + physical
		attrs := []ddbtypes.AttributeDefinition{{AttributeName: aws.String(spec.Partition), AttributeType: ddbtypes.ScalarAttributeTypeS}}
		keys := []ddbtypes.KeySchemaElement{{AttributeName: aws.String(spec.Partition), KeyType: ddbtypes.KeyTypeHash}}
		if spec.Sort != "" {
			attrs = append(attrs, ddbtypes.AttributeDefinition{AttributeName: aws.String(spec.Sort), AttributeType: ddbtypes.ScalarAttributeType(spec.SortType)})
			keys = append(keys, ddbtypes.KeySchemaElement{AttributeName: aws.String(spec.Sort), KeyType: ddbtypes.KeyTypeRange})
		}
		f.ddb.tables[physical] = &ddbtypes.TableDescription{TableName: aws.String(physical), TableArn: aws.String(arn), TableStatus: ddbtypes.TableStatusActive, KeySchema: keys, AttributeDefinitions: attrs}
		for k, v := range inv.RequiredTags() {
			f.ddb.tags[arn] = append(f.ddb.tags[arn], ddbtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
	}
	return f
}

func (f *preflightFixture) table(logical string) *ddbtypes.TableDescription {
	return f.ddb.tables[f.inv.TablePrefix()+logical]
}

func (f *preflightFixture) run() error {
	return dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), f.sts, f.ddb, f.s3, f.inv)
}

func expectStop(t *testing.T, err error, want string) {
	t.Helper()
	if !errors.Is(err, dynamodb.ErrPreflightStop) || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected ErrPreflightStop containing %q, got: %v", want, err)
	}
}

func TestPreflightValidExactSchemaPasses(t *testing.T) {
	f := newPreflightFixture(t)
	if f.inv.TablePrefix() != "miles-member-test-" || f.inv.DataBucket() != "miles-member-test-data-123456789012-ap-southeast-1" {
		t.Fatalf("names must derive from the installation inventory: %s %s", f.inv.TablePrefix(), f.inv.DataBucket())
	}
	if err := f.run(); err != nil {
		t.Fatalf("expected valid preflight to pass, got: %v", err)
	}
}

func TestPreflightRequiresExplicitInventory(t *testing.T) {
	f := newPreflightFixture(t)
	for name, mutate := range map[string]func(*dynamodb.PreflightInventory){
		"no installation": func(i *dynamodb.PreflightInventory) { i.InstallationName = "" },
		"no account":      func(i *dynamodb.PreflightInventory) { i.AccountID = "" },
		"no region":       func(i *dynamodb.PreflightInventory) { i.Region = "" },
		"no environment":  func(i *dynamodb.PreflightInventory) { i.Environment = "" },
		"no owner":        func(i *dynamodb.PreflightInventory) { i.Owner = " " },
		"no deploy role":  func(i *dynamodb.PreflightInventory) { i.Operator.DeployRoleARN = "" },
		"foreign deploy role": func(i *dynamodb.PreflightInventory) {
			i.Operator.DeployRoleARN = "arn:aws:iam::000000000000:role/miles-member-deploy"
		},
		"role path only": func(i *dynamodb.PreflightInventory) {
			i.Operator.DeployRoleARN = "arn:aws:iam::123456789012:role/miles/"
		},
		"uppercase install":    func(i *dynamodb.PreflightInventory) { i.InstallationName = "Miles-Member" },
		"overlong install":     func(i *dynamodb.PreflightInventory) { i.InstallationName = "miles-member-installation-x" },
		"injected prefix char": func(i *dynamodb.PreflightInventory) { i.InstallationName = "miles_dev" },
	} {
		inv := f.inv
		mutate(&inv)
		err := dynamodb.ValidateApprovedInventoryAndSchema(context.Background(), f.sts, f.ddb, f.s3, inv)
		if !errors.Is(err, dynamodb.ErrPreflightStop) {
			t.Errorf("%s: expected stop, got %v", name, err)
		}
	}
	if _, err := dynamodb.LoadPreflightInventory("does-not-exist.json"); !errors.Is(err, dynamodb.ErrPreflightStop) {
		t.Fatalf("missing inventory file must stop, got %v", err)
	}
}

func TestPreflightRejectsNilClients(t *testing.T) {
	f := newPreflightFixture(t)
	ctx := context.Background()
	for name, err := range map[string]error{
		"sts": dynamodb.ValidateApprovedInventoryAndSchema(ctx, nil, f.ddb, f.s3, f.inv),
		"ddb": dynamodb.ValidateApprovedInventoryAndSchema(ctx, f.sts, nil, f.s3, f.inv),
		"s3":  dynamodb.ValidateApprovedInventoryAndSchema(ctx, f.sts, f.ddb, nil, f.inv),
	} {
		if !errors.Is(err, dynamodb.ErrPreflightStop) || !strings.Contains(err.Error(), "clients are required") {
			t.Errorf("nil %s client: expected stop, got %v", name, err)
		}
	}
}

func TestPreflightRejectsWrongCallerIdentity(t *testing.T) {
	for name, caller := range map[string]mockPreflightSTS{
		"other account": {"000000000000", "arn:aws:sts::000000000000:assumed-role/miles-member-deploy/s"},
		"root":          {"123456789012", "arn:aws:iam::123456789012:root"},
		"iam user":      {"123456789012", "arn:aws:iam::123456789012:user/miles/operators/member-deploy"},
		"shared deploy": {"123456789012", "arn:aws:sts::123456789012:assumed-role/miles-dev-deploy/s"},
		"runtime role":  {"123456789012", "arn:aws:sts::123456789012:assumed-role/miles-member-test-jobs/i-0"},
		"role prefix":   {"123456789012", "arn:aws:sts::123456789012:assumed-role/miles-member-deploy-extra/s"},
		"no session":    {"123456789012", "arn:aws:sts::123456789012:assumed-role/miles-member-deploy/"},
	} {
		f := newPreflightFixture(t)
		f.sts = &caller
		if err := f.run(); !errors.Is(err, dynamodb.ErrPreflightStop) {
			t.Errorf("%s: expected stop, got %v", name, err)
		}
	}
}

func TestPreflightRejectsMalformedPartitionKey(t *testing.T) {
	f := newPreflightFixture(t)
	// A Buckets table created with partition key "id" instead of "bucket".
	b := f.table("Buckets")
	b.KeySchema = []ddbtypes.KeySchemaElement{{AttributeName: aws.String("id"), KeyType: ddbtypes.KeyTypeHash}}
	b.AttributeDefinitions = []ddbtypes.AttributeDefinition{{AttributeName: aws.String("id"), AttributeType: ddbtypes.ScalarAttributeTypeS}}
	expectStop(t, f.run(), "miles-member-test-Buckets partition key mismatch")
}

func TestPreflightRejectsMissingSortKey(t *testing.T) {
	f := newPreflightFixture(t)
	f.table("Objects").KeySchema = []ddbtypes.KeySchemaElement{{AttributeName: aws.String("bucket"), KeyType: ddbtypes.KeyTypeHash}}
	expectStop(t, f.run(), "miles-member-test-Objects sort key mismatch")
}

func TestPreflightRejectsWrongSortType(t *testing.T) {
	f := newPreflightFixture(t)
	desc := f.table("ManifestPages")
	for i, attr := range desc.AttributeDefinitions {
		if aws.ToString(attr.AttributeName) == "page_no" {
			desc.AttributeDefinitions[i].AttributeType = ddbtypes.ScalarAttributeTypeS
		}
	}
	expectStop(t, f.run(), "type mismatch")
}

func TestPreflightRejectsUnexpectedSortKey(t *testing.T) {
	f := newPreflightFixture(t)
	users := f.table("Users")
	users.KeySchema = append(users.KeySchema, ddbtypes.KeySchemaElement{AttributeName: aws.String("extra_range"), KeyType: ddbtypes.KeyTypeRange})
	expectStop(t, f.run(), "unexpected sort key")
}

func TestPreflightRejectsForeignOrMissingOwnershipTags(t *testing.T) {
	for _, tag := range []string{"Project", "Installation", "Environment", "Owner"} {
		f := newPreflightFixture(t)
		arn := aws.ToString(f.table("Chunks").TableArn)
		for i, v := range f.ddb.tags[arn] {
			if aws.ToString(v.Key) == tag {
				f.ddb.tags[arn][i].Value = aws.String("miles-dev")
			}
		}
		expectStop(t, f.run(), "mismatched required tag "+tag)
	}
	f := newPreflightFixture(t)
	f.ddb.tags[aws.ToString(f.table("Chunks").TableArn)] = nil
	expectStop(t, f.run(), "missing or mismatched required tag")
}

func TestPreflightRejectsBucketDrift(t *testing.T) {
	f := newPreflightFixture(t)
	f.s3.status = s3types.BucketVersioningStatusSuspended
	expectStop(t, f.run(), `versioning is "Suspended"`)

	f = newPreflightFixture(t)
	f.s3.owner = "000000000000" // bucket name exists but belongs to another account
	expectStop(t, f.run(), "failed to read bucket versioning")
}

func TestPreflightRejectsNonActiveTable(t *testing.T) {
	f := newPreflightFixture(t)
	f.table("Users").TableStatus = ddbtypes.TableStatusCreating
	expectStop(t, f.run(), `status is "CREATING"`)
}

func TestPreflightRejectsMismatchedTableIdentity(t *testing.T) {
	f := newPreflightFixture(t)
	f.table("Users").TableArn = aws.String("arn:aws:dynamodb:ap-southeast-1:000000000000:table/miles-member-test-Users")
	expectStop(t, f.run(), "table ARN mismatch")

	f.table("Users").TableArn = nil
	expectStop(t, f.run(), "returned nil TableArn")

	f = newPreflightFixture(t)
	f.table("Users").TableName = aws.String("miles-dev-Users")
	expectStop(t, f.run(), "table name mismatch")

	f = newPreflightFixture(t)
	delete(f.ddb.tables, f.inv.TablePrefix()+"Users")
	expectStop(t, f.run(), "describe failed")
}

// Opt-in, read-only check against the operator's own private installation.
// AWS_PROFILE must resolve to that installation's assumed deploy role.
func TestLivePreflightApprovedInventoryAndSchema(t *testing.T) {
	path := os.Getenv("MILES_INSTALLATION_JSON")
	if path == "" {
		t.Skip("set MILES_INSTALLATION_JSON to the private installation JSON to run preflight against live AWS")
	}
	inv, err := dynamodb.LoadPreflightInventory(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(inv.Region))
	if err != nil {
		t.Fatalf("cannot load AWS configuration: %v", err)
	}
	if err := dynamodb.ValidateApprovedInventoryAndSchema(ctx, sts.NewFromConfig(awsCfg), ddb.NewFromConfig(awsCfg), s3.NewFromConfig(awsCfg), inv); err != nil {
		t.Fatalf("live environment preflight rejected: %v", err)
	}
}
