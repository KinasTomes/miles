package coordination

import (
	"context"
	s "github.com/KinasTomes/miles/internal/storage"
)

// ReconcileMultipart consumes a proof directly from the trusted metadata port,
// never a caller-supplied result. It clears ONLY this old final commit unknown;
// uncertain page/pack/location operations still prevent Release. Recovery uses
// its own current authority while ClosedWriter binds the abandoned execution.
func (w *Writer) ReconcileMultipart(ctx context.Context, store s.MultipartRecovery, r s.MultipartRequest, attempt uint64) (s.MultipartRecoveryResult, error) {
	unknown := s.MultipartRecoveryResult{Disposition: s.RecoveryUnknown}
	if w == nil || w.state == nil || ctx == nil || store == nil || attempt == 0 || r.UploadID == "" {
		return unknown, s.ErrInvalid
	}
	w.owner.mu.Lock()
	valid := w.state.closed && w.state.inflight == 0 && w.state.work == 0 && w.state.operationID != "" && w.state.operationID == r.OpID
	w.owner.mu.Unlock()
	if !valid {
		return unknown, ErrWorkPending
	}
	out, err := store.ResolveCompletion(ctx, r)
	if err != nil {
		return unknown, err
	}
	w.owner.mu.Lock()
	defer w.owner.mu.Unlock()
	if !w.state.closed || w.state.inflight != 0 || w.state.work != 0 {
		return unknown, ErrWorkPending
	}
	u := out.Upload
	if out.Disposition != s.RecoveryClosed {
		return unknown, ErrReconcilePending
	}
	if u.ID != r.UploadID || u.Object != w.state.object || u.CompletionOp != w.state.operationID || u.ClosedWriter != w.Fence() || u.ClosedFence != attempt || u.State != "Aborting" && u.State != "Aborted" {
		return unknown, s.ErrConflict
	}
	delete(w.state.unknown, "commit/"+r.OpID)
	return out, nil
}
