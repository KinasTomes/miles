package observability

import (
	"context"
	"errors"
	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go/middleware"
)

type transactionConflict struct {
	Index  int    `json:"index"`
	Table  string `json:"table"`
	Reason string `json:"reason"`
}

// Log before the adapter collapses native cancellation reasons into ErrConflict.
// Never retain cancellation messages/items, transaction keys or request tokens.
func logTransactionConflict(ctx context.Context, input any, meta middleware.Metadata, err error) {
	request, ok := input.(*ddb.TransactWriteItemsInput)
	var canceled *types.TransactionCanceledException
	if !ok || !errors.As(err, &canceled) {
		return
	}
	var conflicts []transactionConflict
	for i, reason := range canceled.CancellationReasons {
		code := aws.ToString(reason.Code)
		if (code != "ConditionalCheckFailed" && code != "TransactionConflict") || i >= len(request.TransactItems) {
			continue
		}
		action := request.TransactItems[i]
		var table *string
		switch {
		case action.Put != nil:
			table = action.Put.TableName
		case action.Update != nil:
			table = action.Update.TableName
		case action.Delete != nil:
			table = action.Delete.TableName
		case action.ConditionCheck != nil:
			table = action.ConditionCheck.TableName
		}
		conflicts = append(conflicts, transactionConflict{Index: i, Table: aws.ToString(table), Reason: code})
	}
	if len(conflicts) == 0 {
		return
	}
	id, _ := ctx.Value(requestIDKey{}).(string)
	awsID, _ := awsmiddleware.GetRequestIDMetadata(meta)
	slog.WarnContext(ctx, "dynamodb transaction conflict", "request_id", id,
		"aws_request_id", awsID, "conflicts", conflicts)
}
