package dynamodb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

var (
	accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)
	regionPattern    = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]$`)
	// Same bounds as the Terraform installation_name and environment variables.
	installationNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,22}[a-z0-9]$`)
	environmentPattern      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)

	// ErrPreflightStop is returned when identity, inventory, schema or tags drift.
	// A mismatch is an explicit stop condition: never adopt or overwrite.
	ErrPreflightStop = errors.New("preflight validation stopped: unapproved identity, inventory or schema mismatch")
)

// PreflightSTSAPI is the caller-identity check run before any inventory read.
type PreflightSTSAPI interface {
	GetCallerIdentity(ctx context.Context, params *sts.GetCallerIdentityInput, optFns ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
}

// PreflightDDBAPI abstracts DynamoDB inspection methods used by the preflight validator.
type PreflightDDBAPI interface {
	DescribeTable(ctx context.Context, params *ddb.DescribeTableInput, optFns ...func(*ddb.Options)) (*ddb.DescribeTableOutput, error)
	ListTagsOfResource(ctx context.Context, params *ddb.ListTagsOfResourceInput, optFns ...func(*ddb.Options)) (*ddb.ListTagsOfResourceOutput, error)
}

// PreflightS3API abstracts S3 inspection methods used by the preflight validator.
type PreflightS3API interface {
	GetBucketVersioning(ctx context.Context, params *s3.GetBucketVersioningInput, optFns ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
}

// PreflightInventory is the subset of the reviewed private installation JSON
// (infra/terraform/cloud/environments/dev/installation.example.json) that
// binds an acceptance run to one installation. Physical names derive from it
// exactly as Terraform does; there is no default installation.
type PreflightInventory struct {
	AccountID        string `json:"account_id"`
	Region           string `json:"region"`
	InstallationName string `json:"installation_name"`
	Environment      string `json:"environment"`
	Owner            string `json:"owner"`
	Operator         struct {
		DeployRoleARN string `json:"deploy_role_arn"`
	} `json:"operator"`
}

// LoadPreflightInventory reads and validates a private installation JSON.
func LoadPreflightInventory(path string) (PreflightInventory, error) {
	var inv PreflightInventory
	body, err := os.ReadFile(path)
	if err != nil {
		return inv, fmt.Errorf("%w: cannot read installation inventory: %v", ErrPreflightStop, err)
	}
	if err := json.Unmarshal(body, &inv); err != nil {
		return inv, fmt.Errorf("%w: invalid installation inventory JSON: %v", ErrPreflightStop, err)
	}
	return inv, inv.validate()
}

// TablePrefix is the Terraform table_prefix: "<installation>-".
func (i PreflightInventory) TablePrefix() string { return i.InstallationName + "-" }

// DataBucket is the Terraform data_bucket_name: "<installation>-data-<account>-<region>".
func (i PreflightInventory) DataBucket() string {
	return fmt.Sprintf("%s-data-%s-%s", i.InstallationName, i.AccountID, i.Region)
}

// RequiredTags are the provider default_tags every workload table carries.
func (i PreflightInventory) RequiredTags() map[string]string {
	return map[string]string{"Project": "Miles", "Installation": i.InstallationName, "Environment": i.Environment, "Owner": i.Owner}
}

func (i PreflightInventory) deployRoleName() string {
	return i.Operator.DeployRoleARN[strings.LastIndex(i.Operator.DeployRoleARN, "/")+1:]
}

func (i PreflightInventory) validate() error {
	switch {
	case !accountIDPattern.MatchString(i.AccountID):
		return fmt.Errorf("%w: invalid account_id %q", ErrPreflightStop, i.AccountID)
	case !regionPattern.MatchString(i.Region):
		return fmt.Errorf("%w: invalid region %q", ErrPreflightStop, i.Region)
	case !installationNamePattern.MatchString(i.InstallationName):
		return fmt.Errorf("%w: invalid installation_name %q", ErrPreflightStop, i.InstallationName)
	case !environmentPattern.MatchString(i.Environment) || strings.TrimSpace(i.Owner) == "":
		return fmt.Errorf("%w: environment and owner ownership tags are required", ErrPreflightStop)
	case !strings.HasPrefix(i.Operator.DeployRoleARN, "arn:aws:iam::"+i.AccountID+":role/") || i.deployRoleName() == "":
		return fmt.Errorf("%w: operator.deploy_role_arn %q is not a role in account %s", ErrPreflightStop, i.Operator.DeployRoleARN, i.AccountID)
	}
	return nil
}

// ValidateApprovedInventoryAndSchema verifies, before any acceptance request,
// that the caller is the installation's assumed deploy role and that the data
// bucket and all adapter tables match the reviewed inventory, schemas and
// ownership tags. It fails closed on any missing client or mismatch.
func ValidateApprovedInventoryAndSchema(ctx context.Context, stsAPI PreflightSTSAPI, ddbAPI PreflightDDBAPI, s3API PreflightS3API, inv PreflightInventory) error {
	if err := inv.validate(); err != nil {
		return err
	}
	if stsAPI == nil || ddbAPI == nil || s3API == nil {
		return fmt.Errorf("%w: sts, dynamodb and s3 clients are required", ErrPreflightStop)
	}
	ident, err := stsAPI.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return fmt.Errorf("%w: cannot verify caller identity: %v", ErrPreflightStop, err)
	}
	if aws.ToString(ident.Account) != inv.AccountID {
		return fmt.Errorf("%w: caller account %q, want %q", ErrPreflightStop, aws.ToString(ident.Account), inv.AccountID)
	}
	// STS omits the IAM path from assumed-role ARNs. Root, IAM users and other
	// roles (shared deploy or runtime roles) never match.
	rolePrefix := fmt.Sprintf("arn:aws:sts::%s:assumed-role/%s/", inv.AccountID, inv.deployRoleName())
	if arn := aws.ToString(ident.Arn); !strings.HasPrefix(arn, rolePrefix) || len(arn) == len(rolePrefix) {
		return fmt.Errorf("%w: caller %q is not an assumed session of the installation deploy role", ErrPreflightStop, arn)
	}

	bucket := inv.DataBucket()
	vOut, err := s3API.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &bucket, ExpectedBucketOwner: &inv.AccountID})
	if err != nil {
		return fmt.Errorf("%w: failed to read bucket versioning for %s: %v", ErrPreflightStop, bucket, err)
	}
	if vOut.Status != s3types.BucketVersioningStatusEnabled {
		return fmt.Errorf("%w: bucket %s versioning is %q, required %q", ErrPreflightStop, bucket, vOut.Status, s3types.BucketVersioningStatusEnabled)
	}

	for logicalName, spec := range Tables() {
		physicalName := inv.TablePrefix() + logicalName
		desc, err := ddbAPI.DescribeTable(ctx, &ddb.DescribeTableInput{TableName: &physicalName})
		if err != nil {
			return fmt.Errorf("%w: table %s describe failed: %v", ErrPreflightStop, physicalName, err)
		}
		t := desc.Table
		if t == nil {
			return fmt.Errorf("%w: table %s returned empty description", ErrPreflightStop, physicalName)
		}
		if t.TableStatus != ddbtypes.TableStatusActive {
			return fmt.Errorf("%w: table %s status is %q, required %q", ErrPreflightStop, physicalName, t.TableStatus, ddbtypes.TableStatusActive)
		}
		if aws.ToString(t.TableName) != physicalName {
			return fmt.Errorf("%w: table name mismatch: got %q, want %q", ErrPreflightStop, aws.ToString(t.TableName), physicalName)
		}
		if t.TableArn == nil {
			return fmt.Errorf("%w: table %s returned nil TableArn", ErrPreflightStop, physicalName)
		}
		expectedArn := fmt.Sprintf("arn:aws:dynamodb:%s:%s:table/%s", inv.Region, inv.AccountID, physicalName)
		if *t.TableArn != expectedArn {
			return fmt.Errorf("%w: table ARN mismatch for %s: got %q, want %q", ErrPreflightStop, physicalName, *t.TableArn, expectedArn)
		}
		attrTypes := make(map[string]ddbtypes.ScalarAttributeType, len(t.AttributeDefinitions))
		for _, attr := range t.AttributeDefinitions {
			if attr.AttributeName != nil {
				attrTypes[*attr.AttributeName] = attr.AttributeType
			}
		}

		var hashKey, rangeKey *string
		for _, key := range t.KeySchema {
			if key.KeyType == ddbtypes.KeyTypeHash {
				hashKey = key.AttributeName
			} else if key.KeyType == ddbtypes.KeyTypeRange {
				rangeKey = key.AttributeName
			}
		}

		if hashKey == nil || *hashKey != spec.Partition {
			return fmt.Errorf("%w: table %s partition key mismatch: got %q, want %q", ErrPreflightStop, physicalName, aws.ToString(hashKey), spec.Partition)
		}
		if attrTypes[*hashKey] != ddbtypes.ScalarAttributeTypeS {
			return fmt.Errorf("%w: table %s partition key %s type mismatch: got %q, want %q", ErrPreflightStop, physicalName, *hashKey, attrTypes[*hashKey], ddbtypes.ScalarAttributeTypeS)
		}

		if spec.Sort != "" {
			if rangeKey == nil || *rangeKey != spec.Sort {
				return fmt.Errorf("%w: table %s sort key mismatch: got %q, want %q", ErrPreflightStop, physicalName, aws.ToString(rangeKey), spec.Sort)
			}
			wantType := ddbtypes.ScalarAttributeType(spec.SortType)
			if attrTypes[*rangeKey] != wantType {
				return fmt.Errorf("%w: table %s sort key %s type mismatch: got %q, want %q", ErrPreflightStop, physicalName, *rangeKey, attrTypes[*rangeKey], wantType)
			}
		} else if rangeKey != nil {
			return fmt.Errorf("%w: table %s has unexpected sort key %q", ErrPreflightStop, physicalName, *rangeKey)
		}

		tagsOut, err := ddbAPI.ListTagsOfResource(ctx, &ddb.ListTagsOfResourceInput{ResourceArn: t.TableArn})
		if err != nil {
			return fmt.Errorf("%w: table %s list tags failed: %v", ErrPreflightStop, physicalName, err)
		}
		foundTags := make(map[string]string, len(tagsOut.Tags))
		for _, tag := range tagsOut.Tags {
			if tag.Key != nil && tag.Value != nil {
				foundTags[*tag.Key] = *tag.Value
			}
		}
		for reqKey, reqVal := range inv.RequiredTags() {
			if gotVal := foundTags[reqKey]; gotVal != reqVal {
				return fmt.Errorf("%w: table %s missing or mismatched required tag %s=%q (got %q)", ErrPreflightStop, physicalName, reqKey, reqVal, gotVal)
			}
		}
	}

	return nil
}
