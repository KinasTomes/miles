package readpath

import (
	"context"
	"errors"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

func TestWarmReaderRetainsCancellationCauseFromTransport(t *testing.T) {
	for _, cause := range []error{storage.ErrClockUncertain, storage.ErrFenced, storage.ErrUnavailable, context.Canceled} {
		t.Run(cause.Error(), func(t *testing.T) {
			f := newWarmFixture(t, 0)
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			f.backend.beforeRange = func(call context.Context, _ *awss3.GetObjectInput) error {
				cancel(cause)
				<-call.Done()
				return call.Err() // SDK cancellation, without its original cause
			}
			_, body, err := f.read(ctx, f.request)
			if !errors.Is(err, cause) || len(body) != 0 {
				t.Fatal("cancellation lost its cause or emitted unverified bytes", err, len(body))
			}
		})
	}
}

type canceledHeadLookup struct {
	storage.MetadataStore
	cancel context.CancelCauseFunc
	cause  error
}

func (m canceledHeadLookup) GetObject(ctx context.Context, _ storage.ObjectKey) (storage.ObjectHead, error) {
	m.cancel(m.cause)
	<-ctx.Done()
	return storage.ObjectHead{}, ctx.Err()
}

func TestWarmReaderRetainsCauseBeforeStreaming(t *testing.T) {
	f := newWarmFixture(t, 0)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	f.reader.options.Metadata = canceledHeadLookup{f.metadata, cancel, storage.ErrClockUncertain}
	_, body, err := f.read(ctx, f.request)
	if !errors.Is(err, storage.ErrClockUncertain) || !errors.Is(err, context.Canceled) || len(body) != 0 {
		t.Fatal("head lookup lost renewal cancellation cause", err)
	}
}
