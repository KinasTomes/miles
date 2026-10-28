// Package dynamodb prepares schema and bounded SDK requests for the W4 adapter.
// It does not implement storage.MetadataStore or execute distributed commits.
package dynamodb

import (
	"context"

	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// API is the SDK seam for the future adapter. Conditional page publication uses
// TransactWriteItems with its owner/fence conditions, never blind BatchWriteItem.
type API interface {
	GetItem(context.Context, *ddb.GetItemInput, ...func(*ddb.Options)) (*ddb.GetItemOutput, error)
	BatchGetItem(context.Context, *ddb.BatchGetItemInput, ...func(*ddb.Options)) (*ddb.BatchGetItemOutput, error)
	Query(context.Context, *ddb.QueryInput, ...func(*ddb.Options)) (*ddb.QueryOutput, error)
	Scan(context.Context, *ddb.ScanInput, ...func(*ddb.Options)) (*ddb.ScanOutput, error)
	TransactGetItems(context.Context, *ddb.TransactGetItemsInput, ...func(*ddb.Options)) (*ddb.TransactGetItemsOutput, error)
	TransactWriteItems(context.Context, *ddb.TransactWriteItemsInput, ...func(*ddb.Options)) (*ddb.TransactWriteItemsOutput, error)
}

var _ API = (*ddb.Client)(nil)
