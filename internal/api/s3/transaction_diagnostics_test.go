package s3

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KinasTomes/miles/internal/object"
	"github.com/KinasTomes/miles/internal/observability"
	"github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go/middleware"
)

type conflictingObjectWriter struct {
	fakeObjectWriter
	client *ddb.Client
}

func (w *conflictingObjectWriter) PutObject(ctx context.Context, _ object.PutRequest) (object.PutResult, error) {
	_, err := w.client.TransactWriteItems(ctx, &ddb.TransactWriteItemsInput{
		ClientRequestToken: aws.String("private-token"),
		TransactItems: []types.TransactWriteItem{{ConditionCheck: &types.ConditionCheck{
			TableName:           aws.String("fixture-SystemConfig"),
			Key:                 map[string]types.AttributeValue{"id": &types.AttributeValueMemberS{Value: "private-key"}},
			ConditionExpression: aws.String("attribute_exists(id)"),
		}}},
	})
	if err == nil {
		return object.PutResult{}, nil
	}
	return object.PutResult{}, &object.PutStageError{Stage: "prepare", Err: storage.ErrConflict}
}

func TestTransactionConflictCorrelatesWithS3WithoutLeakingBackendData(t *testing.T) {
	for _, reason := range []string{"ConditionalCheckFailed", "TransactionConflict"} {
		t.Run(reason, func(t *testing.T) {
			calls := 0
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/x-amz-json-1.0")
				w.Header().Set("x-amzn-RequestId", "backend-request-123")
				w.WriteHeader(400)
				io.WriteString(w, `{"__type":"TransactionCanceledException","Message":"private-backend-message","CancellationReasons":[{"Code":"`+reason+`","Message":"private-reason","Item":{"id":{"S":"private-key"}}}]}`)
			}))
			defer backend.Close()
			metrics := new(observability.Runtime)
			client := ddb.New(ddb.Options{Region: "ap-southeast-1", BaseEndpoint: aws.String(backend.URL),
				Retryer: aws.NopRetryer{}, HTTPClient: backend.Client(),
				Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: "fixture", SecretAccessKey: "fixture-secret"}, nil
				}), APIOptions: []func(*middleware.Stack) error{metrics.DynamoDB("fixture-Leases")}})
			world := newBucketWorld(t)
			world.router.Handlers = NewObjectHandlers(&conflictingObjectWriter{client: client}, nil, nil, nil)
			var logs bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
			defer slog.SetDefault(old)
			request := httptest.NewRequest(http.MethodPut, "/demo/private-object-name", strings.NewReader("body"))
			request.Header.Set("X-Amz-Request-Id", "untrusted-request-id")
			sign(request, accessKey("owner"), "secret-owner")
			response := httptest.NewRecorder()
			world.router.ServeHTTP(response, request)
			if response.Code != http.StatusConflict || checkErrorBody(t, response) != ErrOperationAborted || calls != 1 {
				t.Fatal("response or retry behavior changed", response.Code, calls)
			}
			var native, api map[string]any
			decoder := json.NewDecoder(&logs)
			if err := decoder.Decode(&native); err != nil {
				t.Fatal("missing native conflict log", err)
			}
			if err := decoder.Decode(&api); err != nil {
				t.Fatal("missing S3 conflict log", err)
			}
			id := response.Header().Get("X-Amz-Request-Id")
			if native["request_id"] != id || api["request_id"] != id || native["aws_request_id"] != "backend-request-123" || api["stage"] != "prepare" {
				t.Fatal("request correlation lost", native, api)
			}
			conflict := native["conflicts"].([]any)[0].(map[string]any)
			if conflict["index"] != float64(0) || conflict["table"] != "fixture-SystemConfig" || conflict["reason"] != reason {
				t.Fatal("wrong transaction conflict", conflict)
			}
			for _, secret := range []string{"private-key", "private-token", "private-backend-message", "private-reason", "private-object-name", "secret-owner", "fixture-secret", "untrusted-request-id"} {
				if strings.Contains(logs.String(), secret) || secret != "private-object-name" && strings.Contains(response.Body.String(), secret) {
					t.Fatal("diagnostics leaked private input", secret)
				}
			}
		})
	}
}
