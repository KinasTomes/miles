package dynamodb

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	s "github.com/KinasTomes/miles/internal/storage"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	ddb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/smithy-go"
)

func retryableRead(err error) bool {
	var service smithy.APIError
	if !errors.As(err, &service) {
		return false
	}
	switch service.ErrorCode() {
	case "ProvisionedThroughputExceededException", "ThrottlingException", "RequestLimitExceeded", "InternalServerError", "TransactionConflictException":
		return true
	}
	return false
}

// Discovery must keep successful physical pages when a later Scan is throttled.
// Use service-scale backoff rather than transaction-CAS millisecond delays;
// both the attempt limit and the caller's shutdown/authority budget still apply.
func (m *Store) scanOutboxPage(ctx context.Context, input *ddb.ScanInput) (*ddb.ScanOutput, error) {
	backoff := retry.NewExponentialJitterBackoff(2 * time.Second)
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out, err := m.api.Scan(ctx, input)
		if err == nil || !retryableRead(err) || attempt+1 >= max(1, m.maxAttempts) {
			return out, err
		}
		delay, err := backoff.BackoffDelay(attempt+1, err)
		if err != nil {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		if m.onRetry != nil {
			m.onRetry()
		}
	}
}
func (m *Store) readItem(ctx context.Context, input *ddb.GetItemInput) (*ddb.GetItemOutput, error) {
	for attempt := 0; attempt < m.maxAttempts; attempt++ {
		out, err := m.api.GetItem(ctx, input)
		if err == nil || !retryableRead(err) || attempt+1 == m.maxAttempts {
			return out, err
		}
		if err := m.retryPause(ctx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, s.ErrUnavailable
}
func retryPause(ctx context.Context, attempt int) error {
	ceiling := time.Duration(1<<min(attempt, 6)) * 5 * time.Millisecond
	var jitter [1]byte
	if _, err := rand.Read(jitter[:]); err != nil {
		return err
	}
	delay := ceiling/2 + time.Duration(jitter[0])*ceiling/512
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (m *Store) retryPause(ctx context.Context, attempt int) error {
	if err := retryPause(ctx, attempt); err != nil {
		return err
	}
	if m.onRetry != nil {
		m.onRetry()
	}
	return nil
}
