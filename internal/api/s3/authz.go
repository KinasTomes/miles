package s3

import (
	"context"
	"errors"
	"fmt"

	"github.com/KinasTomes/miles/internal/storage"
)

// Access returns the authorization snapshot of API v2 2.2.3 for an access
// key on a bucket; auth.Snapshots implements it over
// storage.MetadataStore.Authorize. Like that port it reports a revoked
// credential or a disabled user as storage.ErrAccessDenied before it looks
// the bucket up, and a missing or deleting bucket as storage.ErrNotFound. The
// empty name is no bucket, so for ListBuckets only the user is checked.
type Access interface {
	Authorize(ctx context.Context, accessKeyID, bucket string) (storage.Authorization, error)
}

// allowed is the rule of op in API v2 2.2.3: owner has read and write,
// a grant gives read and write independently, and system admin gives no data
// right, only DeleteBucket, AbortMultipartUpload and ListParts. initiator
// tells whether the caller started the upload of a multipart operation.
// ListBuckets and CreateBucket have no bucket rule, and any other operation
// is denied.
func allowed(op Operation, a storage.Authorization, initiator bool) bool {
	owner := a.UserID == a.Bucket.OwnerID
	read, write := owner || a.Read, owner || a.Write
	switch op {
	case HeadBucket, GetBucketLocation, ListObjectsV2, GetObject, HeadObject, RestoreObject:
		return read
	case PutObject, DeleteObject, DeleteObjects, CopyObject, CreateMultipartUpload:
		return write
	case DeleteBucket:
		return owner || a.SystemAdmin
	case UploadPart, CompleteMultipartUpload:
		return write && initiator
	case AbortMultipartUpload:
		return write && initiator || owner || a.SystemAdmin
	case ListParts:
		return read && initiator || owner || a.SystemAdmin
	}
	return false
}

// authorize is steps 2 to 4 of the API v2 2.2.3 order, after the signature:
// disabled user (AccessDenied), bucket (NoSuchBucket; CreateBucket learns
// whether the name is free), then the rule of the operation without the
// initiator and the CopyObject source. It runs before the handler looks up
// any object, upload, source or cache entry, so a denied caller learns
// nothing about them.
func (rt *Router) authorize(ctx context.Context, route Route, caller Caller) (storage.Authorization, error) {
	a, err := rt.Access.Authorize(ctx, caller.AccessKeyID, route.Bucket)
	free := errors.Is(err, storage.ErrNotFound)
	switch {
	case errors.Is(err, storage.ErrAccessDenied):
		return storage.Authorization{}, &Error{Code: ErrAccessDenied}
	case free && (route.Op == ListBuckets || route.Op == CreateBucket):
		return storage.Authorization{}, nil
	case free:
		return storage.Authorization{}, &Error{Code: ErrNoSuchBucket, Bucket: route.Bucket}
	case err != nil:
		return storage.Authorization{}, fmt.Errorf("s3: authorize %s on bucket %q: %w", caller.AccessKeyID, route.Bucket, err)
	}
	switch route.Op {
	case ListBuckets:
		return storage.Authorization{}, nil
	case CreateBucket:
		if a.UserID == a.Bucket.OwnerID {
			return a, &Error{Code: ErrBucketAlreadyOwnedByYou, Bucket: route.Bucket}
		}
		return a, &Error{Code: ErrBucketAlreadyExists, Bucket: route.Bucket}
	}
	if !allowed(route.Op, a, true) {
		return a, &Error{Code: ErrAccessDenied}
	}
	return a, nil
}

// UploadBinding is what CreateMultipartUpload binds an upload_id to (API v2
// 2.2.3): the bucket incarnation, the key decoded once as Route.Key, and the
// user who started it.
type UploadBinding struct {
	BucketID, Key, Initiator string
}

// CheckUpload is step 6 of the API v2 2.2.3 order for UploadPart,
// CompleteMultipartUpload, AbortMultipartUpload and ListParts. u is the
// upload the uploadId query names, nil when there is none. An upload of
// another bucket incarnation or key is NoSuchUpload like an unknown one;
// then the rule of the operation with the initiator decides, so owner and
// system admin can list and abort but never upload or complete for the
// initiator. Handlers call it before they read the upload's state or
// receipt.
func (r *Request) CheckUpload(u *UploadBinding) *Error {
	if u == nil || u.BucketID != r.Access.Bucket.ID || u.Key != r.Key {
		return &Error{Code: ErrNoSuchUpload, Bucket: r.Bucket, Key: r.Key}
	}
	if !allowed(r.Op, r.Access, u.Initiator == r.Access.UserID) {
		return &Error{Code: ErrAccessDenied}
	}
	return nil
}

// AuthorizeCopySource is step 5 of the API v2 2.2.3 order for CopyObject:
// the caller's snapshot on the source bucket, which must exist
// (NoSuchBucket) and give read (AccessDenied). The router already checked
// write on the destination. The handler then looks the source key up
// (NoSuchKey) and pins it on this snapshot.
func (r *Request) AuthorizeCopySource(bucket string) (storage.Authorization, error) {
	a, err := r.access.Authorize(r.HTTP.Context(), r.Caller.AccessKeyID, bucket)
	switch {
	case errors.Is(err, storage.ErrNotFound):
		return a, &Error{Code: ErrNoSuchBucket, Bucket: bucket}
	case errors.Is(err, storage.ErrAccessDenied):
		return a, &Error{Code: ErrAccessDenied}
	case err != nil:
		return a, fmt.Errorf("s3: authorize %s on copy source bucket %q: %w", r.Caller.AccessKeyID, bucket, err)
	case !allowed(GetObject, a, false):
		return a, &Error{Code: ErrAccessDenied}
	}
	return a, nil
}
