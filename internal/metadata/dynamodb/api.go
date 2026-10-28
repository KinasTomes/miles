// Package dynamodb implements native metadata CAS and atomic object publication.
// Production use requires a separately supplied R1/manifest authority provider;
// passing local contracts does not establish multi-node commit safety.
package dynamodb

import (
	"context"

	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// API is the SDK seam for the adapter. Conditional page publication uses
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
