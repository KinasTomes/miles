package observability

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
)

// DynamoDB requests capacity reporting without modifying the transactional plan,
// idempotency token, authority checks, per-call options or retry policy.
func (m *Runtime) DynamoDB(leaseTable string) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Initialize.Add(middleware.InitializeMiddlewareFunc("MilesDynamoDBMetrics", func(ctx context.Context, in middleware.InitializeInput, next middleware.InitializeHandler) (middleware.InitializeOutput, middleware.Metadata, error) {
			transactions, leases := 0, 0
			switch v := in.Parameters.(type) {
			case *ddb.GetItemInput:
				v.ReturnConsumedCapacity = types.ReturnConsumedCapacityTotal
			case *ddb.BatchGetItemInput:
				v.ReturnConsumedCapacity = types.ReturnConsumedCapacityTotal
			case *ddb.ScanInput:
				v.ReturnConsumedCapacity = types.ReturnConsumedCapacityTotal
			case *ddb.QueryInput:
				v.ReturnConsumedCapacity = types.ReturnConsumedCapacityTotal
			case *ddb.TransactGetItemsInput:
				v.ReturnConsumedCapacity = types.ReturnConsumedCapacityTotal
				transactions = 1
			case *ddb.TransactWriteItemsInput:
				// Frozen idempotent requests must keep their original parameters. New
				// adapter plans request capacity before persisting their exact input.
				if aws.ToString(v.ClientRequestToken) == "" {
					v.ReturnConsumedCapacity = types.ReturnConsumedCapacityTotal
				}
				transactions = 1
				for _, item := range v.TransactItems {
					if item.Put != nil && aws.ToString(item.Put.TableName) == leaseTable || item.Update != nil && aws.ToString(item.Update.TableName) == leaseTable || item.Delete != nil && aws.ToString(item.Delete.TableName) == leaseTable {
						leases++
					}
				}
			default:
				return next.HandleInitialize(ctx, in)
			}
			start := time.Now()
			out, meta, err := next.HandleInitialize(ctx, in)
			logTransactionConflict(ctx, in.Parameters, meta, err)
			m.add("miles_ddb_calls_total", 1)
			m.add("miles_ddb_transactions_total", float64(transactions))
			m.add("miles_ddb_lease_write_attempts_total", float64(leases))
			m.add("miles_ddb_call_seconds_total", time.Since(start).Seconds())
			var api smithy.APIError
			if errors.As(err, &api) {
				switch api.ErrorCode() {
				case "ThrottlingException", "ProvisionedThroughputExceededException", "RequestLimitExceeded":
					m.add("miles_ddb_throttles_total", 1)
				}
			}
			var canceled *types.TransactionCanceledException
			if errors.As(err, &canceled) {
				v := canceled
				for _, reason := range v.CancellationReasons {
					if aws.ToString(reason.Code) == "ThrottlingError" || aws.ToString(reason.Code) == "ProvisionedThroughputExceeded" {
						m.add("miles_ddb_throttles_total", 1)
						break
					}
				}
			}
			var capacities []types.ConsumedCapacity
			switch v := out.Result.(type) {
			case *ddb.GetItemOutput:
				if v != nil && v.ConsumedCapacity != nil {
					capacities = append(capacities, *v.ConsumedCapacity)
				}
			case *ddb.QueryOutput:
				if v != nil && v.ConsumedCapacity != nil {
					capacities = append(capacities, *v.ConsumedCapacity)
				}
			case *ddb.ScanOutput:
				if v != nil && v.ConsumedCapacity != nil {
					capacities = append(capacities, *v.ConsumedCapacity)
				}
			case *ddb.BatchGetItemOutput:
				if v != nil {
					capacities = v.ConsumedCapacity
				}
			case *ddb.TransactGetItemsOutput:
				if v != nil {
					capacities = v.ConsumedCapacity
				}
			case *ddb.TransactWriteItemsOutput:
				if v != nil {
					capacities = v.ConsumedCapacity
				}
			}
			if len(capacities) > 0 {
				m.add("miles_ddb_capacity_responses_total", 1)
			}
			for _, c := range capacities {
				m.add("miles_ddb_consumed_units_total", aws.ToFloat64(c.CapacityUnits))
				m.add("miles_ddb_read_units_total", aws.ToFloat64(c.ReadCapacityUnits))
				m.add("miles_ddb_write_units_total", aws.ToFloat64(c.WriteCapacityUnits))
			}
			return out, meta, err
		}), middleware.Before)
	}
}
