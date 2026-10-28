package coordination

import (
	"context"
	"errors"
	s "github.com/KinasTomes/miles/internal/storage"
	"testing"
)

type multipartProofReader struct {
	result s.MultipartRecoveryResult
	err    error
}

func (p *multipartProofReader) ResolveCompletion(context.Context, s.MultipartRequest) (s.MultipartRecoveryResult, error) {
	return p.result, p.err
}
func (p *multipartProofReader) ResolvePartPublisher(context.Context, s.MultipartRequest, string) (s.MultipartRecoveryResult, error) {
	return p.result, p.err
}

func TestMultipartProofReleasesOnlyMatchingQuiescentCommit(t *testing.T) {
	ctx := context.Background()
	c, g, registry, object, pack := r1Fixture(t)
	w := registerWriter(t, c, registry)
	work, err := w.BeginOperation(ctx, object, "op")
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Pin(ctx, pack); err != nil {
		t.Fatal(err)
	}
	if err = w.Do(ctx, "commit/op", func(s.WriterFence) error { return s.ErrUnavailable }); !errors.Is(err, s.ErrUnavailable) {
		t.Fatal(err)
	}
	w.CloseFence()
	r := s.MultipartRequest{UploadID: "upload", OpID: "op"}
	proof := &multipartProofReader{result: s.MultipartRecoveryResult{Disposition: s.RecoveryClosed, Upload: s.MultipartUpload{ID: r.UploadID, Object: object, CompletionOp: "op", ClosedFence: 3, ClosedWriter: w.Fence(), State: "Aborting"}}}
	if _, err = w.ReconcileMultipart(ctx, proof, r, 3); !errors.Is(err, ErrWorkPending) {
		t.Fatal("proof bypassed live work", err)
	}
	work.Finish()
	proof.result.Disposition = s.RecoveryUnknown
	if _, err = w.ReconcileMultipart(ctx, proof, r, 3); !errors.Is(err, ErrReconcilePending) {
		t.Fatal(err)
	}
	if !errors.Is(w.Release(), ErrReconcilePending) || !errors.Is(g.Retire(pack), s.ErrConflict) {
		t.Fatal("unknown released pins")
	}
	proof.result.Disposition = s.RecoveryClosed
	for _, change := range []func(*s.MultipartUpload){func(u *s.MultipartUpload) { u.Object.Key = "other" }, func(u *s.MultipartUpload) { u.ClosedFence++ }, func(u *s.MultipartUpload) { u.ClosedWriter.Epoch++ }, func(u *s.MultipartUpload) { u.State = "Active" }} {
		original := proof.result.Upload
		change(&proof.result.Upload)
		if _, err = w.ReconcileMultipart(ctx, proof, r, 3); !errors.Is(err, s.ErrConflict) {
			t.Fatal("wrong proof accepted", err)
		}
		proof.result.Upload = original
	}
	if _, err = w.ReconcileMultipart(ctx, proof, r, 3); err != nil {
		t.Fatal(err)
	}
	if err = w.Release(); err != nil {
		t.Fatal(err)
	}
	if err = g.Retire(pack); err != nil {
		t.Fatal("proved quiescent operation retained pins", err)
	}
}
