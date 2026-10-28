package dynamodb

import (
	"context"
	"errors"
	"testing"

	s "github.com/KinasTomes/miles/internal/storage"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/smithy-go"
)

type throttledRead struct {
	API
	calls int
	code  string
}

func (a *throttledRead) GetItem(context.Context, *ddb.GetItemInput, ...func(*ddb.Options)) (*ddb.GetItemOutput, error) {
	a.calls++
	if a.calls == 1 || a.code == "ValidationException" {
		return nil, &smithy.GenericAPIError{Code: a.code}
	}
	return &ddb.GetItemOutput{}, nil
}

func TestReadRetryDistinguishesThrottleValidationAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		code  string
		calls int
		fail  bool
	}{
		{"ProvisionedThroughputExceededException", 2, false},
		{"ValidationException", 1, true},
	} {
		t.Run(tc.code, func(t *testing.T) {
			api := &throttledRead{code: tc.code}
			m := &Store{api: api, maxAttempts: 3}
			_, err := m.readItem(context.Background(), &ddb.GetItemInput{})
			if (err != nil) != tc.fail || api.calls != tc.calls {
				t.Fatal(api.calls, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := retryPause(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := backend(context.Canceled); !errors.Is(err, context.Canceled) || !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
}
