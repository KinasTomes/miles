package readpath

import (
	"context"
	"sync"

	"github.com/KinasTomes/miles/internal/clock"
	"github.com/KinasTomes/miles/internal/storage"
)

// BindSource is the metadata-only read view of a CopyObject source: like Read
// it acquires the view before the head lookup and binds the committed head, so
// retirement admission stays closed while the caller resolves that head's
// references. It reads no payload and takes no read budget. The caller hands
// the references to its own generation pins, then calls done: done fails with
// ErrFenced if the view was lost meanwhile, and always releases the view.
// Authorization of the source is the caller's.
func (r *WarmReader) BindSource(ctx context.Context, object storage.ObjectKey, deadline clock.Instant) (storage.ObjectHead, func(context.Context) error, error) {
	if r == nil || ctx == nil || object.Validate() != nil {
		return storage.ObjectHead{}, nil, storage.ErrInvalid
	}
	o := r.options
	view, err := o.Gate.Acquire(ctx, object, deadline)
	if err != nil {
		return storage.ObjectHead{}, nil, err
	}
	work, err := view.BeginWork(ctx)
	if err != nil {
		view.Release()
		return storage.ObjectHead{}, nil, err
	}
	_, stop, err := view.KeepAlive(ctx)
	if err != nil {
		work.Finish()
		view.Release()
		return storage.ObjectHead{}, nil, err
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			stop()
			work.Finish()
			view.Release()
		})
	}
	done := func(ctx context.Context) error {
		err := view.Check(ctx)
		release()
		return err
	}
	head, err := o.Metadata.GetObject(ctx, object)
	if err == nil && head.Deleted {
		err = storage.ErrNotFound
	}
	if err == nil {
		err = view.Bind(ctx, head)
	}
	if err != nil {
		release()
		return storage.ObjectHead{}, nil, err
	}
	return head, done, nil
}
