package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	s3 "github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

var (
	accountIDPattern        = regexp.MustCompile(`^[0-9]{12}$`)
	installationNamePattern = regexp.MustCompile(`^miles-[a-z0-9][a-z0-9-]{0,62}$`)

	// ErrPreflightStop is returned when approved inventory, schema, or tags drift.
	// A schema mismatch is an explicit stop condition: never adopt or overwrite.
	ErrPreflightStop = errors.New("preflight validation stopped: unapproved inventory or schema mismatch")
)

// PreflightDDBAPI abstracts DynamoDB inspection methods used by the preflight validator.
type PreflightDDBAPI interface {
	DescribeTable(ctx context.Context, params *ddb.DescribeTableInput, optFns ...func(*ddb.Options)) (*ddb.DescribeTableOutput, error)
	ListTagsOfResource(ctx context.Context, params *ddb.ListTagsOfResourceInput, optFns ...func(*ddb.Options)) (*ddb.ListTagsOfResourceOutput, error)
}

// PreflightS3API abstracts S3 inspection methods used by the preflight validator.
type PreflightS3API interface {
	GetBucketVersioning(ctx context.Context, params *s3.GetBucketVersioningInput, optFns ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
}

// PreflightInventoryConfig specifies the exact expected environment parameters.
type PreflightInventoryConfig struct {
	AccountID        string
	Region           string
	InstallationName string
	TablePrefix      string
	BucketName       string
	RequiredTags     map[string]string
}

// ValidateApprovedInventoryAndSchema verifies that the live environment matches the exact
// reviewed Terraform inventory and all 20 adapter table schemas. Fails closed on any mismatch.
func ValidateApprovedInventoryAndSchema(ctx context.Context, ddbAPI PreflightDDBAPI, s3API PreflightS3API, cfg PreflightInventoryConfig) error {
	if !accountIDPattern.MatchString(cfg.AccountID) {
		return fmt.Errorf("%w: invalid or unapproved account ID %q", ErrPreflightStop, cfg.AccountID)
	}
	if cfg.Region != "ap-southeast-1" {
		return fmt.Errorf("%w: unapproved region %q; only ap-southeast-1 is permitted", ErrPreflightStop, cfg.Region)
	}
	installation := cfg.InstallationName
	if installation == "" {
		if strings.HasSuffix(cfg.TablePrefix, "-") {
			installation = strings.TrimSuffix(cfg.TablePrefix, "-")
		} else {
			installation = "miles-dev"
		}
	}
	if !installationNamePattern.MatchString(installation) {
		return fmt.Errorf("%w: invalid or unapproved installation name %q; must start with miles-", ErrPreflightStop, installation)
	}
	expectedPrefix := installation + "-"
	if cfg.TablePrefix != expectedPrefix {
		return fmt.Errorf("%w: unapproved table prefix %q; expected %q", ErrPreflightStop, cfg.TablePrefix, expectedPrefix)
	}
	expectedBucket := fmt.Sprintf("%s-data-%s-%s", installation, cfg.AccountID, cfg.Region)
	if cfg.BucketName != expectedBucket {
		return fmt.Errorf("%w: unapproved bucket name %q; expected %q", ErrPreflightStop, cfg.BucketName, expectedBucket)
	}

	if s3API != nil {
		vOut, err := s3API.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: &cfg.BucketName})
		if err != nil {
			return fmt.Errorf("%w: failed to read bucket versioning for %s: %v", ErrPreflightStop, cfg.BucketName, err)
		}
		if vOut.Status != s3types.BucketVersioningStatusEnabled {
			return fmt.Errorf("%w: bucket %s versioning is %q, required %q", ErrPreflightStop, cfg.BucketName, vOut.Status, s3types.BucketVersioningStatusEnabled)
		}
	}

	if ddbAPI == nil {
		return nil
	}

	tables := Tables()
	for logicalName, spec := range tables {
		physicalName := cfg.TablePrefix + logicalName
		desc, err := ddbAPI.DescribeTable(ctx, &ddb.DescribeTableInput{TableName: &physicalName})
		if err != nil {
			return fmt.Errorf("%w: table %s describe failed: %v", ErrPreflightStop, physicalName, err)
		}
		t := desc.Table
		if t == nil {
			return fmt.Errorf("%w: table %s returned empty description", ErrPreflightStop, physicalName)
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
			got := "<none>"
			if hashKey != nil {
				got = *hashKey
			}
			return fmt.Errorf("%w: table %s partition key mismatch: got %q, want %q", ErrPreflightStop, physicalName, got, spec.Partition)
		}
		if attrTypes[*hashKey] != ddbtypes.ScalarAttributeTypeS {
			return fmt.Errorf("%w: table %s partition key %s type mismatch: got %q, want %q", ErrPreflightStop, physicalName, *hashKey, attrTypes[*hashKey], ddbtypes.ScalarAttributeTypeS)
		}

		if spec.Sort != "" {
			if rangeKey == nil || *rangeKey != spec.Sort {
				got := "<none>"
				if rangeKey != nil {
					got = *rangeKey
				}
				return fmt.Errorf("%w: table %s sort key mismatch: got %q, want %q", ErrPreflightStop, physicalName, got, spec.Sort)
			}
			wantType := ddbtypes.ScalarAttributeType(spec.SortType)
			if attrTypes[*rangeKey] != wantType {
				return fmt.Errorf("%w: table %s sort key %s type mismatch: got %q, want %q", ErrPreflightStop, physicalName, *rangeKey, attrTypes[*rangeKey], wantType)
			}
		} else if rangeKey != nil {
			return fmt.Errorf("%w: table %s has unexpected sort key %q", ErrPreflightStop, physicalName, *rangeKey)
		}

		if len(cfg.RequiredTags) > 0 && t.TableArn != nil {
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
			for reqKey, reqVal := range cfg.RequiredTags {
				if gotVal, ok := foundTags[reqKey]; !ok || gotVal != reqVal {
					return fmt.Errorf("%w: table %s missing or mismatched required tag %s=%q (got %q)", ErrPreflightStop, physicalName, reqKey, reqVal, gotVal)
				}
			}
		}
	}

	return nil
}
