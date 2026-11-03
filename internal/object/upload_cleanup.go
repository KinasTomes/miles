package object

import (
	"context"
	"errors"

	"github.com/KinasTomes/miles/internal/storage"
)

// PurgeExpiredUpload is a trusted worker entry point. The recovery transaction
// rechecks expiry and fences publishers before releasing staging accounting.
// A Completing upload is recovered from its frozen input by RecoverUpload.
func (s *Service) PurgeExpiredUpload(ctx context.Context, id string) (result storage.MultipartUpload, err error) {
	if ctx == nil || s.o.Uploads == nil {
		return result, storage.ErrInvalid
	}
	u, err := s.o.Uploads.GetUpload(ctx, id)
	if err != nil || u.State == "Aborted" || u.State == "Completed" {
		return u, err
	}
	if u.State != "Aborting" && (u.State != "Active" || s.o.Clock.Now().WallUTC.Before(u.ExpiresAt)) {
		return u, storage.ErrConflict
	}
	ctx, session, _, fence, _, err := s.begin(ctx, u.Object, u.Credential, "")
	if err != nil {
		return u, err
	}
	defer func() { err = errors.Join(err, session.Close()) }()
	if err = session.BindRoot(u.ID); err != nil {
		return u, err
	}
	for range maxPartAttempts {
		req := storage.MultipartRequest{UploadID: u.ID, Credential: u.Credential, OpID: u.CompletionOp, ExpectedRevision: u.Revision, Fence: u.Fence, Writer: fence}
		out, resolveErr := s.o.Uploads.ResolveCompletion(ctx, req)
		if resolveErr == nil {
			if out.Disposition == storage.RecoveryUnknown {
				return u, storage.ErrUnavailable
			}
			u = out.Upload
			if u.State == "Aborted" || u.State == "Completed" {
				return u, nil
			}
			if u.State != "Aborting" {
				return u, storage.ErrConflict
			}
			if err = s.closePublishers(ctx, req); err != nil {
				return u, err
			}
		} else if !errors.Is(resolveErr, storage.ErrConflict) {
			return u, resolveErr
		}
		u, err = s.o.Uploads.GetUpload(ctx, id)
		if err != nil {
			return u, err
		}
	}
	return u, storage.ErrConflict
}
