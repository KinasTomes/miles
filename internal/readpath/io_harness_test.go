package readpath_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
	"github.com/KinasTomes/miles/internal/storage/fake"
)

type rangeCall struct {
	ref  storage.PackRef
	span storage.ByteRange
}

// Fault wrapper delegates normal physical semantics to the shared fake. It is
// not R1 admission, a bounded production reader, or an alternate backend port.
type faultPackStore struct {
	storage.PackStore
	mu          sync.Mutex
	calls       []rangeCall
	mode        string
	backendErr  error
	entered     chan struct{}
	enteredOnce sync.Once
}

func (s *faultPackStore) RangeGet(ctx context.Context, ref storage.PackRef, span storage.ByteRange) (io.ReadCloser, error) {
	s.mu.Lock()
	s.calls = append(s.calls, rangeCall{ref, span})
	s.mu.Unlock()
	if s.entered != nil {
		s.enteredOnce.Do(func() { close(s.entered) })
	}
	if s.mode == "wait cancellation" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.backendErr != nil {
		return nil, s.backendErr
	}
	r, err := s.PackStore.RangeGet(ctx, ref, span)
	if err != nil {
		return nil, err
	}
	if s.mode == "" {
		return r, nil
	}
	// This harness contains an eight-byte fake pack; refuse larger fixture
	// responses rather than turning fault injection into an unbounded read.
	data, err := io.ReadAll(io.LimitReader(r, 9))
	closeErr := r.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(data) > 8 {
		return nil, storage.ErrInvalid
	}
	switch s.mode {
	case "short":
		data = data[:len(data)-1]
	case "oversized":
		data = append(data, '!')
	default:
		return nil, storage.ErrInvalid
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}
func (s *faultPackStore) snapshot() []rangeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]rangeCall(nil), s.calls...)
}
func packIOFixture(t *testing.T) *faultPackStore {
	t.Helper()
	store := fake.NewPacks(func() time.Time { return time.Unix(0, 0).UTC() })
	info := storage.PackInfo{Version: storage.ModelVersion, Ref: storage.PackRef{ID: "fixture-pack", Generation: "fixture-gen"}, Size: 8, State: "Warm", Class: storage.Standard}
	if err := store.PutPack(context.Background(), info, bytes.NewBufferString("abcdefgh")); err != nil {
		t.Fatal(err)
	}
	return &faultPackStore{PackStore: store}
}
func TestBoundedIOFaultHarnessSelfCheck(t *testing.T) {
	ref := storage.PackRef{ID: "fixture-pack", Generation: "fixture-gen"}
	span := storage.ByteRange{Offset: 2, Length: 3}
	t.Run("multiple calls recorded", func(t *testing.T) {
		s := packIOFixture(t)
		s.entered = make(chan struct{})
		for i := 0; i < 2; i++ {
			r, err := s.RangeGet(context.Background(), ref, span)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
		}
		calls := s.snapshot()
		if len(calls) != 2 || calls[0] != (rangeCall{ref, span}) || calls[1] != calls[0] {
			t.Fatal(calls)
		}
	})
	for _, tc := range []struct{ mode, want string }{{"", "cde"}, {"short", "cd"}, {"oversized", "cde!"}} {
		t.Run(tc.mode, func(t *testing.T) {
			s := packIOFixture(t)
			s.mode = tc.mode
			r, err := s.RangeGet(context.Background(), ref, span)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(r)
			closeErr := r.Close()
			if err != nil || closeErr != nil || string(body) != tc.want {
				t.Fatal(string(body), err, closeErr)
			}
			calls := s.snapshot()
			if len(calls) != 1 || calls[0] != (rangeCall{ref, span}) {
				t.Fatal(calls)
			}
		})
	}
	t.Run("backend error", func(t *testing.T) {
		s := packIOFixture(t)
		injected := errors.New("injected backend unavailable")
		s.backendErr = injected
		r, err := s.RangeGet(context.Background(), ref, span)
		if r != nil || !errors.Is(err, injected) || len(s.snapshot()) != 1 {
			t.Fatal(r, err)
		}
	})
	t.Run("unknown generation", func(t *testing.T) {
		s := packIOFixture(t)
		wrong := ref
		wrong.Generation = "missing"
		r, err := s.RangeGet(context.Background(), wrong, span)
		if r != nil || !errors.Is(err, storage.ErrNotFound) {
			t.Fatal(r, err)
		}
	})
	t.Run("cancellation at I/O barrier", func(t *testing.T) {
		s := packIOFixture(t)
		s.mode = "wait cancellation"
		s.entered = make(chan struct{})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			r, err := s.RangeGet(ctx, ref, span)
			if r != nil {
				_ = r.Close()
				done <- errors.New("unexpected reader")
				return
			}
			done <- err
		}()
		<-s.entered
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || len(s.snapshot()) != 1 {
			t.Fatal(err)
		}
	})
}
