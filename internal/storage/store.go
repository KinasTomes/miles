package storage

import (
	"context"
	"io"
)

// MetadataStore mutations must be linearizable with R1 admission/fence checks.
// CommitObject and DeleteObject atomically condition-check permission, bucket ID,
// head revision, reservation and completion state, then publish all R2 effects.
// ErrUnavailable is never proof that a commit did not happen: retry the SAME OpID
// or inspect its outbox/receipt before releasing reservations or deleting data.
type MetadataStore interface {
	GetBucket(ctx context.Context, name string) (Bucket, error)
	GetCredential(ctx context.Context, credentialID string) (Credential, error)
	ListAccessibleBuckets(ctx context.Context, request BucketListRequest) (BucketListPage, error)
	GetPolicy(ctx context.Context, revision uint64) (PolicySnapshot, error) // zero selects current.
	PutPolicy(ctx context.Context, request PolicyUpdate) (PolicySnapshot, error)
	ListObjects(ctx context.Context, request ListObjectsRequest) (ObjectListPage, error)
	UpdateAccessStats(ctx context.Context, updates []PackAccess) error
	ListPacksByState(ctx context.Context, request PackListRequest) (PackListPage, error)
	Authorize(ctx context.Context, credentialID, bucket string) (Authorization, error)
	GetChunks(ctx context.Context, keys []ChunkKey, consistent bool) (map[ChunkKey]Chunk, error)
	PutChunkIfAbsent(ctx context.Context, chunk Chunk, writer WriterFence) (Chunk, bool, error)
	CompareAndSwapChunkLocation(ctx context.Context, key ChunkKey, expectedRevision uint64, next ChunkLocation, writer WriterFence) (Chunk, error)
	GetPacks(ctx context.Context, refs []PackRef) (map[PackRef]PackInfo, error)
	// Pages are individually immutable/idempotent; a batch may partially succeed.
	// Retry/verify every page before publishing a head; this is NOT a transaction.
	WriteManifestPages(ctx context.Context, pages []ManifestPage, writer WriterFence) error
	GetManifest(ctx context.Context, manifestID string) ([]ManifestPage, error)
	GetObject(ctx context.Context, object ObjectKey) (ObjectHead, error) // includes tombstones for CAS.
	GetQuota(ctx context.Context, ownerID string) (LogicalQuota, error)
	ReserveLogicalQuota(ctx context.Context, reservation Reservation) error
	GetReservation(ctx context.Context, reservationID string) (Reservation, error)
	ClaimReservation(ctx context.Context, request ReservationClaim) (ReservationResolution, error)
	ReleaseLogicalQuota(ctx context.Context, request ReservationRelease) error
	CommitObject(ctx context.Context, request CommitRequest) (CommitResult, error)
	DeleteObject(ctx context.Context, request CommitRequest) (CommitResult, error) // tombstone, never physical delete.
	GetOutbox(ctx context.Context, eventID string) (OutboxEvent, error)
	GetCompletionReceipt(ctx context.Context, uploadID string) (CompletedReceipt, error)
}

// PackStore provides physical I/O, NOT authorization, GC safety or S3 fencing.
// Callers must hold R1 protection; class changes/deletes require durable
// Transitioning/Deleting metadata and reconciliation before reopening admission.
// Sidecars remain STANDARD regardless of the pack's class.
type PackStore interface {
	PutPack(ctx context.Context, info PackInfo, body io.Reader) error
	PutSidecar(ctx context.Context, ref PackRef, size int64, body io.Reader) error
	HeadPack(ctx context.Context, ref PackRef) (PackInfo, error)
	RangeGet(ctx context.Context, ref PackRef, byteRange ByteRange) (io.ReadCloser, error)
	ChangeStorageClass(ctx context.Context, ref PackRef, class StorageClass) error
	Restore(ctx context.Context, ref PackRef, request RestoreRequest) error // acceptance does not imply restored/readable.
	DeletePack(ctx context.Context, ref PackRef) error
}

// Cache is disposable physical data. The caller authorizes before EVERY Get;
// cache hits do not grant access. Returned/stored byte slices are owned copies.
type Cache interface {
	Get(ctx context.Context, key CacheKey) ([]byte, bool, error)
	Put(ctx context.Context, key CacheKey, data []byte) error
	Evict(ctx context.Context, ref PackRef) error
}
