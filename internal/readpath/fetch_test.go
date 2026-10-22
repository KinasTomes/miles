package readpath_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"sync/atomic"
	"testing"

	"github.com/KinasTomes/miles/internal/readpath"
	"github.com/KinasTomes/miles/internal/storage"
)

type fetchStore struct {
	storage.PackStore
	body  io.ReadCloser
	err   error
	calls int
}

func (s *fetchStore) RangeGet(context.Context, storage.PackRef, storage.ByteRange) (io.ReadCloser, error) {
	s.calls++
	return s.body, s.err
}

type fetchBody struct {
	read     func([]byte) (int, error)
	closeErr error
	closes   atomic.Int32
}

func (b *fetchBody) Read(p []byte) (int, error) { return b.read(p) }
func (b *fetchBody) Close() error               { b.closes.Add(1); return b.closeErr }

func TestFetchRangeSharedFake(t *testing.T) {
	ref := storage.PackRef{ID: "fixture-pack", Generation: "fixture-gen"}
	span := storage.ByteRange{Offset: 2, Length: 3}
	for _, tc := range []struct {
		mode string
		want error
	}{
		{"", nil}, {"short", readpath.ErrShortRange}, {"oversized", readpath.ErrOversizedRange},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s := packIOFixture(t)
			s.mode = tc.mode
			data, err := readpath.FetchRange(context.Background(), s, ref, span, 3)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if tc.want != nil && data != nil {
				t.Fatal("partial payload", data)
			}
			if tc.want == nil {
				if string(data) != "cde" || cap(data) != 3 {
					t.Fatal(data, cap(data))
				}
				data[0] = '!'
				again, err := readpath.FetchRange(context.Background(), s, ref, span, 3)
				if err != nil || string(again) != "cde" {
					t.Fatal("aliased backend", again, err)
				}
			}
			calls := s.snapshot()
			wantCalls := 1
			if tc.want == nil {
				wantCalls = 2
			}
			if len(calls) != wantCalls {
				t.Fatal(calls)
			}
			for _, call := range calls {
				if call != (rangeCall{ref, span}) {
					t.Fatal(call)
				}
			}
		})
	}
	t.Run("missing generation", func(t *testing.T) {
		s := packIOFixture(t)
		ref.Generation = "missing"
		data, err := readpath.FetchRange(context.Background(), s, ref, span, 3)
		if data != nil || !errors.Is(err, storage.ErrNotFound) || len(s.snapshot()) != 1 {
			t.Fatal(data, err)
		}
	})
}

func TestFetchRangePreflight(t *testing.T) {
	ref := storage.PackRef{ID: "p", Generation: "g"}
	for _, tc := range []struct {
		name  string
		ref   storage.PackRef
		span  storage.ByteRange
		limit int64
	}{
		{"empty ref", storage.PackRef{}, storage.ByteRange{Length: 1}, 1},
		{"empty generation", storage.PackRef{ID: "p"}, storage.ByteRange{Length: 1}, 1},
		{"empty range", ref, storage.ByteRange{}, 1},
		{"negative offset", ref, storage.ByteRange{Offset: -1, Length: 1}, 1},
		{"negative length", ref, storage.ByteRange{Length: -1}, 1},
		{"overflow", ref, storage.ByteRange{Offset: math.MaxInt64, Length: 1}, 1},
		{"length overflow", ref, storage.ByteRange{Offset: 1, Length: math.MaxInt64}, math.MaxInt64},
		{"exceeds cap", ref, storage.ByteRange{Length: 4}, 3},
		{"zero cap", ref, storage.ByteRange{Length: 1}, 0},
		{"negative cap", ref, storage.ByteRange{Length: 1}, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &fetchStore{}
			data, err := readpath.FetchRange(context.Background(), s, tc.ref, tc.span, tc.limit)
			if data != nil || !errors.Is(err, storage.ErrInvalid) || s.calls != 0 {
				t.Fatal(data, err, s.calls)
			}
		})
	}
	s := &fetchStore{}
	if _, err := readpath.FetchRange(nil, s, ref, storage.ByteRange{Length: 1}, 1); !errors.Is(err, storage.ErrInvalid) || s.calls != 0 {
		t.Fatal(err)
	}
	if _, err := readpath.FetchRange(context.Background(), nil, ref, storage.ByteRange{Length: 1}, 1); !errors.Is(err, storage.ErrInvalid) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data, err := readpath.FetchRange(ctx, s, ref, storage.ByteRange{Length: 1}, 1)
	if data != nil || !errors.Is(err, context.Canceled) || s.calls != 0 {
		t.Fatal(data, err)
	}
}

func TestFetchRangeStreamFailuresAndClose(t *testing.T) {
	injected := errors.New("injected read/backend failure")
	closeErr := errors.New("injected close failure")
	for _, tc := range []struct {
		name                                string
		content                             string
		readErr, backendErr, closeErr, want error
	}{
		{"exact", "abc", nil, nil, nil, nil},
		{"empty", "", nil, nil, nil, readpath.ErrShortRange},
		{"short", "ab", nil, nil, nil, readpath.ErrShortRange},
		{"oversized", "abcd", nil, nil, nil, readpath.ErrOversizedRange},
		{"mid read error", "a", injected, nil, nil, injected},
		{"error with full bytes", "abc", injected, nil, nil, injected},
		{"backend with body", "abc", nil, injected, nil, injected},
		{"close failure", "abc", nil, nil, closeErr, closeErr},
		{"read and close failure", "a", injected, nil, closeErr, injected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := bytes.NewReader([]byte(tc.content))
			reads := 0
			body := &fetchBody{closeErr: tc.closeErr, read: func(p []byte) (int, error) {
				reads++
				n, err := r.Read(p)
				if tc.readErr != nil {
					err = tc.readErr
				}
				return n, err
			}}
			s := &fetchStore{body: body, err: tc.backendErr}
			data, err := readpath.FetchRange(context.Background(), s, storage.PackRef{ID: "p", Generation: "g"}, storage.ByteRange{Length: 3}, 3)
			if !errors.Is(err, tc.want) || body.closes.Load() != 1 || s.calls != 1 {
				t.Fatal(err, body.closes.Load(), s.calls)
			}
			if tc.closeErr != nil && !errors.Is(err, tc.closeErr) {
				t.Fatal("lost close error", err)
			}
			if tc.want != nil && data != nil {
				t.Fatal("partial bytes", data)
			}
			if tc.want == nil && string(data) != "abc" {
				t.Fatal(data)
			}
			if tc.backendErr != nil && reads != 0 {
				t.Fatal("read after backend error")
			}
		})
	}
	for _, backendErr := range []error{nil, injected} {
		s := &fetchStore{err: backendErr}
		data, err := readpath.FetchRange(context.Background(), s, storage.PackRef{ID: "p", Generation: "g"}, storage.ByteRange{Length: 1}, 1)
		want := backendErr
		if want == nil {
			want = storage.ErrInvalid
		}
		if data != nil || !errors.Is(err, want) {
			t.Fatal(data, err)
		}
	}
}

func TestFetchRangeProbeAndReaderBoundaries(t *testing.T) {
	injected := errors.New("probe failure")
	for _, tc := range []struct {
		name           string
		probeN         int
		probeErr, want error
	}{
		{"EOF", 0, io.EOF, nil}, {"extra", 1, io.EOF, readpath.ErrOversizedRange},
		{"probe error", 0, injected, injected}, {"no progress", 0, nil, io.ErrNoProgress},
		{"invalid count", 2, nil, storage.ErrInvalid}, {"negative count", -1, nil, storage.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			body := &fetchBody{read: func(p []byte) (int, error) {
				calls++
				if calls == 1 {
					if len(p) != 3 {
						t.Fatal(len(p))
					}
					copy(p, "abc")
					return 3, io.EOF
				}
				if len(p) != 1 {
					t.Fatal("probe must be one byte", len(p))
				}
				return tc.probeN, tc.probeErr
			}}
			data, err := readpath.FetchRange(context.Background(), &fetchStore{body: body}, storage.PackRef{ID: "p", Generation: "g"}, storage.ByteRange{Offset: math.MaxInt64 - 3, Length: 3}, 3)
			if !errors.Is(err, tc.want) || calls != 2 || body.closes.Load() != 1 {
				t.Fatal(err, calls, body.closes.Load())
			}
			if tc.want != nil && data != nil {
				t.Fatal(data)
			}
			if tc.want == nil && string(data) != "abc" {
				t.Fatal(data)
			}
		})
	}
}

func TestFetchRangeCancellationRetainsOwnershipUntilReadReturns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered, unblock := make(chan struct{}), make(chan struct{})
	body := &fetchBody{read: func(p []byte) (int, error) { close(entered); <-unblock; p[0] = 'a'; return 1, nil }}
	done := make(chan error, 1)
	go func() {
		data, err := readpath.FetchRange(ctx, &fetchStore{body: body}, storage.PackRef{ID: "p", Generation: "g"}, storage.ByteRange{Length: 2}, 2)
		if data != nil {
			err = errors.New("partial output on cancellation")
		}
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		t.Fatalf("returned before quiescence: %v", err)
	default:
	}
	if body.closes.Load() != 0 {
		t.Fatal("closed while Read active")
	}
	close(unblock)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if body.closes.Load() != 1 {
		t.Fatal(body.closes.Load())
	}
}
