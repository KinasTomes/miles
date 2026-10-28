package readpath

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestWarmSelectRangeRunsUnderBoundReadView(t *testing.T) {
	f := newWarmFixture(t, 0)
	selected := false
	q := f.request
	q.SelectRange = func(head storage.ObjectHead) (*storage.ByteRange, error) {
		selected = true
		if head.Size != int64(len(f.body)) || head.Revision != 1 {
			t.Fatal("wrong bound head", head)
		}
		if !errors.Is(f.reader.options.Gate.Retire(f.ref), storage.ErrConflict) {
			t.Fatal("selection has no resolving protection")
		}
		return &storage.ByteRange{Offset: head.Size - 7, Length: 7}, nil
	}
	result, got, err := f.read(context.Background(), q)
	if err != nil || !selected || !bytes.Equal(got, f.body[len(f.body)-7:]) || result.Range.Length != 7 {
		t.Fatal("selection did not control actual read", result, string(got), err)
	}
	drained(t, f.reader.options.Admission)
}
func TestWarmSelectRangeErrorStopsBeforePayload(t *testing.T) {
	f := newWarmFixture(t, 0)
	q := f.request
	q.SelectRange = func(storage.ObjectHead) (*storage.ByteRange, error) { return nil, storage.ErrAccessDenied }
	q.Ready = func(WarmResult) error { t.Error("Ready called after rejection"); return nil }
	_, body, err := f.read(context.Background(), q)
	if !errors.Is(err, storage.ErrAccessDenied) || len(body) != 0 || f.backend.gets != 0 {
		t.Fatal("selection failure accessed payload", len(body), f.backend.gets, err)
	}
	drained(t, f.reader.options.Admission)
	if err := f.reader.options.Gate.Retire(f.ref); err != nil {
		t.Fatal("rejected read leaked protection", err)
	}
}
