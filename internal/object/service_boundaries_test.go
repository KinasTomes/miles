package object

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/KinasTomes/miles/internal/storage"
)

func TestServiceServesSuccessiveWrites(t *testing.T) {
	f := newServiceFixture(t)
	for _, key := range []string{"first", "second"} {
		data := []byte("hello")
		_, err := f.service.PutObject(context.Background(), PutRequest{Bucket: f.bucket, Key: key, Body: body(data), ContentLength: int64(len(data)), UserID: "owner", CredentialID: "owner-key"})
		if err != nil {
			t.Fatalf("shared ObjectWriter rejects request %q: %v", key, err)
		}
	}
}

type eventCapture struct{ event CommitEvent }

func (c *eventCapture) ObjectCommit(ctx context.Context, e CommitEvent) (storage.OutboxEvent, error) {
	c.event = e
	return (fixtureEvents{}).ObjectCommit(ctx, e)
}
func TestEventReceivesPreparedManifestDescriptor(t *testing.T) {
	f := newServiceFixture(t)
	capture := &eventCapture{}
	opts := f.options
	opts.Events = capture
	svc, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("hello")
	_, err = svc.PutObject(context.Background(), PutRequest{Bucket: f.bucket, Key: "file", Body: body(data), ContentLength: int64(len(data)), UserID: "owner", CredentialID: "owner-key"})
	if err != nil {
		t.Fatal(err)
	}
	published, err := f.metadata.GetObject(context.Background(), storage.ObjectKey{Bucket: f.bucket.Name, BucketID: f.bucket.ID, Key: "file"})
	if err != nil {
		t.Fatal(err)
	}
	if capture.event.Head.ValidateManifestDescriptor() != nil || capture.event.Head.PageCount != published.PageCount || capture.event.Head.ChunkCount != published.ChunkCount || !reflect.DeepEqual(capture.event.Head.PageIndex, published.PageIndex) || capture.event.Revision != published.Revision {
		t.Fatalf("event head omits prepared descriptor: event pages/chunks=%d/%d, published=%d/%d", capture.event.Head.PageCount, capture.event.Head.ChunkCount, published.PageCount, published.ChunkCount)
	}
}

func TestServiceServesConcurrentWrites(t *testing.T) {
	f := newServiceFixture(t)
	start := make(chan struct{})
	done := make(chan error, 2)
	for _, key := range []string{"first", "second"} {
		go func() {
			<-start
			data := []byte(key)
			_, err := f.service.PutObject(context.Background(), PutRequest{Bucket: f.bucket, Key: key, Body: body(data), ContentLength: int64(len(data)), UserID: "owner", CredentialID: "owner-key"})
			done <- err
		}()
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"first", "second"} {
		_, got := f.get(t, context.Background(), key, nil)
		if !bytes.Equal(got, []byte(key)) {
			t.Fatal("wrong concurrent object", key, string(got))
		}
	}
}

func TestServiceRejectsInsufficientWriterProtection(t *testing.T) {
	f := newServiceFixture(t)
	opts := f.options
	admit := opts.AdmitWriter
	opts.AdmitWriter = func(ctx context.Context, r WriterRequest) (AdmittedWriter, error) {
		w, err := admit(ctx, r)
		w.ProtectedUntil = r.WallDeadline.Add(15*time.Minute - time.Nanosecond)
		return w, err
	}
	svc, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.PutObject(context.Background(), PutRequest{Bucket: f.bucket, Key: "file", Body: body(nil), ContentLength: 0, UserID: "owner", CredentialID: "owner-key"})
	if !errors.Is(err, storage.ErrFenced) {
		t.Fatal("insufficient protection admitted", err)
	}
	q, err := f.metadata.GetQuota(context.Background(), "owner")
	if err != nil || q.Used != 0 || q.Reserved != 0 || q.Staged != 0 {
		t.Fatal("invalid authority changed quota", q, err)
	}
}

func TestServiceDeadlineCannotExtendConfiguredBudget(t *testing.T) {
	f := newServiceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Hour)
	defer cancel()
	before := f.options.Clock.Now()
	end, wallEnd, err := f.service.deadline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := end.Sub(before.Instant)
	if err != nil || budget != f.options.RequestTimeout || !wallEnd.Equal(before.WallUTC.Add(budget)) {
		t.Fatal("request extended configured deadline", budget, wallEnd, err)
	}
}
